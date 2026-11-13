// Package paths resolves the host directories agent-sandbox uses.
package paths

import (
	"errors"
	"path/filepath"
)

// Paths holds the host directories agent-sandbox reads and writes.
type Paths struct {
	Home       string // $HOME
	ConfigHome string // $XDG_CONFIG_HOME, default ~/.config
	DataHome   string // $XDG_DATA_HOME, default ~/.local/share
	XDGRuntime string // $XDG_RUNTIME_DIR
	ConfigDir  string // ConfigHome/agent-sandbox
	DataDir    string // DataHome/agent-sandbox
	RuntimeDir string // XDGRuntime/agent-sandbox
}

// FromEnv builds Paths from environment variables. Relative XDG values are
// ignored, as the XDG base directory spec requires.
//
// AGENT_SANDBOX_CONFIG_DIR and AGENT_SANDBOX_DATA_DIR override only
// agent-sandbox's own directories. Changing XDG_CONFIG_HOME or
// XDG_DATA_HOME instead would also move podman's configuration and image
// store.
func FromEnv(getenv func(string) string) (Paths, error) {
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return Paths{}, errors.New("HOME must be set to an absolute path")
	}
	rt := getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(rt) {
		return Paths{}, errors.New("XDG_RUNTIME_DIR must be set to an absolute path (rootless podman needs it too)")
	}
	home = filepath.Clean(home)
	p := Paths{
		Home:       home,
		ConfigHome: absOr(getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")),
		DataHome:   absOr(getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")),
		XDGRuntime: filepath.Clean(rt),
	}
	p.ConfigDir = absOr(getenv("AGENT_SANDBOX_CONFIG_DIR"), filepath.Join(p.ConfigHome, "agent-sandbox"))
	p.DataDir = absOr(getenv("AGENT_SANDBOX_DATA_DIR"), filepath.Join(p.DataHome, "agent-sandbox"))
	p.RuntimeDir = filepath.Join(p.XDGRuntime, "agent-sandbox")
	return p, nil
}

func absOr(v, def string) string {
	if filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return def
}

// GlobalConfig is the global config file.
func (p Paths) GlobalConfig() string { return filepath.Join(p.ConfigDir, "config.toml") }

// SecretsFile is the env file passed to every sandbox.
func (p Paths) SecretsFile() string { return filepath.Join(p.ConfigDir, "secrets.env") }

// BaseOverrideDir is the optional base image build context.
func (p Paths) BaseOverrideDir() string { return filepath.Join(p.ConfigDir, "base") }

// HomeSeedDir is the optional seed copied into new home volumes.
func (p Paths) HomeSeedDir() string { return filepath.Join(p.ConfigDir, "home") }
