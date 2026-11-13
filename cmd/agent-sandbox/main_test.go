package main

import (
	"bytes"
	"context"
	"errors"
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
	code   int
	err    error
}

func (r *recorder) cmds() commands {
	return commands{run: r.run, build: r.build, trust: r.trust}
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
