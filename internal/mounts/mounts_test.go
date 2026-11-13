package mounts

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/config"
)

func TestArg(t *testing.T) {
	for m, want := range map[Mount]string{
		{Src: "/a", Dst: "/b"}:                          "/a:/b",
		{Src: "/a", Dst: "/b", RO: true}:                "/a:/b:ro",
		{Src: "/a", Dst: "/b", Relabel: true}:           "/a:/b:z",
		{Src: "/a", Dst: "/b", RO: true, Relabel: true}: "/a:/b:ro,z",
		{Src: "vol", Dst: "/h", Volume: true}:           "vol:/h",
	} {
		if got := m.Arg(); got != want {
			t.Errorf("%+v.Arg() = %q, want %q", m, got, want)
		}
	}
}

func TestBuildRO(t *testing.T) {
	cfg := config.Defaults()
	cfg.Caches = []string{"go", "npm"}
	cfg.Mounts = []config.Mount{{Src: "/home/u/data/kind.yaml", Dst: "/home/u/.kube/config", RO: true}}
	p, err := Build(Input{Root: "/home/u/src/proj", Deny: []string{".git", ".envrc"}, ID: "proj-1a2b3c4d", Home: "/home/u", Binary: "/home/u/.local/share/agent-sandbox/bin/0123/agent-sandbox", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{
		Mounts: []Mount{
			{Src: "/home/u/src/proj", Dst: "/home/u/src/proj", Relabel: true},
			{Src: "/home/u/src/proj/.git", Dst: "/home/u/src/proj/.git", RO: true},
			{Src: "/home/u/src/proj/.envrc", Dst: "/home/u/src/proj/.envrc", RO: true},
			{Src: "/home/u/data/kind.yaml", Dst: "/home/u/.kube/config", RO: true, Relabel: true},
			{Src: "/home/u/.local/share/agent-sandbox/bin/0123/agent-sandbox", Dst: "/usr/local/bin/agent-sandbox", RO: true, Relabel: true},
			{Src: "asb-proj-1a2b3c4d-home", Dst: "/home/u", Volume: true},
			{Src: "asb-proj-1a2b3c4d-go-mod", Dst: "/cache/go-mod", Volume: true},
			{Src: "asb-proj-1a2b3c4d-go-build", Dst: "/cache/go-build", Volume: true},
			{Src: "asb-proj-1a2b3c4d-npm", Dst: "/cache/npm", Volume: true},
		},
		Volumes: []Volume{
			{Name: "asb-proj-1a2b3c4d-home", Home: true},
			{Name: "asb-proj-1a2b3c4d-go-mod"},
			{Name: "asb-proj-1a2b3c4d-go-build"},
			{Name: "asb-proj-1a2b3c4d-npm"},
		},
		Env: []string{"GOMODCACHE=/cache/go-mod", "GOCACHE=/cache/go-build", "npm_config_cache=/cache/npm"},
	}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestBuildWithoutGit(t *testing.T) {
	p, err := Build(Input{Root: "/p", ID: "p-1", Home: "/home/u", Binary: "/b", Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Mounts {
		if m.Dst == "/p/.git" {
			t.Error(".git overlay planned for a project without .git")
		}
	}
}

func TestBuildWorktree(t *testing.T) {
	cfg := config.Defaults()
	cfg.Git = "worktree"
	wt := "/home/u/.local/share/agent-sandbox/worktrees/proj-1/default"
	p, err := Build(Input{
		Root: wt, Deny: []string{".git"}, ID: "proj-1", Home: "/home/u", Binary: "/b", Config: cfg,
		Worktree: &GitMount{GitDir: "/home/u/src/proj/.git", AdminDir: "/home/u/src/proj/.git/worktrees/default", ConfigWorktree: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range p.Mounts {
		if !m.Volume && m.Dst != BinaryPath {
			got = append(got, m.Arg())
		}
	}
	want := []string{
		wt + ":" + wt + ":z",
		wt + "/.git:" + wt + "/.git:ro",
		"/home/u/src/proj/.git:/home/u/src/proj/.git:ro,z",
		"/home/u/src/proj/.git/objects:/home/u/src/proj/.git/objects",
		"/home/u/src/proj/.git/objects/pack:/home/u/src/proj/.git/objects/pack:ro",
		"/home/u/src/proj/.git/objects/info:/home/u/src/proj/.git/objects/info:ro",
		"/home/u/src/proj/.git/refs/heads/asb:/home/u/src/proj/.git/refs/heads/asb",
		"/home/u/src/proj/.git/logs/refs/heads/asb:/home/u/src/proj/.git/logs/refs/heads/asb",
		"/home/u/src/proj/.git/worktrees/default:/home/u/src/proj/.git/worktrees/default",
		"/home/u/src/proj/.git/worktrees/default/commondir:/home/u/src/proj/.git/worktrees/default/commondir:ro",
		"/home/u/src/proj/.git/worktrees/default/gitdir:/home/u/src/proj/.git/worktrees/default/gitdir:ro",
		"/home/u/src/proj/.git/worktrees/default/config.worktree:/home/u/src/proj/.git/worktrees/default/config.worktree:ro",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestBuildModeChecks(t *testing.T) {
	ro, wt := config.Defaults(), config.Defaults()
	wt.Git = "worktree"
	g := &GitMount{GitDir: "/p/.git", AdminDir: "/p/.git/worktrees/x"}
	for name, in := range map[string]Input{
		"worktree without git mount": {Root: "/w", Binary: "/b", Config: wt},
		"ro with git mount":          {Root: "/p", Binary: "/b", Config: ro, Worktree: g},
		"admin outside worktrees":    {Root: "/w", Binary: "/b", Config: wt, Worktree: &GitMount{GitDir: "/p/.git", AdminDir: "/p/.git"}},
		"admin nested":               {Root: "/w", Binary: "/b", Config: wt, Worktree: &GitMount{GitDir: "/p/.git", AdminDir: "/p/.git/worktrees/x/y"}},
		"admin elsewhere":            {Root: "/w", Binary: "/b", Config: wt, Worktree: &GitMount{GitDir: "/p/.git", AdminDir: "/q/.git/worktrees/x"}},
	} {
		if _, err := Build(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildRequiresBinary(t *testing.T) {
	if _, err := Build(Input{Root: "/p", ID: "p-1", Home: "/home/u", Config: config.Defaults()}); err == nil {
		t.Error("a plan without the binary mount must be refused")
	}
}

func TestBuildRefusesDenyOutsideRoot(t *testing.T) {
	for _, bad := range []string{"../x", ".", "/etc"} {
		if _, err := Build(Input{Root: "/p", Deny: []string{bad}, ID: "p-1", Home: "/home/u", Binary: "/b", Config: config.Defaults()}); err == nil {
			t.Errorf("deny entry %q accepted", bad)
		}
	}
}
