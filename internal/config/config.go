// Package config loads, merges and validates agent-sandbox configuration (spec 3.1).
//
// The global file is always trusted; the project file is gated by trust
// (spec 3.4, implemented elsewhere). Each file is validated on its own so
// errors name the file, then the files are merged: project scalars override
// global ones, lists are concatenated and deduplicated, maps are merged with
// the project key winning. command is a list but behaves like a scalar.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Mount is one extra bind mount from the mounts list.
type Mount struct {
	Src string `toml:"src"`
	Dst string `toml:"dst"`
	RO  bool   `toml:"ro"`
}

// Resources are the container resource limits.
type Resources struct {
	Memory string
	CPUs   float64
	PIDs   int
}

// Config is the merged, validated configuration.
type Config struct {
	Command      []string
	Network      string
	AllowCIDRs   []string
	AllowDomains []string
	Git          string
	IdleTimeout  time.Duration
	ReadOnly     []string
	WorktreeCopy []string
	Env          map[string]string
	Mounts       []Mount // src resolved, dst expanded
	Caches       []string
	Resources    Resources
	Strict       bool
}

// Defaults returns the configuration used when no file sets a key.
func Defaults() Config {
	return Config{
		Network:     "restricted",
		Git:         "ro",
		IdleTimeout: 10 * time.Minute,
		Resources:   Resources{Memory: "8g", CPUs: 4, PIDs: 4096},
	}
}

// Env is the host context needed to validate a config.
type Env struct {
	Home        string   // $HOME
	ConfigHome  string   // XDG config base (~/.config)
	DataHome    string   // XDG data base (~/.local/share)
	XDGRuntime  string   // $XDG_RUNTIME_DIR
	DataDir     string   // agent-sandbox data dir
	ProjectRoot string   // real path of the project root
	PathDirs    []string // host $PATH entries
}

// Load reads the global and project config files (either may be missing),
// validates each, and merges them over Defaults.
func Load(globalPath, projectPath string, env Env) (Config, error) {
	c := Defaults()
	for _, p := range []string{globalPath, projectPath} {
		f, err := decodeFile(p)
		if err != nil {
			return Config{}, err
		}
		if f == nil {
			continue
		}
		if err := f.validate(p, env); err != nil {
			return Config{}, err
		}
		c.apply(f)
	}
	return c, nil
}

// file mirrors one TOML file; pointers distinguish unset keys from zero values.
type file struct {
	Command      []string          `toml:"command"`
	Network      *string           `toml:"network"`
	AllowCIDRs   []string          `toml:"allow_cidrs"`
	AllowDomains []string          `toml:"allow_domains"`
	Git          *string           `toml:"git"`
	IdleTimeout  *duration         `toml:"idle_timeout"`
	ReadOnly     []string          `toml:"readonly"`
	WorktreeCopy []string          `toml:"worktree_copy"`
	Env          map[string]string `toml:"env"`
	Mounts       []Mount           `toml:"mounts"`
	Caches       []string          `toml:"caches"`
	Resources    *fileResources    `toml:"resources"`
	Strict       *bool             `toml:"strict"`
}

type fileResources struct {
	Memory *string `toml:"memory"`
	CPUs   *cpus   `toml:"cpus"`
	PIDs   *int    `toml:"pids"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// cpus accepts both integers and floats (cpus = 4, cpus = 1.5).
type cpus float64

func (c *cpus) UnmarshalTOML(v any) error {
	switch n := v.(type) {
	case int64:
		*c = cpus(n)
	case float64:
		*c = cpus(n)
	default:
		return fmt.Errorf("cpus must be a number, got %T", v)
	}
	return nil
}

func decodeFile(path string) (*file, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	return &f, nil
}

func (c *Config) apply(f *file) {
	if f.Command != nil {
		c.Command = slices.Clone(f.Command)
	}
	if f.Network != nil {
		c.Network = *f.Network
	}
	if f.Git != nil {
		c.Git = *f.Git
	}
	if f.IdleTimeout != nil {
		c.IdleTimeout = f.IdleTimeout.Duration
	}
	if f.Strict != nil {
		c.Strict = *f.Strict
	}
	c.AllowCIDRs = appendUnique(c.AllowCIDRs, f.AllowCIDRs...)
	c.AllowDomains = appendUnique(c.AllowDomains, f.AllowDomains...)
	c.ReadOnly = appendUnique(c.ReadOnly, f.ReadOnly...)
	c.WorktreeCopy = appendUnique(c.WorktreeCopy, f.WorktreeCopy...)
	c.Caches = appendUnique(c.Caches, f.Caches...)
	c.Mounts = appendUnique(c.Mounts, f.Mounts...)
	for k, v := range f.Env {
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		c.Env[k] = v
	}
	if r := f.Resources; r != nil {
		if r.Memory != nil {
			c.Resources.Memory = *r.Memory
		}
		if r.CPUs != nil {
			c.Resources.CPUs = float64(*r.CPUs)
		}
		if r.PIDs != nil {
			c.Resources.PIDs = *r.PIDs
		}
	}
}

func appendUnique[T comparable](dst []T, src ...T) []T {
	for _, v := range src {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
