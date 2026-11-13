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
	plan, err := mounts.Build(mounts.Input{Root: p.Root, Deny: []string{".git"}, ID: p.ID, Home: "/home/u", Binary: "/home/u/.local/share/agent-sandbox/bin/0123456789abcdef/agent-sandbox", Config: cfg})
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
		Command:   InitCommand(cfg, 1000),
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

func TestInitCommand(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network = "restricted"
	cfg.AllowCIDRs = []string{"10.0.0.1", "192.168.1.0/24"}
	want := []string{"/usr/local/bin/agent-sandbox", "_init", "--network=restricted", "--idle=10m0s", "--uid=1000",
		"--allow-cidr=10.0.0.1", "--allow-cidr=192.168.1.0/24"}
	if diff := cmp.Diff(want, InitCommand(cfg, 1000)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestRunArgsGoldenWorktree(t *testing.T) {
	p := project.Project{Root: "/home/u/src/proj", HasGit: true, ID: "proj-1a2b3c4d"}
	cfg := config.Defaults()
	cfg.Git = "worktree"
	wt := "/home/u/.local/share/agent-sandbox/worktrees/proj-1a2b3c4d/default"
	plan, err := mounts.Build(mounts.Input{
		Root: wt, Deny: []string{".git", ".envrc"}, ID: p.ID, Home: "/home/u",
		Binary: "/home/u/.local/share/agent-sandbox/bin/0123456789abcdef/agent-sandbox", Config: cfg,
		Worktree: &mounts.GitMount{GitDir: "/home/u/src/proj/.git", AdminDir: "/home/u/src/proj/.git/worktrees/default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "run-worktree", RunArgs(RunInput{
		Container: project.ContainerName(p.ID, "default"),
		Labels:    Labels(p, "default", "worktree", "feedface", "beef"),
		Hostname:  "asb-proj",
		Resources: cfg.Resources,
		Mounts:    plan.Mounts,
		Image:     "localhost/agent-sandbox-base:0123456789abcdef",
		Command:   InitCommand(cfg, 1000),
	}))
}

func TestGitSessionEnv(t *testing.T) {
	want := []string{
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0",
		"GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=0",
		"GIT_CONFIG_KEY_2=user.name", "GIT_CONFIG_VALUE_2=agent-sandbox (dev)",
		"GIT_CONFIG_KEY_3=user.email", "GIT_CONFIG_VALUE_3=agent-sandbox@localhost",
	}
	if diff := cmp.Diff(want, GitSessionEnv("dev", "", "")); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got := GitSessionEnv("dev", "Me", "me@x"); got[6] != "GIT_CONFIG_VALUE_2=Me" || got[8] != "GIT_CONFIG_VALUE_3=me@x" {
		t.Errorf("overrides: %q", got)
	}
}

func TestInitCommandAllowlist(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network = "allowlist"
	cfg.AllowDomains = []string{"*.example.com"}
	got := InitCommand(cfg, 1000)
	if !slices.Contains(got, "--network=allowlist") || !slices.Contains(got, "--allow-domain=api.anthropic.com") ||
		got[len(got)-1] != "--allow-domain=*.example.com" {
		t.Errorf("InitCommand = %q", got)
	}
	cfg.Network = "restricted"
	for _, a := range InitCommand(cfg, 1000) {
		if strings.HasPrefix(a, "--allow-domain") {
			t.Errorf("restricted mode got %q", a)
		}
	}
}

func TestProxyEnv(t *testing.T) {
	want := []string{
		"HTTP_PROXY=http://127.0.0.1:3128", "HTTPS_PROXY=http://127.0.0.1:3128",
		"http_proxy=http://127.0.0.1:3128", "https_proxy=http://127.0.0.1:3128",
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
	if diff := cmp.Diff(want, ProxyEnv("127.0.0.1:3128")); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestConfigHash(t *testing.T) {
	p, cfg, plan := goldenPlan(t)
	base := RunInput{
		Container: project.ContainerName(p.ID, "default"),
		Labels:    Labels(p, "default", "ro", "feedface", "beef"),
		Hostname:  "asb-proj",
		Resources: cfg.Resources,
		Mounts:    plan.Mounts,
		Image:     "img:1",
		Command:   InitCommand(cfg, 1000),
	}
	h := ConfigHash(base)
	if got := RunArgs(base); !slices.Contains(got, ConfigLabel+"="+h) {
		t.Errorf("RunArgs lacks the %s label: %q", ConfigLabel, got)
	}
	same := map[string]func(*RunInput){
		"image":  func(in *RunInput) { in.Image = "img:2" },
		"labels": func(in *RunInput) { in.Labels = Labels(p, "default", "ro", "cafe", "f00d") },
	}
	for name, mod := range same {
		in := base
		mod(&in)
		if ConfigHash(in) != h {
			t.Errorf("%s changed the config hash; it is only stale", name)
		}
	}
	open, allowCIDR := cfg, cfg
	open.Network = "open"
	allowCIDR.AllowCIDRs = []string{"10.0.0.0/8"}
	differ := map[string]func(*RunInput){
		"network":    func(in *RunInput) { in.Command = InitCommand(open, 1000) },
		"allow_cidr": func(in *RunInput) { in.Command = InitCommand(allowCIDR, 1000) },
		"mount": func(in *RunInput) {
			in.Mounts = append(slices.Clone(in.Mounts), mounts.Mount{Src: "/home/u/.aws", Dst: "/x", RO: true})
		},
		"readonly":  func(in *RunInput) { in.Mounts = slices.Clone(in.Mounts[:len(in.Mounts)-1]) },
		"resources": func(in *RunInput) { in.Resources.Memory = "1g" },
		"env file":  func(in *RunInput) { in.EnvFile = "/s.env" },
	}
	for name, mod := range differ {
		in := base
		mod(&in)
		if ConfigHash(in) == h {
			t.Errorf("%s did not change the config hash", name)
		}
	}
}
