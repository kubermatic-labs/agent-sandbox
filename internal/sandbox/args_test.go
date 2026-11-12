package sandbox

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/project"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, args []string) {
	t.Helper()
	got := strings.Join(args, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("%s mismatch (-want +got):\n%s", path, diff)
	}
}

func goldenPlan(t *testing.T) (project.Project, config.Config, mounts.Plan) {
	t.Helper()
	p := project.Project{Root: "/home/u/src/proj", HasGit: true, ID: "proj-1a2b3c4d"}
	cfg := config.Defaults()
	cfg.Caches = []string{"go"}
	cfg.Env = map[string]string{"FOO": "bar", "A": "b"}
	plan, err := mounts.Build(mounts.Input{Root: p.Root, HasGit: p.HasGit, ID: p.ID, Home: "/home/u", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return p, cfg, plan
}

func TestRunArgsGoldenRO(t *testing.T) {
	p, cfg, plan := goldenPlan(t)
	golden(t, "run-ro", RunArgs(RunInput{
		Container: project.ContainerName(p.ID, "default"),
		Labels:    Labels(p, "default", "ro", "feedface", "beef"),
		Hostname:  "asb-proj",
		Resources: cfg.Resources,
		Mounts:    plan.Mounts,
		EnvFile:   "/home/u/.config/agent-sandbox/secrets.env",
		Image:     "localhost/agent-sandbox-base:0123456789abcdef",
		Command:   []string{"sleep", "infinity"},
	}))
}

func TestExecArgsGolden(t *testing.T) {
	p, cfg, plan := goldenPlan(t)
	golden(t, "exec-tty", ExecArgs(ExecInput{
		Container: project.ContainerName(p.ID, "default"),
		UID:       1000, GID: 1000,
		Workdir: "/home/u/src/proj/pkg",
		TTY:     true,
		Env:     SessionEnv(cfg.Env, plan.Env),
		Cmd:     []string{"claude", "--help"},
	}))
}

func TestExecArgsNoTTY(t *testing.T) {
	args := ExecArgs(ExecInput{Container: "c", UID: 1, GID: 2, Workdir: "/w", Cmd: []string{"cat"}})
	if slices.Contains(args, "-t") || !slices.Contains(args, "-i") {
		t.Errorf("without a TTY want -i and no -t: %q", args)
	}
}

func TestRunArgsNoEnvFile(t *testing.T) {
	args := RunArgs(RunInput{Container: "c", Resources: config.Defaults().Resources, Image: "img", Command: []string{"x"}})
	if slices.Contains(args, "--env-file") {
		t.Errorf("--env-file without a secrets file: %q", args)
	}
	if slices.Contains(args, "--rm") {
		t.Errorf("--rm must not be used: %q", args)
	}
}

func TestVolumeInitArgs(t *testing.T) {
	got := VolumeInitArgs("vol", 1000, 1001, "img", "")
	want := []string{"run", "--rm", "--userns=keep-id", "--user", "1000:1001", "--network", "none",
		"--cap-drop=all", "--security-opt", "no-new-privileges", "-v", "vol:/v:U", "img", "touch", "/v/.agent-sandbox-volume"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("no seed (-want +got):\n%s", diff)
	}
	got = VolumeInitArgs("vol", 1000, 1001, "img", "/run/seed-1")
	want = append(want[:len(want)-3], "-v", "/run/seed-1:/seed:ro,Z", "img",
		"sh", "-c", "cp -R --preserve=mode,timestamps /seed/. /v/ && touch /v/.agent-sandbox-volume")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("seed (-want +got):\n%s", diff)
	}
}
