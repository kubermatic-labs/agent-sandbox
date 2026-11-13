package hostgit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCommandEnv(t *testing.T) {
	got := CommandEnv([]string{"HOME=/h", "GIT_DIR=/evil", "GIT_CONFIG_GLOBAL=/x", "PATH=/bin"})
	want := []string{"HOME=/h", "PATH=/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	if !slices.Equal(got, want) {
		t.Errorf("CommandEnv = %q", got)
	}
}

func TestArgs(t *testing.T) {
	got := Args("/r", "status")
	want := []string{"-C", "/r", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "status"}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %q", got)
	}
}

// TestRunnerIgnoresRepoHooksAndFsmonitor uses real git: a hostile repo
// config must not execute anything.
func TestRunnerIgnoresRepoHooksAndFsmonitor(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	r := Runner{}
	ctx := context.Background()
	if _, err := r.Run(ctx, dir, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[core]\n\tfsmonitor = touch " + marker + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"status"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"checkout", "-q", "-b", "other"},
	} {
		if _, err := r.Run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a repo hook or fsmonitor ran on the host")
	}

	_, err = r.Run(ctx, dir, "rev-parse", "--verify", "refs/heads/nope")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code == 0 || !strings.Contains(ee.Error(), "rev-parse") {
		t.Errorf("want ExitError, got %v", err)
	}
}
