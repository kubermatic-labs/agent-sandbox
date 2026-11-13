package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, global, project string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	g, p := filepath.Join(dir, "global.toml"), filepath.Join(dir, "project.toml")
	if global != "" {
		writeFile(t, g, global)
	}
	if project != "" {
		writeFile(t, p, project)
	}
	return Load(g, p, Env{})
}

func TestLoadMissingFilesGiveDefaults(t *testing.T) {
	c, err := load(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(Defaults(), c); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestLoadMerge(t *testing.T) {
	global := `
command = ["claude"]
network = "open"
allow_cidrs = ["10.0.0.1", "192.168.1.0/24"]
caches = ["go"]
env = { A = "global", B = "global" }
resources = { memory = "4g", cpus = 2 }
strict = true
git_user_name = "Agent"
`
	project := `
command = ["opencode", "--x"]
git = "worktree"
idle_timeout = "30m"
allow_cidrs = ["10.0.0.1", "10.0.0.2"]
caches = ["go", "npm"]
env = { B = "project", C = "project" }
resources = { cpus = 1.5 }
strict = false
`
	c, err := load(t, global, project)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Command:     []string{"opencode", "--x"},
		Network:     "open",
		AllowCIDRs:  []string{"10.0.0.1", "192.168.1.0/24", "10.0.0.2"},
		Git:         "worktree",
		IdleTimeout: 30 * time.Minute,
		Env:         map[string]string{"A": "global", "B": "project", "C": "project"},
		Caches:      []string{"go", "npm"},
		Resources:   Resources{Memory: "4g", CPUs: 1.5, PIDs: 4096},
		Strict:      false,
		GitUserName: "Agent",
	}
	if diff := cmp.Diff(want, c); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, project string
		want          []string // substrings of the error
	}{
		{"unknown key", "netwrok = \"open\"", []string{"project.toml", "unknown key", "netwrok"}},
		{"unknown nested key", "[[mounts]]\nsrc = \"/x\"\ndst = \"/y\"\nmode = \"ro\"", []string{"mounts.mode"}},
		{"bad network", `network = "wide"`, []string{"project.toml", "network", "wide"}},
		{"bad git", `git = "rw"`, []string{"git"}},
		{"bad cache", `caches = ["cargo"]`, []string{"caches", "cargo"}},
		{"bad cidr", `allow_cidrs = ["10.0.0.0/33"]`, []string{"allow_cidrs"}},
		{"bad domain", `allow_domains = ["Example.com"]`, []string{"allow_domains"}},
		{"zero idle", `idle_timeout = "0s"`, []string{"idle_timeout"}},
		{"bad duration", `idle_timeout = "ten"`, []string{"project.toml"}},
		{"empty command", `command = []`, []string{"command"}},
		{"readonly escape", `readonly = ["../x"]`, []string{"readonly"}},
		{"readonly abs", `readonly = ["/x"]`, []string{"readonly"}},
		{"worktree_copy escape", `worktree_copy = ["a/../../b"]`, []string{"worktree_copy"}},
		{"env reserved", `env = { HOME = "/tmp" }`, []string{"env", "HOME", "reserved"}},
		{"env git config", `env = { GIT_CONFIG_COUNT = "1" }`, []string{"GIT_CONFIG_COUNT"}},
		{"env cache var", `env = { GOCACHE = "/tmp" }`, []string{"GOCACHE"}},
		{"env bad name", `env = { "A-B" = "x" }`, []string{"A-B"}},
		{"bad memory", `resources = { memory = "lots" }`, []string{"resources.memory"}},
		{"bad cpus type", `resources = { cpus = "4" }`, []string{"cpus"}},
		{"bad pids", `resources = { pids = 0 }`, []string{"resources.pids"}},
		{"empty git user", `git_user_name = " "`, []string{"git_user_name"}},
		{"multiline git email", "git_user_email = \"a\\nb\"", []string{"git_user_email"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, "", tt.project)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, s := range tt.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
		})
	}
}

func TestParseCIDR(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.1":      "10.0.0.1/32",
		"10.0.0.7/24":   "10.0.0.0/24",
		"fd00::1":       "fd00::1/128",
		"2001:db8::/32": "2001:db8::/32",
	} {
		p, err := ParseCIDR(in)
		if err != nil || p.String() != want {
			t.Errorf("ParseCIDR(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
}

func TestCheckSecrets(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.env")
	if ok, err := CheckSecrets(missing); ok || err != nil {
		t.Errorf("missing file: %v, %v", ok, err)
	}
	p := writeFile(t, filepath.Join(dir, "secrets.env"), "A=b\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckSecrets(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("0644 file: want chmod hint, got %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckSecrets(p); !ok || err != nil {
		t.Errorf("0600 file: %v, %v", ok, err)
	}
	if _, err := CheckSecrets(dir); err == nil {
		t.Error("directory accepted as secrets file")
	}
}

func TestAllowDomainsWithDefaults(t *testing.T) {
	c := Config{AllowDomains: []string{"api.anthropic.com", "*.example.com"}}
	got := c.AllowDomainsWithDefaults()
	if got[0] != "api.anthropic.com" || got[len(got)-1] != "*.example.com" || len(got) != len(DefaultAllowDomains)+1 {
		t.Errorf("got %q", got)
	}
	for _, d := range DefaultAllowDomains {
		if !domainRE.MatchString(d) {
			t.Errorf("default %q is not a valid allow_domains entry", d)
		}
	}
}

func TestLoadEmptyPathMeansNone(t *testing.T) {
	c, err := Load("", "", Env{})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(Defaults(), c); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
