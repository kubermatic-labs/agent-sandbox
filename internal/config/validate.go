package config

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	networks = []string{"restricted", "open", "allowlist"}
	gitModes = []string{"ro", "worktree"}
)

// CacheVolume is one named volume a cache preset mounts.
type CacheVolume struct {
	Suffix string // volume name is asb-<project-id>-<Suffix>
	Dst    string // mount point in the container
	EnvVar string // session env var pointing at Dst
}

// CachePresets maps the caches config values to their volumes (spec 3.5).
var CachePresets = map[string][]CacheVolume{
	"go": {
		{Suffix: "go-mod", Dst: "/cache/go-mod", EnvVar: "GOMODCACHE"},
		{Suffix: "go-build", Dst: "/cache/go-build", EnvVar: "GOCACHE"},
	},
	"npm": {{Suffix: "npm", Dst: "/cache/npm", EnvVar: "npm_config_cache"}},
	"pip": {{Suffix: "pip", Dst: "/cache/pip", EnvVar: "PIP_CACHE_DIR"}},
}

var (
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	memoryRE = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)
	domainRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// reservedEnv reports whether config env may not set key: agent-sandbox sets
// these itself, and letting config override them would undo its settings.
func reservedEnv(key string) bool {
	if strings.HasPrefix(key, "GIT_CONFIG") {
		return true
	}
	switch key {
	case "HOME", "PATH", "USER", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy":
		return true
	}
	for _, vols := range CachePresets {
		for _, v := range vols {
			if v.EnvVar == key {
				return true
			}
		}
	}
	return false
}

// ParseCIDR parses a CIDR or a bare IP address (as a single-host prefix).
func ParseCIDR(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// checkRelPath validates a project-relative path (readonly, worktree_copy).
func checkRelPath(p string) error {
	if p == "" || filepath.IsAbs(p) {
		return fmt.Errorf("%q must be a non-empty relative path", p)
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q must stay inside the project", p)
	}
	return nil
}

// validate checks one file and resolves its mounts in place.
func (f *file) validate(path string, env Env) error {
	var errs []error
	bad := func(key, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s: %s", path, key, fmt.Sprintf(format, a...)))
	}

	if f.Command != nil && len(f.Command) == 0 {
		bad("command", "must not be empty")
	}
	if f.Network != nil && !slices.Contains(networks, *f.Network) {
		bad("network", "%q is not one of %s", *f.Network, strings.Join(networks, ", "))
	}
	if f.Git != nil && !slices.Contains(gitModes, *f.Git) {
		bad("git", "%q is not one of %s", *f.Git, strings.Join(gitModes, ", "))
	}
	if f.IdleTimeout != nil && f.IdleTimeout.Duration <= 0 {
		bad("idle_timeout", "must be positive")
	}
	for _, c := range f.Caches {
		if _, ok := CachePresets[c]; !ok {
			bad("caches", "unknown preset %q (known: go, npm, pip)", c)
		}
	}
	for _, s := range f.AllowCIDRs {
		if _, err := ParseCIDR(s); err != nil {
			bad("allow_cidrs", "%q: %v", s, err)
		}
	}
	for _, d := range f.AllowDomains {
		if !domainRE.MatchString(d) {
			bad("allow_domains", "%q is not a lowercase domain or *.domain", d)
		}
	}
	for _, p := range f.ReadOnly {
		if err := checkRelPath(p); err != nil {
			bad("readonly", "%v", err)
		}
	}
	for _, p := range f.WorktreeCopy {
		if err := checkRelPath(p); err != nil {
			bad("worktree_copy", "%v", err)
		}
	}
	for k := range f.Env {
		switch {
		case !envKeyRE.MatchString(k):
			bad("env", "%q is not a valid variable name", k)
		case reservedEnv(k):
			bad("env", "%q is reserved; agent-sandbox sets it", k)
		}
	}
	if r := f.Resources; r != nil {
		if r.Memory != nil && !memoryRE.MatchString(*r.Memory) {
			bad("resources.memory", "%q is not a size like 8g or 512m", *r.Memory)
		}
		if r.CPUs != nil && *r.CPUs <= 0 {
			bad("resources.cpus", "must be positive")
		}
		if r.PIDs != nil && *r.PIDs <= 0 {
			bad("resources.pids", "must be positive")
		}
	}
	for i, m := range f.Mounts {
		resolved, err := resolveMount(m, env)
		if err != nil {
			bad(fmt.Sprintf("mounts[%d]", i), "%v", err)
			continue
		}
		f.Mounts[i] = resolved
	}
	return errors.Join(errs...)
}
