package prune

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
)

var (
	liveRoot, goneRoot = "/src/live", "/src/gone"
	live, gone         = project.ComputeID(liveRoot), project.ComputeID(goneRoot)
	legacy             = "legacy-0badc0de" // volumes from before the root label
)

func volLabels(root string) map[string]string {
	return labels.Volume(project.Project{Root: root, ID: project.ComputeID(root)})
}

func ctr(root, name, state, imageID string) podman.Container {
	l := labels.Container(project.Project{Root: root, ID: project.ComputeID(root)}, name, "ro", imageID, "")
	n := "asb-" + project.ComputeID(root) + "-" + name
	return podman.Container{ID: "id-" + n, Names: []string{n}, State: state, Labels: l}
}

// inventory: live runs "default" (from image p1) and has an exited "dev";
// gone has an exited "default" (from image g1). Plus a legacy volume
// without a root label, the shared base image and someone else's images.
func inventory() Inventory {
	base := "localhost/agent-sandbox-base:aaaa"
	return Inventory{
		Containers: []podman.Container{
			ctr(liveRoot, "default", "running", "sha256:p1"),
			ctr(liveRoot, "dev", "exited", "p1"),
			ctr(goneRoot, "default", "exited", "g1"),
		},
		Volumes: []podman.Volume{
			{Name: "asb-" + live + "-home", Labels: volLabels(liveRoot)},
			{Name: "asb-" + gone + "-home", Labels: volLabels(goneRoot)},
			{Name: "asb-" + gone + "-go-mod", Labels: volLabels(goneRoot)},
			{Name: "asb-" + legacy + "-home", Labels: map[string]string{labels.Managed: labels.ManagedValue, labels.ProjectID: legacy}},
		},
		Images: []podman.Image{
			{ID: "b1", RepoTags: []string{base}, Containers: 0, Labels: labels.BaseImage()},
			{ID: "b0", RepoTags: []string{"<none>:<none>"}, Labels: labels.BaseImage()},
			{ID: "p1", RepoTags: []string{"localhost/asb-" + live + ":1111"}, Containers: 2},
			{ID: "p0", RepoTags: []string{"localhost/asb-" + live + ":0000"}},
			{ID: "g1", RepoTags: []string{"localhost/asb-" + gone + ":2222", base}, Containers: 1},
			{ID: "g0", Labels: labels.ProjectImage(goneRoot, gone)},
			{ID: "l0", RepoTags: []string{"localhost/asb-" + legacy + ":3333"}},
			{ID: "x1", RepoTags: []string{"docker.io/library/alpine:3"}},
			{ID: "x2", Labels: map[string]string{"maintainer": "someone"}},
			{ID: "x3", RepoTags: []string{"localhost/mine:latest"}, Labels: labels.BaseImage()},
		},
		Dirs: []Dir{
			{Kind: Trust, ProjectID: live, Path: "/d/trust/" + live},
			{Kind: Trust, ProjectID: gone, Path: "/d/trust/" + gone},
			{Kind: Findings, ProjectID: gone, Path: "/d/state/" + gone},
			{Kind: Worktrees, ProjectID: gone, Path: "/d/worktrees/" + gone},
			{Kind: Trust, ProjectID: legacy, Path: "/d/trust/" + legacy},
		},
	}
}

func exists(root string) bool { return root != goneRoot }

// render lists items as "kind name" plus " [stop]" or ": reason".
func render(its []Item) []string {
	out := []string{}
	for _, it := range its {
		s := fmt.Sprintf("%s %s", it.Kind, it.Name)
		if it.Stop {
			s += " [stop]"
		}
		if it.Reason != "" {
			s += ": " + it.Reason
		}
		out = append(out, s)
	}
	return out
}

func TestMake(t *testing.T) {
	all := Kinds{Containers: true, Volumes: true, Images: true, State: true}
	tests := []struct {
		name       string
		opts       Options
		remove     []string
		keep       []string
		wantUnkown []string
	}{
		{
			name:   "project containers: running one stays",
			opts:   Options{Kinds: Kinds{Containers: true}, ProjectID: live},
			remove: []string{"container asb-" + live + "-dev"},
			keep:   []string{"container asb-" + live + "-default: running; --force stops it, ending its sessions"},
		},
		{
			name:   "project containers forced",
			opts:   Options{Kinds: Kinds{Containers: true}, ProjectID: live, Force: true},
			remove: []string{"container asb-" + live + "-default [stop]", "container asb-" + live + "-dev"},
		},
		{
			name: "project volumes stay while a sandbox exists",
			opts: Options{Kinds: Kinds{Volumes: true}, ProjectID: live},
			keep: []string{"volume asb-" + live + "-home: a sandbox of this project still exists; prune containers too"},
		},
		{
			name: "project all: running sandbox keeps volumes and its image",
			opts: Options{Kinds: all, ProjectID: live},
			remove: []string{
				"container asb-" + live + "-dev",
				"image localhost/asb-" + live + ":0000",
				"trust /d/trust/" + live,
			},
			keep: []string{
				"container asb-" + live + "-default: running; --force stops it, ending its sessions",
				"volume asb-" + live + "-home: a sandbox of this project still exists",
				"image localhost/asb-" + live + ":1111: used by a container",
			},
		},
		{
			name: "project all forced: base image stays without --base",
			opts: Options{Kinds: all, ProjectID: live, Force: true},
			remove: []string{
				"container asb-" + live + "-default [stop]",
				"container asb-" + live + "-dev",
				"volume asb-" + live + "-home",
				"image localhost/asb-" + live + ":0000",
				"image localhost/asb-" + live + ":1111",
				"trust /d/trust/" + live,
			},
		},
		{
			name:   "base images only with --base, and not while used",
			opts:   Options{Kinds: Kinds{Images: true}, ProjectID: "none", Base: true},
			remove: []string{"image <untagged> b0", "image localhost/agent-sandbox-base:aaaa"},
			keep:   []string{"image localhost/agent-sandbox-base:aaaa: used by a container"},
		},
		{
			name: "orphaned: only the deleted project; worktrees stay; legacy is unknown",
			opts: Options{Kinds: all, Scope: ScopeOrphaned},
			remove: []string{
				"container asb-" + gone + "-default",
				"volume asb-" + gone + "-go-mod",
				"volume asb-" + gone + "-home",
				"image <untagged> g0",
				"image localhost/asb-" + gone + ":2222",
				"trust /d/trust/" + gone,
				"findings /d/state/" + gone,
			},
			keep:       []string{"worktrees /d/worktrees/" + gone + ": worktrees are never pruned: they may hold uncommitted work"},
			wantUnkown: []string{legacy},
		},
		{
			name: "all projects images: everything ours that is unused, never foreign images",
			opts: Options{Kinds: Kinds{Images: true}, Scope: ScopeAll, Base: true},
			remove: []string{
				"image <untagged> b0",
				"image localhost/agent-sandbox-base:aaaa",
				"image <untagged> g0",
				"image localhost/asb-" + legacy + ":3333",
				"image localhost/asb-" + live + ":0000",
			},
			keep: []string{
				"image localhost/agent-sandbox-base:aaaa: used by a container",
				"image localhost/asb-" + gone + ":2222: used by a container",
				"image localhost/asb-" + live + ":1111: used by a container",
			},
		},
		{
			name: "all projects volumes: legacy too",
			opts: Options{Kinds: Kinds{Containers: true, Volumes: true}, Scope: ScopeAll},
			remove: []string{
				"container asb-" + gone + "-default",
				"container asb-" + live + "-dev",
				"volume asb-" + gone + "-go-mod",
				"volume asb-" + gone + "-home",
				"volume asb-" + legacy + "-home",
			},
			keep: []string{
				"container asb-" + live + "-default: running; --force stops it, ending its sessions",
				"volume asb-" + live + "-home: a sandbox of this project still exists",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Exists = exists
			p := Make(inventory(), tt.opts)
			if diff := cmp.Diff(append([]string{}, tt.remove...), render(p.Remove)); diff != "" {
				t.Errorf("remove (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(append([]string{}, tt.keep...), render(p.Keep)); diff != "" {
				t.Errorf("keep (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantUnkown, p.Unknown); diff != "" {
				t.Errorf("unknown (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMakeRefsAndProjects(t *testing.T) {
	p := Make(inventory(), Options{Kinds: Kinds{Containers: true, Images: true}, Scope: ScopeOrphaned, Exists: exists})
	want := []Item{
		{Kind: Container, Ref: "id-asb-" + gone + "-default", Name: "asb-" + gone + "-default", ProjectID: gone, Project: goneRoot},
		{Kind: Image, Ref: "g0", Name: "<untagged> g0", ProjectID: gone, Project: goneRoot},
		{Kind: Image, Ref: "localhost/asb-" + gone + ":2222", Name: "localhost/asb-" + gone + ":2222", ProjectID: gone, Project: goneRoot},
	}
	if diff := cmp.Diff(want, p.Remove); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// A root label that does not hash to the project ID is not trusted: the
// project stays unknown rather than being judged by another path.
func TestMakeIgnoresMismatchedRoot(t *testing.T) {
	inv := Inventory{Volumes: []podman.Volume{{Name: "asb-" + live + "-home", Labels: map[string]string{
		labels.Managed: labels.ManagedValue, labels.Project: goneRoot, labels.ProjectID: live,
	}}}}
	p := Make(inv, Options{Kinds: Kinds{Volumes: true}, Scope: ScopeOrphaned, Exists: exists})
	if len(p.Remove) != 0 || len(p.Unknown) != 1 {
		t.Errorf("plan = %+v", p)
	}
}
