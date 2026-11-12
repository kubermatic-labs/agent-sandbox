package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountEnv builds a fake home with the interesting locations in it.
func mountEnv(t *testing.T) Env {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	for _, d := range []string{
		".ssh", ".config", ".local/bin", ".local/share/agent-sandbox", ".local/share/containers",
		"src/proj/.git", "data/kube", "bin",
	} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".ssh/id_ed25519", ".bashrc", "data/kube/kind.yaml"} {
		writeFile(t, filepath.Join(home, f), "x")
	}
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(home, "data", "innocent")); err != nil {
		t.Fatal(err)
	}
	return Env{
		Home:        home,
		ConfigHome:  filepath.Join(home, ".config"),
		DataHome:    filepath.Join(home, ".local", "share"),
		XDGRuntime:  filepath.Join(base, "run"),
		DataDir:     filepath.Join(home, ".local", "share", "agent-sandbox"),
		ProjectRoot: filepath.Join(home, "src", "proj"),
		PathDirs:    []string{filepath.Join(home, "bin"), "relative/ignored"},
	}
}

func TestResolveMountAccepts(t *testing.T) {
	env := mountEnv(t)
	tests := []struct {
		in, want Mount
	}{
		{
			Mount{Src: "~/data/kube/kind.yaml", Dst: "~/.kube/config", RO: true},
			Mount{Src: env.Home + "/data/kube/kind.yaml", Dst: env.Home + "/.kube/config", RO: true},
		},
		{Mount{Src: "~/data", Dst: "/data"}, Mount{Src: env.Home + "/data", Dst: "/data"}},
		{Mount{Src: "~/.bashrc", Dst: "/x", RO: true}, Mount{Src: env.Home + "/.bashrc", Dst: "/x", RO: true}},
		{Mount{Src: "~/bin", Dst: "/b", RO: true}, Mount{Src: env.Home + "/bin", Dst: "/b", RO: true}},
	}
	for _, tt := range tests {
		got, err := resolveMount(tt.in, env)
		if err != nil {
			t.Errorf("resolveMount(%+v): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolveMount(%+v) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestResolveMountRefuses(t *testing.T) {
	env := mountEnv(t)
	tests := []struct {
		name string
		m    Mount
		want string
	}{
		{"root", Mount{Src: "/", Dst: "/r", RO: true}, "not allowed"},
		{"home", Mount{Src: "~", Dst: "/h", RO: true}, "not allowed"},
		{"etc", Mount{Src: "/etc", Dst: "/e", RO: true}, "protected"},
		{"ssh key", Mount{Src: "~/.ssh/id_ed25519", Dst: "/k", RO: true}, "protected"},
		{"symlink into ssh", Mount{Src: "~/data/innocent", Dst: "/k", RO: true}, "protected"},
		{"config", Mount{Src: "~/.config", Dst: "/c", RO: true}, "protected"},
		{"runtime dir", Mount{Src: env.XDGRuntime, Dst: "/r", RO: true}, "protected"},
		{"podman storage", Mount{Src: "~/.local/share/containers", Dst: "/s", RO: true}, "protected"},
		{"data dir", Mount{Src: "~/.local/share/agent-sandbox", Dst: "/d", RO: true}, "protected"},
		{"project ancestor", Mount{Src: "~/src", Dst: "/s"}, "overlaps"},
		{"inside project", Mount{Src: "~/src/proj/.git", Dst: "/g"}, "overlaps"},
		{"rw dotfile", Mount{Src: "~/.bashrc", Dst: "/x"}, "dot"},
		{"rw inside dot-dir", Mount{Src: "~/.local/bin", Dst: "/x"}, "dot"},
		{"rw PATH dir", Mount{Src: "~/bin", Dst: "/x"}, "$PATH"},
		{"missing src", Mount{Src: "~/nope", Dst: "/x", RO: true}, "no such file"},
		{"tilde user", Mount{Src: "~root/x", Dst: "/x", RO: true}, "only ~"},
		{"relative src", Mount{Src: "data", Dst: "/x", RO: true}, "absolute"},
		{"colon", Mount{Src: "~/da:ta", Dst: "/x", RO: true}, "not allowed in mount paths"},
		{"comma dst", Mount{Src: "~/data", Dst: "/x,ro", RO: true}, "not allowed in mount paths"},
		{"empty", Mount{Dst: "/x"}, "required"},
		{"dst in project", Mount{Src: "~/data", Dst: "~/src/proj/.git", RO: true}, "shadow"},
		{"dst home", Mount{Src: "~/data", Dst: "~", RO: true}, "home volume"},
		{"dst root", Mount{Src: "~/data", Dst: "/", RO: true}, "home volume"},
		{"dst relative", Mount{Src: "~/data", Dst: "x", RO: true}, "absolute"},
		{"dst cache", Mount{Src: "~/data", Dst: "/cache/go", RO: true}, "reserved"},
		{"dst binary", Mount{Src: "~/data", Dst: "/usr/local/bin/agent-sandbox", RO: true}, "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMount(tt.m, env)
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadResolvesMountsAndNamesFile(t *testing.T) {
	env := mountEnv(t)
	dir := t.TempDir()
	g := writeFile(t, filepath.Join(dir, "global.toml"), `mounts = [{ src = "~/data", dst = "/data", ro = true }]`)
	p := writeFile(t, filepath.Join(dir, "project.toml"), `mounts = [{ src = "~/data", dst = "/data", ro = true }]`)
	c, err := Load(g, p, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Mounts) != 1 || c.Mounts[0].Src != env.Home+"/data" {
		t.Errorf("mounts not resolved and deduplicated: %+v", c.Mounts)
	}

	writeFile(t, p, `mounts = [{ src = "~/.ssh", dst = "/k", ro = true }]`)
	_, err = Load(g, p, env)
	if err == nil || !strings.Contains(err.Error(), "project.toml: mounts[0]") {
		t.Errorf("want error naming project.toml and mounts[0], got %v", err)
	}
}
