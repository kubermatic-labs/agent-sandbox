package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/app"
	"k8c.io/agent-sandbox/internal/image"
)

type recorder struct {
	runs   []app.RunOptions
	builds []image.BuildOptions
	trusts int
	calls  []string // management commands, rendered
	code   int
	err    error
}

func (r *recorder) cmds() commands {
	return commands{
		run: r.run, build: r.build, trust: r.trust,
		status: func(_ context.Context, all bool, _ io.Writer) error {
			r.calls = append(r.calls, fmt.Sprintf("status all=%v", all))
			return r.err
		},
		stop: func(_ context.Context, name string, all bool, _ io.Writer) error {
			r.calls = append(r.calls, fmt.Sprintf("stop name=%s all=%v", name, all))
			return r.err
		},
		worktreeList: func(context.Context, io.Writer) error {
			r.calls = append(r.calls, "worktree list")
			return r.err
		},
		worktreeRm: func(_ context.Context, name string, force, del bool, _ io.Writer) error {
			r.calls = append(r.calls, fmt.Sprintf("worktree rm %s force=%v delete=%v", name, force, del))
			return r.err
		},
		scaffold: func(context.Context, io.Writer) error {
			r.calls = append(r.calls, "init")
			return r.err
		},
	}
}

func (r *recorder) trust(context.Context) error {
	r.trusts++
	return r.err
}

func (r *recorder) run(_ context.Context, o app.RunOptions) (int, error) {
	r.runs = append(r.runs, o)
	return r.code, r.err
}

func (r *recorder) build(_ context.Context, o image.BuildOptions) error {
	r.builds = append(r.builds, o)
	return r.err
}

func TestArgs(t *testing.T) {
	tests := []struct {
		args []string
		want app.RunOptions
	}{
		{nil, app.RunOptions{Name: "default"}},
		{[]string{"--"}, app.RunOptions{Name: "default"}},
		{[]string{"--", "claude", "-p", "hi"}, app.RunOptions{Name: "default", Cmd: []string{"claude", "-p", "hi"}}},
		{[]string{"-n", "dev", "--", "ls"}, app.RunOptions{Name: "dev", Cmd: []string{"ls"}}},
		{[]string{"--name=dev", "--", "build"}, app.RunOptions{Name: "dev", Cmd: []string{"build"}}},
		{[]string{"--strict", "--", "ls"}, app.RunOptions{Name: "default", Cmd: []string{"ls"}, Strict: true}},
		{[]string{"--gc", "-n", "x"}, app.RunOptions{Name: "x", GC: true}},
		{[]string{"--replace"}, app.RunOptions{Name: "default", Replace: true}},
	}
	for _, tt := range tests {
		r := &recorder{}
		var stderr bytes.Buffer
		if code := run(tt.args, &stderr, r.cmds()); code != 0 {
			t.Errorf("%q: exit %d: %s", tt.args, code, stderr.String())
			continue
		}
		if len(r.runs) != 1 || len(r.builds) != 0 {
			t.Errorf("%q: runs %v, builds %v", tt.args, r.runs, r.builds)
			continue
		}
		if diff := cmp.Diff(tt.want, r.runs[0]); diff != "" {
			t.Errorf("%q (-want +got):\n%s", tt.args, diff)
		}
	}
}

func TestUnknownPositionalIsError(t *testing.T) {
	r := &recorder{}
	var stderr bytes.Buffer
	if code := run([]string{"claude"}, &stderr, r.cmds()); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if len(r.runs) != 0 || !strings.Contains(stderr.String(), "put the command after --") {
		t.Errorf("runs %v, stderr %q", r.runs, stderr.String())
	}
}

func TestExitCodePassthrough(t *testing.T) {
	r := &recorder{code: 42}
	if code := run([]string{"--", "false"}, &bytes.Buffer{}, r.cmds()); code != 42 {
		t.Errorf("exit %d, want 42", code)
	}
	r = &recorder{err: errors.New("boom")}
	var stderr bytes.Buffer
	if code := run(nil, &stderr, r.cmds()); code != 1 || !strings.Contains(stderr.String(), "agent-sandbox: boom") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestBuild(t *testing.T) {
	r := &recorder{}
	if code := run([]string{"build", "--pull"}, &bytes.Buffer{}, r.cmds()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if diff := cmp.Diff([]image.BuildOptions{{Pull: true}}, r.builds); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if code := run([]string{"build", "extra"}, &bytes.Buffer{}, r.cmds()); code != 1 {
		t.Errorf("build with args: exit %d, want 1", code)
	}
}

func TestTrust(t *testing.T) {
	r := &recorder{}
	if code := run([]string{"trust"}, &bytes.Buffer{}, r.cmds()); code != 0 || r.trusts != 1 || len(r.runs) != 0 {
		t.Errorf("exit %d, trusts %d, runs %d", code, r.trusts, len(r.runs))
	}
	if code := run([]string{"trust", "now"}, &bytes.Buffer{}, r.cmds()); code != 1 {
		t.Errorf("trust with args: exit %d", code)
	}
}

func TestManagementCommands(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"status"}, "status all=false", 0},
		{[]string{"status", "--all"}, "status all=true", 0},
		{[]string{"stop"}, "stop name=default all=false", 0},
		{[]string{"stop", "-n", "dev"}, "stop name=dev all=false", 0},
		{[]string{"stop", "--all"}, "stop name=default all=true", 0},
		{[]string{"stop", "-n", "dev", "--all"}, "", 1},
		{[]string{"worktree", "list"}, "worktree list", 0},
		{[]string{"worktree", "rm", "dev", "--force", "--delete-branch"}, "worktree rm dev force=true delete=true", 0},
		{[]string{"worktree", "rm"}, "", 1},
		{[]string{"init"}, "init", 0},
		{[]string{"init", "x"}, "", 1},
	} {
		r := &recorder{}
		var stderr bytes.Buffer
		code := run(tt.args, &stderr, r.cmds())
		got := strings.Join(r.calls, ";")
		if code != tt.code || got != tt.want || len(r.runs) != 0 {
			t.Errorf("%q: exit %d, calls %q, runs %d; want exit %d, %q (stderr %q)", tt.args, code, got, len(r.runs), tt.code, tt.want, stderr.String())
		}
	}
}
