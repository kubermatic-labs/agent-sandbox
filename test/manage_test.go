//go:build integration

package test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// row returns the fields of the table row whose first column is name.
func row(out, name string) []string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return f
		}
	}
	return nil
}

func TestStatusAndStop(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "true")
	e.mustRun(e.root, "", "-n", "dev", "--", "true")

	out := e.mustRun(e.root, "", "status")
	for _, name := range []string{"default", "dev"} {
		f := row(out, name)
		if len(f) != 7 || f[1] != "running" || f[4] != "no" || f[5] != "ro" || f[6] != e.root {
			t.Errorf("status row %s = %q\n%s", name, f, out)
		}
	}

	if got := e.mustRun(e.root, "", "stop"); got != "stopped default\n" {
		t.Errorf("stop: %q", got)
	}
	out = e.mustRun(e.root, "", "status")
	if row(out, "default") != nil || row(out, "dev") == nil {
		t.Errorf("after stop:\n%s", out)
	}
	if got := e.mustRun(e.root, "", "stop", "--all"); got != "stopped dev\n" {
		t.Errorf("stop --all: %q", got)
	}
	if got := e.mustRun(e.root, "", "status"); got != "no sandboxes\n" {
		t.Errorf("status after stop --all: %q", got)
	}
}

func TestStatusCountsSessions(t *testing.T) {
	e := newProject(t, openNetwork)
	e.mustRun(e.root, "", "--", "true")
	var wg sync.WaitGroup
	wg.Go(func() { e.run(e.root, "", "--", "sleep", "6") })
	time.Sleep(2 * time.Second)
	out := e.mustRun(e.root, "", "status")
	wg.Wait()
	if f := row(out, "default"); len(f) < 4 || f[3] != "1" {
		t.Errorf("sessions while one runs: %q\n%s", f, out)
	}
}

func TestStatusAllListsVolumes(t *testing.T) {
	e := newProject(t, openNetwork+"caches = [\"go\"]\n")
	e.mustRun(e.root, "", "--", "true")
	out := e.mustRun(e.root, "", "status", "--all")
	if !strings.Contains(out, e.root) {
		t.Errorf("sandbox missing from status --all:\n%s", out)
	}
	f := row(out, e.id)
	if len(f) != 4 || f[1] != "asb-"+e.id+"-go-build" || f[3] != "asb-"+e.id+"-home" {
		t.Errorf("volumes row = %q\n%s", f, out)
	}
}

func TestInitScaffoldsAndTrusts(t *testing.T) {
	e := newProject(t, openNetwork)
	r := e.run(e.root, "", "init")
	if r.code != 0 || !strings.Contains(r.stdout, "trusted") {
		t.Fatalf("init: exit %d, %q %q", r.code, r.stdout, r.stderr)
	}
	for _, f := range []string{"config.toml", "Containerfile"} {
		if _, err := os.Stat(filepath.Join(e.root, ".agent-sandbox", f)); err != nil {
			t.Errorf("%s not created: %v", f, err)
		}
	}
	// Trusted without a prompt; the scaffolded Containerfile builds.
	e.mustRun(e.root, "", "--", "true")
	if tags := e.projectImageTags(); len(tags) != 1 {
		t.Errorf("project image tags = %q", tags)
	}
	if r := e.run(e.root, "", "init"); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Errorf("second init: exit %d, %q", r.code, r.stderr)
	}
}

func TestWorktreeListAndRm(t *testing.T) {
	e := newWorktreeProject(t, "")
	e.mustRun(e.root, "", "--", "sh", "-c", "echo x > f && git add f && git commit -qm work && echo y > untracked")

	out := e.mustRun(e.root, "", "worktree", "list")
	f := row(out, "default")
	if len(f) != 6 || f[1] != "asb/default" || f[2] != "1" || f[3] != "yes" || f[4] != "yes" || f[5] != e.worktreePath() {
		t.Errorf("worktree list row = %q\n%s", f, out)
	}

	if r := e.run(e.root, "", "worktree", "rm", "default"); r.code != 1 || !strings.Contains(r.stderr, "is running") {
		t.Errorf("rm while running: exit %d, %q", r.code, r.stderr)
	}
	e.mustRun(e.root, "", "stop")
	if r := e.run(e.root, "", "worktree", "rm", "default"); r.code != 1 || !strings.Contains(r.stderr, "uncommitted") {
		t.Errorf("rm while dirty: exit %d, %q", r.code, r.stderr)
	}
	if _, err := os.Stat(e.worktreePath()); err != nil {
		t.Fatal("a refused rm removed the worktree")
	}

	r := e.run(e.root, "", "worktree", "rm", "default", "--force", "--delete-branch")
	if r.code != 0 || !strings.Contains(r.stdout, "deleted branch asb/default") {
		t.Fatalf("rm --force --delete-branch: exit %d, %q %q", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(e.worktreePath()); !os.IsNotExist(err) {
		t.Error("worktree dir still there")
	}
	if b := e.git("branch", "--list", "asb/default"); b != "" {
		t.Errorf("branch still there: %q", b)
	}
	if got := e.mustRun(e.root, "", "worktree", "list"); got != "no worktrees\n" {
		t.Errorf("list after rm: %q", got)
	}
}

func TestWorktreeRmKeepsBranchByDefault(t *testing.T) {
	e := newWorktreeProject(t, "")
	e.mustRun(e.root, "", "--", "sh", "-c", "git commit -q --allow-empty -m keep")
	e.mustRun(e.root, "", "stop")
	e.mustRun(e.root, "", "worktree", "rm", "default")
	if s := e.git("log", "-1", "--format=%s", "asb/default"); s != "keep" {
		t.Errorf("branch lost: %q", s)
	}
}
