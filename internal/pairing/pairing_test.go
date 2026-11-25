package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeCollie answers /oauth/device_authorization and then plays tokenAnswers in order on /oauth/token.
type fakeCollie struct {
	t            *testing.T
	interval     int
	expiresIn    int
	tokenAnswers []answer

	mu         sync.Mutex
	authForm   map[string]string
	tokenPolls int
	tokenForms []map[string]string
}

type answer struct {
	status int
	body   any
}

func pending() answer  { return answer{400, map[string]string{"error": "authorization_pending"}} }
func slowDown() answer { return answer{400, map[string]string{"error": "slow_down"}} }

func (f *fakeCollie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	if r.Method != http.MethodPost {
		f.t.Errorf("%s %s: want POST", r.Method, r.URL.Path)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		f.t.Errorf("Content-Type = %q", ct)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Fatal(err)
	}
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/oauth/device_authorization":
		f.authForm = form
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev-123", "user_code": "BCDF-GHJK",
			"verification_uri": "https://collie.test/device", "verification_uri_complete": "https://collie.test/device?user_code=BCDF-GHJK",
			"expires_in": f.expiresIn, "interval": f.interval,
		})
	case "/oauth/token":
		f.tokenForms = append(f.tokenForms, form)
		if f.tokenPolls >= len(f.tokenAnswers) {
			f.t.Errorf("unexpected poll #%d", f.tokenPolls+1)
			w.WriteHeader(500)
			return
		}
		a := f.tokenAnswers[f.tokenPolls]
		f.tokenPolls++
		w.WriteHeader(a.status)
		_ = json.NewEncoder(w).Encode(a.body)
	default:
		http.NotFound(w, r)
	}
}

// run authorizes and polls against the fake with a fake clock; it returns the result, the error and every wait.
func run(t *testing.T, f *fakeCollie) (*Result, error, []time.Duration) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	var waits []time.Duration
	c := NewClient(srv.URL)
	c.Now = func() time.Time { return now }
	c.Sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		now = now.Add(d)
		return nil
	}
	da, err := c.Authorize(context.Background(), Machine{Name: "laptop", OS: "linux", Hostname: "laptop.local"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if da.UserCode != "BCDF-GHJK" || da.VerificationURI != "https://collie.test/device" {
		t.Fatalf("unexpected device authorization %+v", da)
	}
	res, err := c.Poll(context.Background(), da)
	return res, err, waits
}

func TestAuthorizeSendsTheMachine(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{{200, map[string]any{"machine_id": 42, "machine_token": "tok"}}}}
	if _, err, _ := run(t, f); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"client_id": "collie-agent", "name": "laptop", "os": "linux", "hostname": "laptop.local"}
	for k, v := range want {
		if f.authForm[k] != v {
			t.Errorf("device_authorization %s = %q, want %q", k, f.authForm[k], v)
		}
	}
	tf := f.tokenForms[0]
	if tf["grant_type"] != DeviceCodeGrant || tf["device_code"] != "dev-123" || tf["client_id"] != ClientID {
		t.Errorf("token form = %v", tf)
	}
}

func TestPollApprovedAfterPending(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{
		pending(), pending(),
		{200, map[string]any{"machine_id": 42, "machine_token": "secret-token", "token_type": "Bearer"}},
	}}
	res, err, waits := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if res.MachineID != "42" || res.MachineToken != "secret-token" {
		t.Errorf("result = %+v", res)
	}
	assertWaits(t, waits, 5, 5, 5)
}

func TestPollSlowDownAddsFiveSeconds(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{
		pending(), slowDown(), pending(), slowDown(),
		{200, map[string]any{"machine_id": "7", "machine_token": "t"}},
	}}
	res, err, waits := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if res.MachineID != "7" {
		t.Errorf("machine id = %q", res.MachineID)
	}
	assertWaits(t, waits, 5, 5, 10, 10, 15)
}

func TestPollDenied(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{pending(), {400, map[string]string{"error": "access_denied"}}}}
	_, err, _ := run(t, f)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
}

func TestPollExpiredByServer(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{pending(), {400, map[string]string{"error": "expired_token"}}}}
	_, err, _ := run(t, f)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestPollGivesUpWhenTheCodeExpiresLocally(t *testing.T) {
	// 12 seconds and an interval of 5: two polls fit, the third would land after expiry.
	f := &fakeCollie{interval: 5, expiresIn: 12, tokenAnswers: []answer{pending(), pending()}}
	_, err, waits := run(t, f)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if f.tokenPolls != 2 {
		t.Errorf("polled %d times, want 2", f.tokenPolls)
	}
	assertWaits(t, waits, 5, 5)
}

func TestPollRetriesServerErrors(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{
		{502, map[string]string{}},
		{200, map[string]any{"machine_id": 1, "machine_token": "t"}},
	}}
	if _, err, _ := run(t, f); err != nil {
		t.Fatal(err)
	}
}

func TestPollUnknownErrorStops(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{{400, map[string]string{"error": "invalid_grant", "error_description": "used"}}}}
	_, err, _ := run(t, f)
	if err == nil || errors.Is(err, ErrDenied) || errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want a generic pairing error", err)
	}
}

func TestPollStopsWhenCancelled(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Poll(ctx, &DeviceAuthorization{DeviceCode: "d", Interval: 1, ExpiresIn: 60})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAuthorizeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"client_id must be collie-agent"}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL).Authorize(context.Background(), Machine{Name: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
}

func assertWaits(t *testing.T, got []time.Duration, wantSeconds ...int) {
	t.Helper()
	if len(got) != len(wantSeconds) {
		t.Fatalf("waits = %v, want %v seconds", got, wantSeconds)
	}
	for i, s := range wantSeconds {
		if got[i] != time.Duration(s)*time.Second {
			t.Errorf("wait %d = %s, want %ds", i, got[i], s)
		}
	}
}

func TestPollBacksOffAfterServerErrorsKeepingSlowDown(t *testing.T) {
	f := &fakeCollie{interval: 5, expiresIn: 600, tokenAnswers: []answer{
		slowDown(),                 // next wait 10
		{503, map[string]string{}}, // next wait 20
		{502, map[string]string{}}, // next wait 40
		pending(),
		{200, map[string]any{"machine_id": 1, "machine_token": "t"}},
	}}
	if _, err, waits := run(t, f); err != nil {
		t.Fatal(err)
	} else {
		assertWaits(t, waits, 5, 10, 20, 40, 40)
	}
}

func TestPollBacksOffAfterTimeouts(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			// Drop the connection without an answer, as a timeout or a reset would.
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"machine_id": 3, "machine_token": "t"}`))
	}))
	defer srv.Close()
	var waits []time.Duration
	now := time.Now()
	c := NewClient(srv.URL)
	c.Now = func() time.Time { return now }
	c.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); now = now.Add(d); return nil }
	res, err := c.Poll(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 600})
	if err != nil || res.MachineID != "3" {
		t.Fatalf("Poll = %+v, %v", res, err)
	}
	assertWaits(t, waits, 5, 10, 20)
}

func TestBackOffIsCapped(t *testing.T) {
	if got := backOff(45 * time.Second); got != maxInterval {
		t.Errorf("backOff(45s) = %s", got)
	}
}

func TestValidateVerificationURI(t *testing.T) {
	for _, ok := range []string{"https://collies.sh/device", "https://collies.sh/device?user_code=BCDF-GHJK", "http://localhost:3000/device", "http://127.0.0.1/device"} {
		if err := ValidateVerificationURI(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "--help", "/etc/passwd", "file:///etc/passwd", "collie.test/device", "javascript:alert(1)",
		"vscode://x", "http://collies.sh/device", "https://user:pw@collies.sh/device", "https:///device"} {
		if ValidateVerificationURI(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAuthorizeRejectsUnsafeVerificationURIs(t *testing.T) {
	for _, field := range []string{"verification_uri", "verification_uri_complete"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := map[string]any{"device_code": "d", "user_code": "U", "verification_uri": "https://collie.test/device",
				"verification_uri_complete": "https://collie.test/device?user_code=U", "expires_in": 600, "interval": 5}
			body[field] = "file:///Applications/Calculator.app"
			_ = json.NewEncoder(w).Encode(body)
		}))
		_, err := NewClient(srv.URL).Authorize(context.Background(), Machine{Name: "x"})
		srv.Close()
		if err == nil {
			t.Errorf("%s: accepted a file URL", field)
		}
	}
}
