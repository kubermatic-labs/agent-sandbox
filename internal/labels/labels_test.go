package labels

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/project"
)

// The labels are persisted on podman objects, so this test spells them out
// literally: changing one orphans everything an older version created.
func TestLabels(t *testing.T) {
	p := project.Project{Root: "/src/p", ID: "p-1"}
	for name, tc := range map[string]struct {
		got, want map[string]string
	}{
		"owned": {Owned(), map[string]string{"agent-sandbox": "1"}},
		"container": {Container(p, "dev", "worktree", "img", "bin"), map[string]string{
			"agent-sandbox":            "1",
			"agent-sandbox.project":    "/src/p",
			"agent-sandbox.project-id": "p-1",
			"agent-sandbox.name":       "dev",
			"agent-sandbox.image-id":   "img",
			"agent-sandbox.binary":     "bin",
			"agent-sandbox.git-mode":   "worktree",
		}},
		"lookup": {Lookup("p-1", "dev"), map[string]string{"agent-sandbox.project-id": "p-1", "agent-sandbox.name": "dev"}},
		"volume": {Volume(p), map[string]string{
			"agent-sandbox": "1", "agent-sandbox.project": "/src/p", "agent-sandbox.project-id": "p-1",
		}},
		"base image": {BaseImage(), map[string]string{"agent-sandbox": "1", "agent-sandbox.kind": "base"}},
		"project image": {ProjectImage("/src/p", "p-1"), map[string]string{
			"agent-sandbox": "1", "agent-sandbox.kind": "project",
			"agent-sandbox.project": "/src/p", "agent-sandbox.project-id": "p-1",
		}},
	} {
		if diff := cmp.Diff(tc.want, tc.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", name, diff)
		}
	}
	if Config != "agent-sandbox.config" {
		t.Errorf("Config = %q", Config)
	}
}

func TestIsOwned(t *testing.T) {
	for l, want := range map[string]bool{"1": true, "0": false, "": false} {
		if got := IsOwned(map[string]string{"agent-sandbox": l}); got != want {
			t.Errorf("IsOwned(agent-sandbox=%q) = %v", l, got)
		}
	}
}

func TestArgs(t *testing.T) {
	got := Args(map[string]string{"b": "2", "a": "1"})
	if diff := cmp.Diff([]string{"--label", "a=1", "--label", "b=2"}, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
