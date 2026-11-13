package paths

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	p, err := FromEnv(envOf(map[string]string{"HOME": "/home/u/", "XDG_RUNTIME_DIR": "/run/user/1000"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Home:       "/home/u",
		ConfigHome: "/home/u/.config",
		DataHome:   "/home/u/.local/share",
		XDGRuntime: "/run/user/1000",
		ConfigDir:  "/home/u/.config/agent-sandbox",
		DataDir:    "/home/u/.local/share/agent-sandbox",
		RuntimeDir: "/run/user/1000/agent-sandbox",
	}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("FromEnv mismatch (-want +got):\n%s", diff)
	}
	if got := p.GlobalConfig(); got != "/home/u/.config/agent-sandbox/config.toml" {
		t.Errorf("GlobalConfig = %q", got)
	}
	if got := p.SecretsFile(); got != "/home/u/.config/agent-sandbox/secrets.env" {
		t.Errorf("SecretsFile = %q", got)
	}
	if got := p.BaseOverrideDir(); got != "/home/u/.config/agent-sandbox/base" {
		t.Errorf("BaseOverrideDir = %q", got)
	}
	if got := p.HomeSeedDir(); got != "/home/u/.config/agent-sandbox/home" {
		t.Errorf("HomeSeedDir = %q", got)
	}
}

func TestFromEnvXDGOverrides(t *testing.T) {
	p, err := FromEnv(envOf(map[string]string{
		"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/1000",
		"XDG_CONFIG_HOME": "/cfg", "XDG_DATA_HOME": "relative/ignored",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/cfg/agent-sandbox" {
		t.Errorf("ConfigDir = %q", p.ConfigDir)
	}
	if p.DataDir != "/home/u/.local/share/agent-sandbox" {
		t.Errorf("DataDir = %q, relative XDG_DATA_HOME must be ignored", p.DataDir)
	}
}

func TestFromEnvErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no HOME":            {"XDG_RUNTIME_DIR": "/run/user/1000"},
		"relative HOME":      {"HOME": "home", "XDG_RUNTIME_DIR": "/run/user/1000"},
		"no XDG_RUNTIME_DIR": {"HOME": "/home/u"},
	} {
		if _, err := FromEnv(envOf(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFromEnvAgentSandboxOverrides(t *testing.T) {
	p, err := FromEnv(envOf(map[string]string{
		"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/1000",
		"AGENT_SANDBOX_CONFIG_DIR": "/t/cfg", "AGENT_SANDBOX_DATA_DIR": "/t/data",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/t/cfg" || p.DataDir != "/t/data" {
		t.Errorf("ConfigDir %q, DataDir %q", p.ConfigDir, p.DataDir)
	}
	if p.ConfigHome != "/home/u/.config" || p.DataHome != "/home/u/.local/share" {
		t.Errorf("the overrides must not move the XDG bases: %q %q", p.ConfigHome, p.DataHome)
	}
	if p.GlobalConfig() != "/t/cfg/config.toml" {
		t.Errorf("GlobalConfig = %q", p.GlobalConfig())
	}
}
