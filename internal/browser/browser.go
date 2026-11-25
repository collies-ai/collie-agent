// Package browser opens a URL in the person's browser when the machine has one.
package browser

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// ErrNoBrowser means there is no graphical session to open a browser in (e.g. an SSH session on a server).
var ErrNoBrowser = errors.New("no browser available")

// Open starts the browser on url without waiting for it.
func Open(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		if os.Getenv("SSH_CONNECTION") != "" {
			return ErrNoBrowser
		}
		cmd = exec.Command("open", url)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return ErrNoBrowser
		}
		path, err := exec.LookPath("xdg-open")
		if err != nil {
			return ErrNoBrowser
		}
		cmd = exec.Command(path, url)
	default:
		return ErrNoBrowser
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
