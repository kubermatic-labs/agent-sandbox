// Package mounts computes the container mount plan (spec 4.4, 5). It is pure:
// it takes inputs and returns a plan, so the plan can be golden-tested
// without podman. Security-critical.
package mounts

import (
	"fmt"
	"path/filepath"
	"strings"

	"k8c.io/agent-sandbox/internal/config"
)

// Mount is one -v argument.
type Mount struct {
	Src     string // host path, or volume name when Volume
	Dst     string
	Volume  bool
	RO      bool
	Relabel bool // ":z", a one-time shared SELinux relabel
}

// Arg renders the mount as a podman -v value.
func (m Mount) Arg() string {
	var opts []string
	if m.RO {
		opts = append(opts, "ro")
	}
	if m.Relabel {
		opts = append(opts, "z")
	}
	s := m.Src + ":" + m.Dst
	if len(opts) > 0 {
		s += ":" + strings.Join(opts, ",")
	}
	return s
}

// Volume is a named volume that must exist before podman run.
type Volume struct {
	Name string
	Home bool // the home volume, which receives the home seed
}

// BinaryPath is where the agent-sandbox binary is mounted in the container.
const BinaryPath = "/usr/local/bin/agent-sandbox"

// Input is everything the plan depends on.
type Input struct {
	Root   string   // project root, real path
	Deny   []string // existing deny-list paths under Root, relative (ResolveDeny)
	ID     string   // project ID
	Home   string   // $HOME, also the home volume mount point
	Binary string   // staged agent-sandbox binary (selfbin.Stage)
	Config config.Config
}

// Plan is the computed mount plan.
type Plan struct {
	Mounts  []Mount
	Volumes []Volume
	Env     []string // K=V session env set by cache presets
}

// VolumeName returns asb-<project-id>-<suffix>.
func VolumeName(projectID, suffix string) string {
	return "asb-" + projectID + "-" + suffix
}

// Build returns the mount plan. Order matters for readability only: podman
// sorts mounts by destination, so parents are mounted before nested mounts.
func Build(in Input) (Plan, error) {
	if in.Config.Git != "ro" {
		return Plan{}, fmt.Errorf("git mode %q is not implemented yet", in.Config.Git)
	}
	var p Plan
	p.Mounts = append(p.Mounts, Mount{Src: in.Root, Dst: in.Root, Relabel: true})
	// Read-only overlays of the deny-list (spec 5.1, 5.2). They lie under
	// the :z project mount, so they need no relabel of their own.
	for _, rel := range in.Deny {
		path := filepath.Join(in.Root, rel)
		if filepath.IsAbs(rel) || path == in.Root || !strings.HasPrefix(path, in.Root+"/") {
			return Plan{}, fmt.Errorf("deny-list entry %q is outside the project", rel)
		}
		p.Mounts = append(p.Mounts, Mount{Src: path, Dst: path, RO: true})
	}
	for _, m := range in.Config.Mounts {
		p.Mounts = append(p.Mounts, Mount{Src: m.Src, Dst: m.Dst, RO: m.RO, Relabel: true})
	}
	if in.Binary == "" {
		return Plan{}, fmt.Errorf("no agent-sandbox binary to mount")
	}
	p.Mounts = append(p.Mounts, Mount{Src: in.Binary, Dst: BinaryPath, RO: true, Relabel: true})

	home := VolumeName(in.ID, "home")
	p.Volumes = append(p.Volumes, Volume{Name: home, Home: true})
	p.Mounts = append(p.Mounts, Mount{Src: home, Dst: in.Home, Volume: true})

	for _, preset := range in.Config.Caches {
		for _, cv := range config.CachePresets[preset] {
			name := VolumeName(in.ID, cv.Suffix)
			p.Volumes = append(p.Volumes, Volume{Name: name})
			p.Mounts = append(p.Mounts, Mount{Src: name, Dst: cv.Dst, Volume: true})
			p.Env = append(p.Env, cv.EnvVar+"="+cv.Dst)
		}
	}
	return p, nil
}
