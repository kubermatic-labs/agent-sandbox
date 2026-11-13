package app

import (
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
	if err := image.CheckProjectContainerfile([]byte(containerfileTemplate)); err != nil {
		t.Errorf("Containerfile template: %v", err)
	}
	// Every config key is documented in the scaffold (AGENTS.md).
	for _, k := range config.Keys() {
		if !strings.Contains(configTemplate, "\n# "+k+" ") {
			t.Errorf("config template does not mention %q", k)
		}
	}
}
