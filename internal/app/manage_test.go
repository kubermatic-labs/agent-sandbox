package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/podman"
)

func TestCountSessions(t *testing.T) {
	ps := []podman.Process{{PID: "1", User: "nobody"}, {PID: "7", User: "65533"}, {PID: "12", User: "kron"}, {PID: "13", User: "kron"}, {PID: "20", User: "root"}}
	if n := countSessions(ps); n != 3 {
		t.Errorf("countSessions = %d, want 3", n)
	}
}

func TestFormatUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42*time.Second + 300*time.Millisecond:       "42s",
		12*time.Minute + 5*time.Second:              "12m0s",
		3*time.Hour + 4*time.Minute + 5*time.Second: "3h4m",
	} {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTemplates(t *testing.T) {
	var m map[string]any
	if _, err := toml.Decode(configTemplate, &m); err != nil || len(m) != 0 {
		t.Errorf("config template must parse to nothing (all commented): %v, %v", m, err)
	}
	if scaffoldConfig("", nil) != configTemplate {
		t.Error("config scaffold without --base must be the template")
	}
	for _, base := range append([]string{""}, config.Bases...) {
		cf := scaffoldContainerfile(base)
		if err := image.CheckProjectContainerfile([]byte(cf)); err != nil {
			t.Errorf("Containerfile template for %q: %v", base, err)
		}
		if strings.Contains(cf, "%!") {
			t.Errorf("Containerfile template for %q is misformatted:\n%s", base, cf)
		}
		if base == "" {
			continue
		}
		var got map[string]any
		if _, err := toml.Decode(scaffoldConfig(base, nil), &got); err != nil || len(got) != 1 || got["base"] != base {
			t.Errorf("config scaffold for %q = %v, %v; want only base set", base, got, err)
		}
	}
	// Every config key is documented in the scaffold (AGENTS.md).
	for _, k := range config.Keys() {
		if !strings.Contains(configTemplate, "\n# "+k+" ") {
			t.Errorf("config template does not mention %q", k)
		}
	}
}

func TestScaffoldConfigCaches(t *testing.T) {
	for _, caches := range [][]string{{"go"}, {"npm", "pip"}, {"go", "npm", "pip"}} {
		var got struct {
			Base   string
			Caches []string
		}
		cfg := scaffoldConfig("alpine:3", caches)
		if _, err := toml.Decode(cfg, &got); err != nil || got.Base != "alpine:3" || !slices.Equal(got.Caches, caches) {
			t.Errorf("config scaffold for caches %q = %+v, %v", caches, got, err)
		}
	}
	if !strings.Contains(scaffoldConfig("", []string{"go", "npm"}), "\ncaches        = [\"go\", \"npm\"]       # go | npm | pip\n") {
		t.Errorf("caches line misaligned:\n%s", scaffoldConfig("", []string{"go", "npm"}))
	}
}

func TestDetectCaches(t *testing.T) {
	tests := []struct {
		files []string
		want  []string
	}{
		{nil, nil},
		{[]string{"go.mod"}, []string{"go"}},
		{[]string{"package.json", "go.mod"}, []string{"go", "npm"}},
		{[]string{"pyproject.toml", "requirements.txt"}, []string{"pip"}},
		{[]string{"go.work", "package.json", "setup.py"}, []string{"go", "npm", "pip"}},
		{[]string{"sub/go.mod", "README.md"}, nil},
	}
	for _, tt := range tests {
		root := t.TempDir()
		for _, f := range tt.files {
			p := filepath.Join(root, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := detectCaches(root); !slices.Equal(got, tt.want) {
			t.Errorf("detectCaches(%q) = %q, want %q", tt.files, got, tt.want)
		}
	}
	// A directory named like a marker is not one.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "package.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectCaches(root); got != nil {
		t.Errorf("detectCaches(dir package.json) = %q", got)
	}
}
