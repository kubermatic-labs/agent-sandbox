//go:build integration

package test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const worktreeConfig = openNetwork + "git = \"worktree\"\n"

// git runs host git in the project root.
func (e *sandboxEnv) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", e.root, "-c", "user.name=host", "-c", "user.email=host@x"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newWorktreeProject is a project with one commit on main.
func newWorktreeProject(t *testing.T, extraConfig string) *sandboxEnv {
	t.Helper()
	e := newProject(t, worktreeConfig+extraConfig)
	e.git("checkout", "-q", "-b", "main")
	e.writeFile("README.md", "hello\n")
	e.git("add", ".")
	e.git("commit", "-q", "-m", "init")
	return e
}

func (e *sandboxEnv) worktreePath() string {
	return filepath.Join(e.dataHome, "worktrees", e.id, "default")
}

func TestWorktreeCommitVisibleOnHost(t *testing.T) {
	e := newWorktreeProject(t, "")
	sub := filepath.Join(e.root, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := e.mustRun(e.root, "", "--", "sh", "-c",
		"pwd && echo change >> README.md && git commit -qam 'agent change' && git log -1 --format='%an <%ae>'")
	want := e.worktreePath() + "\nagent-sandbox (default) <agent-sandbox@localhost>\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if s := e.git("log", "-1", "--format=%s", "asb/default"); s != "agent change" {
		t.Errorf("asb/default tip on the host = %q", s)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "README.md")); string(b) != "hello\n" {
		t.Errorf("main checkout changed: %q", b)
	}
	if !strings.Contains(e.git("worktree", "list"), e.worktreePath()) {
		t.Error("worktree not listed on the host")
	}
	// The second run resumes on the same worktree and branch.
	if got := e.mustRun(e.root, "", "--", "git", "log", "-1", "--format=%s"); got != "agent change\n" {
		t.Errorf("resume: %q", got)
	}
}

func TestWorktreeEscapes(t *testing.T) {
	e := newWorktreeProject(t, "")
	// A repo without objects/info: agent-sandbox must create it so it can be
	// overlaid, or the agent could add objects/info/alternates.
	if err := os.RemoveAll(filepath.Join(e.root, ".git", "objects", "info")); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(e.root, ".git")
	admin := filepath.Join(gitDir, "worktrees", "default")
	checks := []struct{ name, cmd string }{
		{"move main", "git update-ref refs/heads/main HEAD~0"},
		{"delete main", "git update-ref -d refs/heads/main"},
		{"other branch", "git branch other"},
		{"tag", "git tag v1"},
		{"alternates", "echo /tmp > " + gitDir + "/objects/info/alternates"},
		{"pack", "touch " + gitDir + "/objects/pack/x.pack"},
		{"main config", "echo '[core] fsmonitor = x' >> " + gitDir + "/config"},
		{"main hook", "echo x > " + gitDir + "/hooks/pre-commit"},
		{"packed-refs", "echo x >> " + gitDir + "/packed-refs"},
		{"commondir", "echo /tmp > " + admin + "/commondir"},
		{"admin gitdir", "echo /tmp > " + admin + "/gitdir"},
		{"worktree .git file", "echo 'gitdir: /tmp' > .git"},
		{"stash", "echo x >> README.md && git stash"},
		{"main checkout visible", "test -e " + e.root + "/README.md"},
	}
	var script strings.Builder
	for _, c := range checks {
		fmt.Fprintf(&script, "if (%s) >/dev/null 2>&1; then echo 'ALLOWED %s'; fi\n", c.cmd, c.name)
	}
	script.WriteString("git checkout -q -- README.md; git branch asb/extra && echo 'ok asb branch'\n")
	got := e.mustRun(e.root, "", "--", "sh", "-c", script.String())
	if got != "ok asb branch\n" {
		t.Errorf("escape attempts:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		t.Error("alternates written on the host")
	}
	if s := e.git("rev-parse", "--abbrev-ref", "main"); s != "main" {
		t.Errorf("main ref damaged: %q", s)
	}
	if s := e.git("branch", "--list", "asb/extra"); s == "" {
		t.Error("asb/* branch creation did not reach the host")
	}
}

func TestWorktreeCopy(t *testing.T) {
	e := newWorktreeProject(t, "worktree_copy = [\".env.local\", \"missing\"]\n")
	if err := os.WriteFile(filepath.Join(e.root, ".env.local"), []byte("TOKEN=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.root, "", "--", "cat", ".env.local")
	if r.code != 0 || r.stdout != "TOKEN=1\n" || !strings.Contains(r.stderr, "missing: not found") {
		t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestWorktreeMismatchRefused(t *testing.T) {
	e := newWorktreeProject(t, "")
	stray := e.worktreePath()
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "git worktree prune") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("stray worktree dir deleted")
	}
}

func TestWorktreeReftableRefused(t *testing.T) {
	e := newProject(t, worktreeConfig)
	if err := os.RemoveAll(filepath.Join(e.root, ".git")); err != nil {
		t.Fatal(err)
	}
	e.git("init", "-q", "--ref-format=reftable")
	e.git("commit", "-q", "--allow-empty", "-m", "init")
	r := e.run(e.root, "", "--", "true")
	if r.code != 1 || !strings.Contains(r.stderr, "reftable") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestWorktreePostScan(t *testing.T) {
	e := newWorktreeProject(t, "")
	r := e.run(e.root, "", "--", "sh", "-c", "mkdir -p x/.git && touch x/.git/config")
	if !strings.Contains(r.stderr, filepath.Join(e.worktreePath(), "x", ".git")) {
		t.Errorf("nested .git in the worktree not reported: %q", r.stderr)
	}
	if strings.Contains(r.stderr, filepath.Join(e.worktreePath(), ".git")+"\n") {
		t.Errorf("the worktree's own .git file reported: %q", r.stderr)
	}
}
