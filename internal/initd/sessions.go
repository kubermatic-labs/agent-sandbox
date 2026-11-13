package initd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// CountSessions counts the session processes in procDir: every process
// other than PID 1, zombies, and those exclude reports (the proxy's tree,
// milestone 7). A process that vanishes mid-scan is not counted.
func CountSessions(procDir string, exclude func(pid int) bool) (int, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 || (exclude != nil && exclude(pid)) {
			continue
		}
		state, ok := procState(procDir, pid)
		if !ok || state == 'Z' {
			continue
		}
		n++
	}
	return n, nil
}

// procState returns the state letter from /proc/<pid>/stat.
func procState(procDir string, pid int) (byte, bool) {
	b, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	// "pid (comm) S ...": comm may contain spaces and parens, so take the
	// last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0, false
	}
	return b[i+2], true
}

// idleTracker decides when the sandbox has been idle long enough.
type idleTracker struct {
	timeout    time.Duration
	lastActive time.Time
}

// observe records a scan result and reports whether no session has existed
// for a continuous timeout.
func (t *idleTracker) observe(now time.Time, sessions int) bool {
	if sessions > 0 {
		t.lastActive = now
		return false
	}
	return now.Sub(t.lastActive) >= t.timeout
}
