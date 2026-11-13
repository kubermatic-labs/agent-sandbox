package hostgit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	write(t, cfg, `# comment
[core]
	bare = false
	hooksPath = .githooks ; trailing comment
	fsmonitor
[Core]
	HooksPath = "with space/hooks" # another
[remote "Origin"]
	url = https://example.com/x.git
[extensions]
	refStorage = reftable
[branch.main]
	merge = refs/heads/main
[alias]
	long = log \
		--oneline
	q = "a \"quoted\" \\ value"
[include]
	path = inc.conf
`)
	write(t, filepath.Join(dir, "inc.conf"), "[core]\n\thooksPath = from-include\n[include]\n\tpath = missing.conf\n")

	c, err := ParseFiles("/home/u", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{".githooks", "with space/hooks", "from-include"}, c.All("core.hookspath")); diff != "" {
		t.Errorf("hooksPath (-want +got):\n%s", diff)
	}
	for key, want := range map[string]string{
		"core.fsmonitor":        "true",
		"remote.Origin.url":     "https://example.com/x.git",
		"extensions.refstorage": "reftable",
		"branch.main.merge":     "refs/heads/main",
		"alias.long":            "log \t\t--oneline", // git keeps internal whitespace verbatim
		"alias.q":               `a "quoted" \ value`,
	} {
		if got, ok := c.Get(key); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", key, got, ok, want)
		}
	}
	if _, ok := c.Get("remote.origin.url"); ok {
		t.Error("quoted subsections are case-sensitive")
	}
}

func TestIncludeIfAndHome(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "extra.conf"), "[core]\n\thooksPath = ~/hooks\n")
	cfg := filepath.Join(t.TempDir(), "config")
	write(t, cfg, "[includeIf \"gitdir:~/src/\"]\n\tpath = ~/extra.conf\n")
	c, err := ParseFiles(home, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("core.hooksPath"); got != "~/hooks" {
		t.Errorf("hooksPath = %q", got)
	}
}

func TestIncludeLoop(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), "[include]\n\tpath = b\n")
	write(t, filepath.Join(dir, "b"), "[include]\n\tpath = a\n")
	if _, err := ParseFiles("/h", filepath.Join(dir, "a")); err == nil {
		t.Error("include loop not detected")
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"unterminated": "[core\n",
		"no section":   "key = value\n",
	} {
		p := filepath.Join(dir, name)
		write(t, p, content)
		if _, err := ParseFiles("/h", p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGitDirs(t *testing.T) {
	base := t.TempDir()

	plain := filepath.Join(base, "plain")
	write(t, filepath.Join(plain, ".git", "config"), "")
	if g, c, ok, err := GitDirs(plain); !ok || err != nil || g != filepath.Join(plain, ".git") || c != g {
		t.Errorf("plain: %q %q %v %v", g, c, ok, err)
	}

	main := filepath.Join(base, "main")
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	write(t, filepath.Join(wtGit, "commondir"), "../..\n")
	wt := filepath.Join(base, "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")
	if g, c, ok, err := GitDirs(wt); !ok || err != nil || g != wtGit || c != filepath.Join(main, ".git") {
		t.Errorf("worktree: %q %q %v %v", g, c, ok, err)
	}

	sub := filepath.Join(main, "sub")
	write(t, filepath.Join(main, ".git", "modules", "sub", "config"), "")
	write(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
	if g, _, ok, err := GitDirs(sub); !ok || err != nil || g != filepath.Join(main, ".git", "modules", "sub") {
		t.Errorf("submodule: %q %v %v", g, ok, err)
	}

	if _, _, ok, err := GitDirs(filepath.Join(base, "none")); ok || err != nil {
		t.Errorf("no .git: %v %v", ok, err)
	}
	bad := filepath.Join(base, "bad")
	write(t, filepath.Join(bad, ".git"), "nonsense\n")
	if _, _, _, err := GitDirs(bad); err == nil {
		t.Error("bad .git file accepted")
	}
}

func TestRepoConfigAndHooksPaths(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "proj")
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\thooksPath = .husky\n")
	write(t, filepath.Join(root, ".git", "config"), "[core]\n\thooksPath = tools/hooks\n[core]\n\thooksPath = /usr/share/hooks\n[core]\n\thooksPath = ~/global-hooks\n[core]\n\thooksPath = ../outside\n[core]\n\thooksPath = .\n")
	cfg, err := RepoConfig(root, Env{Home: home, ConfigHome: filepath.Join(home, ".config")})
	if err != nil {
		t.Fatal(err)
	}
	got := HooksPathsInside(cfg, root, home)
	if diff := cmp.Diff([]string{".husky", "tools/hooks"}, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestIncludesInside(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "proj")
	write(t, filepath.Join(home, ".gitconfig"), "[include]\n\tpath = ~/extra.conf\n")
	write(t, filepath.Join(root, ".git", "config"), "[include]\n\tpath = ../.gitconfig\n[includeIf \"gitdir:/x/\"]\n\tpath = ../conf/missing.inc\n[include]\n\tpath = /etc/elsewhere\n[include]\n\tpath = local.inc\n")
	write(t, filepath.Join(root, ".gitconfig"), "[include]\n\tpath = nested.inc\n")
	cfg, err := RepoConfig(root, Env{Home: home, ConfigHome: filepath.Join(home, ".config")})
	if err != nil {
		t.Fatal(err)
	}
	// Missing targets count too; targets outside root do not.
	want := []string{".gitconfig", "nested.inc", "conf/missing.inc", ".git/local.inc"}
	if diff := cmp.Diff(want, IncludesInside(cfg, root)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
