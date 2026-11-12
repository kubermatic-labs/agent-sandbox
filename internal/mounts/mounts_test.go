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
	p, err := Build(Input{Root: "/home/u/src/proj", HasGit: true, ID: "proj-1a2b3c4d", Home: "/home/u", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{
		Mounts: []Mount{
			{Src: "/home/u/src/proj", Dst: "/home/u/src/proj", Relabel: true},
			{Src: "/home/u/src/proj/.git", Dst: "/home/u/src/proj/.git", RO: true},
			{Src: "/home/u/data/kind.yaml", Dst: "/home/u/.kube/config", RO: true, Relabel: true},
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
	p, err := Build(Input{Root: "/p", ID: "p-1", Home: "/home/u", Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Mounts {
		if m.Dst == "/p/.git" {
			t.Error(".git overlay planned for a project without .git")
		}
	}
}

func TestBuildRefusesWorktree(t *testing.T) {
	cfg := config.Defaults()
	cfg.Git = "worktree"
	if _, err := Build(Input{Root: "/p", ID: "p-1", Home: "/home/u", Config: cfg}); err == nil {
		t.Error("worktree mode must be refused until implemented")
	}
}
