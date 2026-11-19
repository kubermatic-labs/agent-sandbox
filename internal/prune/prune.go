// Package prune plans the removal of what agent-sandbox leaves behind:
// containers, volumes, images and data-dir state (spec 2.5).
//
// It is pure: the caller lists what exists and carries out the plan, so
// --dry-run shows exactly what a real run removes. The plan only ever names
// objects labelled as agent-sandbox's (labels.Owned), image tags in agent-sandbox's own
// repositories, and directories in agent-sandbox's data dir. It never
// removes something a container still uses, and never removes worktrees.
package prune

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
)

// Kinds selects what to remove.
type Kinds struct {
	Containers bool
	Volumes    bool
	Images     bool
	State      bool // trust records and scan findings
}

// Scope selects whose resources to remove.
type Scope int

const (
	// ScopeProject is one project.
	ScopeProject Scope = iota
	// ScopeOrphaned is every project whose root directory no longer exists.
	ScopeOrphaned
	// ScopeAll is every project.
	ScopeAll
)

// Kind is the kind of an Item.
type Kind string

// Item kinds.
const (
	Container Kind = "container"
	Volume    Kind = "volume"
	Image     Kind = "image"
	Trust     Kind = "trust"     // trust record: the approved snapshot and its hash
	Findings  Kind = "findings"  // post-session scan findings
	Worktrees Kind = "worktrees" // a project's worktree dir: listed, never removed
)

// Dir is a per-project directory in the data dir.
type Dir struct {
	Kind      Kind // Trust, Findings or Worktrees
	ProjectID string
	Path      string
}

// Inventory is what exists.
type Inventory struct {
	Containers []podman.Container // every container matching labels.Owned
	Volumes    []podman.Volume    // every volume matching labels.Owned
	Images     []podman.Image     // every local image
	Dirs       []Dir
}

// Options select what to remove.
type Options struct {
	Kinds
	Scope     Scope
	ProjectID string // ScopeProject
	Base      bool   // also base images, which every project shares
	Force     bool   // also running containers, which are stopped
	// Exists reports whether a project root still exists (ScopeOrphaned).
	// When in doubt it must report true.
	Exists func(root string) bool
}

// Item is one thing to remove or keep.
type Item struct {
	Kind      Kind
	Ref       string // what podman or the filesystem removes: container ID, volume name, image tag or ID, path
	Name      string // shown to the user
	ProjectID string // "" for base images
	Project   string // project root, "" if unknown
	Stop      bool   // a running container: stop it first
	Reason    string // why a kept item stays
}

// Plan is the outcome of Make.
type Plan struct {
	Remove []Item // in removal order: containers, volumes, images, state
	Keep   []Item // selected but staying, with a reason
	// Unknown are project IDs whose root is not recorded on anything left
	// of them. ScopeOrphaned cannot tell whether they still exist, so it
	// keeps their resources.
	Unknown []string
}

// Make plans what to remove.
func Make(inv Inventory, o Options) Plan {
	roots := projectRoots(inv)
	exists := map[string]bool{}
	unknown := map[string]bool{}
	selected := func(id string) bool {
		if id == "" {
			return false
		}
		switch o.Scope {
		case ScopeProject:
			return id == o.ProjectID
		case ScopeAll:
			return true
		}
		root, ok := roots[id]
		if !ok {
			unknown[id] = true
			return false
		}
		e, ok := exists[root]
		if !ok {
			e = o.Exists(root)
			exists[root] = e
		}
		return !e
	}
	var p Plan
	item := func(kind Kind, ref, name, id string) Item {
		return Item{Kind: kind, Ref: ref, Name: name, ProjectID: id, Project: roots[id]}
	}

	// Containers. The ones that stay keep their project's volumes, and
	// images count as in use until every container using them is removed.
	remaining := map[string]int{} // project ID -> containers that stay
	removedFrom := map[string]int{}
	for _, c := range sortedBy(inv.Containers, func(c podman.Container) string { return containerName(c) }) {
		id := c.Labels[labels.ProjectID]
		if !selected(id) {
			continue
		}
		it := item(Container, c.ID, containerName(c), id)
		active := !stopped(c.State)
		switch {
		case !o.Containers:
			remaining[id]++
		case active && !o.Force:
			it.Reason = c.State + "; --force stops it, ending its sessions"
			p.Keep = append(p.Keep, it)
			remaining[id]++
		default:
			it.Stop = active
			p.Remove = append(p.Remove, it)
			removedFrom[imageKey(c.Labels[labels.ImageID])]++
		}
	}

	if o.Volumes {
		for _, v := range sortedBy(inv.Volumes, func(v podman.Volume) string { return v.Name }) {
			id := v.Labels[labels.ProjectID]
			if !selected(id) {
				continue
			}
			it := item(Volume, v.Name, v.Name, id)
			if remaining[id] > 0 {
				it.Reason = "a sandbox of this project still exists"
				if !o.Containers {
					it.Reason += "; prune containers too"
				}
				p.Keep = append(p.Keep, it)
				continue
			}
			p.Remove = append(p.Remove, it)
		}
	}

	if o.Images {
		var remove, keep []Item
		for _, img := range inv.Images {
			var its []Item
			tags := realTags(img.RepoTags)
			for _, t := range tags {
				switch repo := repository(t); {
				case repo == image.BaseRepo:
					if o.Base {
						its = append(its, item(Image, t, t, ""))
					}
				case strings.HasPrefix(repo, image.ProjectRepoPrefix):
					if id := strings.TrimPrefix(repo, image.ProjectRepoPrefix); selected(id) {
						its = append(its, item(Image, t, t, id))
					}
				}
			}
			// An untagged image is ours only by its labels: a newer build
			// took the tag (build --pull keeps the tag, not the image).
			if len(tags) == 0 && labels.IsOwned(img.Labels) {
				name := "<untagged> " + shortID(img.ID)
				switch img.Labels[labels.Kind] {
				case labels.KindBase:
					if o.Base {
						its = append(its, item(Image, img.ID, name, ""))
					}
				case labels.KindProject:
					if id := img.Labels[labels.ProjectID]; selected(id) {
						its = append(its, item(Image, img.ID, name, id))
					}
				}
			}
			inUse := img.Containers - removedFrom[imageKey(img.ID)]
			for _, it := range its {
				if inUse > 0 {
					it.Reason = "used by a container"
					keep = append(keep, it)
				} else {
					remove = append(remove, it)
				}
			}
		}
		p.Remove = append(p.Remove, sortItems(remove)...)
		p.Keep = append(p.Keep, sortItems(keep)...)
	}

	if o.State {
		dirs := sortedBy(inv.Dirs, func(d Dir) string { return d.Path })
		for _, kind := range []Kind{Trust, Findings, Worktrees} {
			for _, d := range dirs {
				if d.Kind != kind || !selected(d.ProjectID) {
					continue
				}
				it := item(d.Kind, d.Path, d.Path, d.ProjectID)
				if d.Kind == Worktrees {
					it.Reason = "worktrees are never pruned: they may hold uncommitted work"
					p.Keep = append(p.Keep, it)
					continue
				}
				p.Remove = append(p.Remove, it)
			}
		}
	}

	if o.Scope == ScopeOrphaned {
		p.Unknown = slices.Sorted(func(yield func(string) bool) {
			for id := range unknown {
				if !yield(id) {
					return
				}
			}
		})
	}
	return p
}

// projectRoots maps project IDs to roots, from the labels that record both.
// A root that does not hash to its ID is ignored.
func projectRoots(inv Inventory) map[string]string {
	roots := map[string]string{}
	add := func(l map[string]string) {
		id, root := l[labels.ProjectID], l[labels.Project]
		if id != "" && filepath.IsAbs(root) && project.ComputeID(root) == id {
			roots[id] = root
		}
	}
	for _, c := range inv.Containers {
		add(c.Labels)
	}
	for _, v := range inv.Volumes {
		add(v.Labels)
	}
	for _, img := range inv.Images {
		add(img.Labels)
	}
	return roots
}

// stopped reports whether a container in this state runs nothing, so
// removing it ends no session.
func stopped(state string) bool {
	switch state {
	case "created", "configured", "exited", "stopped":
		return true
	}
	return false
}

func containerName(c podman.Container) string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return shortID(c.ID)
}

// realTags drops podman's placeholder for a missing tag.
func realTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t != "" && !strings.HasPrefix(t, "<none>") {
			out = append(out, t)
		}
	}
	return out
}

// repository returns the repository of an image tag (repo:tag).
func repository(tag string) string {
	if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
		return tag[:i]
	}
	return tag
}

// imageKey normalizes an image ID: labels and listings may differ in the
// sha256: prefix.
func imageKey(id string) string {
	return strings.TrimPrefix(id, "sha256:")
}

func shortID(id string) string {
	id = imageKey(id)
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func sortedBy[T any](s []T, key func(T) string) []T {
	return slices.SortedStableFunc(slices.Values(s), func(a, b T) int { return cmp.Compare(key(a), key(b)) })
}

func sortItems(its []Item) []Item {
	slices.SortStableFunc(its, func(a, b Item) int {
		return cmp.Or(cmp.Compare(a.ProjectID, b.ProjectID), cmp.Compare(a.Name, b.Name))
	})
	return its
}
