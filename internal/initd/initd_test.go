package initd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeProc(t *testing.T, procs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, stat := range procs {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if stat == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCountSessions(t *testing.T) {
	proc := fakeProc(t, map[string]string{
		"1":    "1 (agent-sandbox) S 0 1 1",
		"23":   "23 (bash) S 0 23 23",
		"45":   "45 (weird) name)) R 23 45 23",
		"67":   "67 (defunct) Z 23 67 23",
		"89":   "89 (proxy) S 1 89 89",
		"91":   "", // vanished mid-scan: no stat file
		"self": "",
		"sys":  "",
	})
	n, err := CountSessions(proc, nil)
	if err != nil || n != 3 {
		t.Errorf("CountSessions = %d, %v; want 3 (23, 45, 89)", n, err)
	}
	n, err = CountSessions(proc, func(pid int) bool { return pid == 89 })
	if err != nil || n != 2 {
		t.Errorf("with exclude = %d, %v; want 2", n, err)
	}
	if _, err := CountSessions(filepath.Join(proc, "missing"), nil); err == nil {
		t.Error("expected error for a missing proc dir")
	}
}

func TestIdleTracker(t *testing.T) {
	start := time.Unix(1000, 0)
	tr := idleTracker{timeout: 10 * time.Second, lastActive: start}
	steps := []struct {
		after    time.Duration
		sessions int
		want     bool
	}{
		{5 * time.Second, 0, false},  // idle 5s
		{9 * time.Second, 1, false},  // a session resets the clock
		{15 * time.Second, 0, false}, // idle 6s since the session
		{19 * time.Second, 0, true},  // idle 10s
	}
	for _, s := range steps {
		if got := tr.observe(start.Add(s.after), s.sessions); got != s.want {
			t.Errorf("at +%v with %d sessions: %v, want %v", s.after, s.sessions, got, s.want)
		}
	}
}

func TestIdleTrackerNeverUsed(t *testing.T) {
	start := time.Unix(1000, 0)
	tr := idleTracker{timeout: time.Minute, lastActive: start}
	if !tr.observe(start.Add(time.Minute), 0) {
		t.Error("a sandbox that never had a session must also go idle")
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []Options{
		{Network: "open", Idle: time.Minute, UID: 1000},
		{Network: "restricted", Idle: time.Minute, UID: 1000, AllowCIDRs: []string{"10.0.0.1", "fd00::/8"}},
		{Network: "allowlist", Idle: time.Minute, UID: 1000, AllowDomains: []string{"api.anthropic.com"}},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("valid options %+v: %v", ok, err)
		}
	}
	for name, o := range map[string]Options{
		"bad allow cidr":  {Network: "restricted", Idle: time.Minute, UID: 1000, AllowCIDRs: []string{"10.0.0.0/33"}},
		"proxy uid":       {Network: "allowlist", Idle: time.Minute, UID: ProxyID},
		"unknown network": {Network: "wide", Idle: time.Minute, UID: 1000},
		"no idle":         {Network: "open", UID: 1000},
		"root uid":        {Network: "open", Idle: time.Minute},
		"nobody uid":      {Network: "open", Idle: time.Minute, UID: NobodyID},
	} {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMainRefusesOutsidePID1(t *testing.T) {
	var log strings.Builder
	if code := Main(Options{Network: "open", Idle: time.Minute, UID: 1000}, &log); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(log.String(), "PID 1") {
		t.Errorf("log = %q", log.String())
	}
}

// The wrapper waits for a non-empty ready file (test -s): _init creates it
// empty first and marks it only after the privilege drop.
func TestMarkReadyMakesFileNonEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ready")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Size() != 0 {
		t.Fatal("not empty before markReady")
	}
	if err := markReady(f); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Size() == 0 {
		t.Error("still empty after markReady")
	}
}
