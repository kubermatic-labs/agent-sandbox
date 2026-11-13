package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8c.io/agent-sandbox/internal/hostgit"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, initArgs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, append([]string{"init", "-q", "-b", "main"}, initArgs...)...)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", ".")
	gitCmd(t, root, "commit", "-q", "-m", "init")
	return root
}

func input(t *testing.T, root string) Input {
	t.Helper()
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Input{Root: root, ProjectID: "p-1", Name: "default", DataDir: data, Home: t.TempDir(), Log: &bytes.Buffer{}}
}

func TestEnsureCreatesAndResumes(t *testing.T) {
	root := newRepo(t)
	if err := os.RemoveAll(filepath.Join(root, ".git", "objects", "info")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := input(t, root)
	in.Copy = []string{".env.local", "missing"}

	w, err := Ensure(context.Background(), hostgit.Runner{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Created || w.Path != Path(in.DataDir, "p-1", "default") || w.Branch != "asb/default" {
		t.Errorf("worktree = %+v", w)
	}
	if got := gitCmd(t, w.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "asb/default" {
		t.Errorf("branch = %q", got)
	}
	if w.AdminDir != filepath.Join(root, ".git", "worktrees", "default") {
		t.Errorf("AdminDir = %q", w.AdminDir)
	}
	for _, d := range []string{"refs/heads/asb", "logs/refs/heads/asb", "objects/pack", "objects/info"} {
		if fi, err := os.Stat(filepath.Join(root, ".git", d)); err != nil || !fi.IsDir() {
			t.Errorf("%s not prepared: %v", d, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(w.Path, ".env.local")); string(b) != "SECRET=1\n" {
		t.Errorf("worktree_copy: %q", b)
	}
	if !strings.Contains(in.Log.(*bytes.Buffer).String(), "missing: not found") {
		t.Errorf("no warning for a missing worktree_copy entry: %q", in.Log)
	}

	// A commit on the branch survives; the next Ensure resumes.
	gitCmd(t, w.Path, "commit", "-q", "--allow-empty", "-m", "agent work")
	w2, err := Ensure(context.Background(), hostgit.Runner{}, in)
	if err != nil || w2.Created || w2.Path != w.Path {
		t.Fatalf("resume: %+v, %v", w2, err)
	}
	if got := gitCmd(t, root, "log", "-1", "--format=%s", "asb/default"); got != "agent work" {
		t.Errorf("branch tip = %q", got)
	}
}

func TestEnsureReusesExistingBranch(t *testing.T) {
	root := newRepo(t)
	gitCmd(t, root, "branch", "asb/default")
	gitCmd(t, root, "commit", "-q", "--allow-empty", "-m", "main moves on")
	w, err := Ensure(context.Background(), hostgit.Runner{}, input(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, w.Path, "log", "-1", "--format=%s"); got != "init" {
		t.Errorf("existing branch not checked out: tip %q", got)
	}
}

func TestEnsureMismatchDeletesNothing(t *testing.T) {
	root := newRepo(t)
	in := input(t, root)
	stray := Path(in.DataDir, "p-1", "default")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), hostgit.Runner{}, in); err == nil || !strings.Contains(err.Error(), "git worktree prune") {
		t.Errorf("unregistered dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "keep")); err != nil {
		t.Error("stray dir content deleted")
	}

	root2 := newRepo(t)
	in2 := input(t, root2)
	w, err := Ensure(context.Background(), hostgit.Runner{}, in2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), hostgit.Runner{}, in2); err == nil || !strings.Contains(err.Error(), "directory is missing") {
		t.Errorf("registration without dir: %v", err)
	}
}

func TestEnsureRefusesReftableAndNonMain(t *testing.T) {
	root := newRepo(t, "--ref-format=reftable")
	if _, err := Ensure(context.Background(), hostgit.Runner{}, input(t, root)); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Errorf("reftable: %v", err)
	}
	if _, err := Ensure(context.Background(), hostgit.Runner{}, input(t, t.TempDir())); err == nil {
		t.Error("a directory without .git accepted")
	}
}

func TestEnsureWorktreeConfig(t *testing.T) {
	root := newRepo(t)
	gitCmd(t, root, "config", "extensions.worktreeConfig", "true")
	w, err := Ensure(context.Background(), hostgit.Runner{}, input(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if !w.ConfigWorktree {
		t.Error("ConfigWorktree not set")
	}
	if _, err := os.Stat(filepath.Join(w.AdminDir, "config.worktree")); err != nil {
		t.Errorf("config.worktree not created: %v", err)
	}
}

func TestCopySkipsSymlinks(t *testing.T) {
	root := newRepo(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	in := input(t, root)
	in.Copy = []string{"linked", "linked/secret"}
	w, err := Ensure(context.Background(), hostgit.Runner{}, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"linked", "linked/secret"} {
		if _, err := os.Lstat(filepath.Join(w.Path, p)); err == nil && p == "linked/secret" {
			t.Errorf("%s copied through a symlink", p)
		}
	}
	if !strings.Contains(in.Log.(*bytes.Buffer).String(), "symlink") {
		t.Errorf("no symlink warning: %q", in.Log)
	}
}

// The agent commits symlinks on its asb/ branch; when the worktree is
// recreated from that branch, worktree_copy must not write through them.
func TestCopyRefusesSymlinksFromBranch(t *testing.T) {
	root := newRepo(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "bashrc")
	if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant the symlinks on asb/default from a scratch worktree.
	scratch := filepath.Join(t.TempDir(), "scratch")
	gitCmd(t, root, "worktree", "add", "-q", "-b", "asb/default", scratch)
	if err := os.Symlink(victim, filepath.Join(scratch, ".env.local")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scratch, "config")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, scratch, "add", ".")
	gitCmd(t, scratch, "commit", "-q", "-m", "plant")
	gitCmd(t, root, "worktree", "remove", scratch)

	for name, content := range map[string]string{".env.local": "SECRET=1\n", "config/app.env": "X=1\n", "ok.env": "OK=1\n"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := input(t, root)
	in.Copy = []string{".env.local", "config", "ok.env"}
	w, err := Ensure(context.Background(), hostgit.Runner{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original\n" {
		t.Errorf("wrote through a file symlink: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(outside, "app.env")); err == nil {
		t.Error("wrote through a directory symlink")
	}
	if b, err := os.ReadFile(filepath.Join(w.Path, "ok.env")); err != nil || string(b) != "OK=1\n" {
		t.Errorf("ordinary entry not copied: %q, %v", b, err)
	}
	if got := strings.Count(in.Log.(*bytes.Buffer).String(), "symlink or lies under one"); got != 2 {
		t.Errorf("want 2 refusals, log: %q", in.Log)
	}
}
