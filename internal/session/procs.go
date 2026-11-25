package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcessGrace is how long processes left in a worktree get to exit after SIGTERM before SIGKILL; tests shorten it.
var ProcessGrace = 5 * time.Second

// stopProcessesIn ends every process whose working directory is dir or below it: what the agent started and that
// outlived it outside its process group (`nohup … &`, setsid, a dev server that daemonizes), which the stop of the
// group never reaches and which would keep running in a deleted folder. SIGTERM first, SIGKILL after ProcessGrace.
// Returns how many it found.
func stopProcessesIn(ctx context.Context, dir string) int {
	pids := processesIn(ctx, dir)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(ProcessGrace)
	for len(alive(pids)) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range alive(pids) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return len(pids)
}

func alive(pids []int) []int {
	var out []int
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			out = append(out, pid)
		}
	}
	return out
}

// processesIn returns the processes (other than this one) whose working directory is dir or below it: from /proc on
// Linux, from lsof elsewhere (macOS). Best effort: an unreadable process is skipped.
func processesIn(ctx context.Context, dir string) []int {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	inside := func(cwd string) bool {
		cwd = filepath.Clean(strings.TrimSuffix(cwd, " (deleted)"))
		return cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator))
	}
	self := os.Getpid()
	seen := map[int]bool{}
	var out []int
	add := func(pid int, cwd string) {
		if pid > 0 && pid != self && !seen[pid] && inside(cwd) {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	if runtime.GOOS == "linux" {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd")); err == nil {
				add(pid, cwd)
			}
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// -F pn: a "p<pid>" line per process, then "n<path>" for its cwd. lsof exits 1 when some process could not be
	// read, with the rest of the list still printed.
	b, _ := exec.CommandContext(ctx, "lsof", "-n", "-P", "-w", "-d", "cwd", "-F", "pn").Output()
	pid := 0
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n"):
			add(pid, line[1:])
		}
	}
	return out
}
