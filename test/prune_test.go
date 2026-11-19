//go:build integration

package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8c.io/agent-sandbox/internal/labels"
)

// volumes returns the names of the project's volumes.
func (e *sandboxEnv) volumes() []string {
	e.t.Helper()
	out, err := exec.Command("podman", "volume", "ls", "-q", "--filter", e.projectFilter()).Output()
	if err != nil {
		e.t.Fatal(err)
	}
	vs := strings.Fields(string(out))
	slices.Sort(vs)
	return vs
}

// decoyVolume creates an unlabelled volume named like the project's own.
func (e *sandboxEnv) decoyVolume() string {
	e.t.Helper()
	name := "asb-" + e.id + "-decoy"
	if out, err := exec.Command("podman", "volume", "create", name).CombinedOutput(); err != nil {
		e.t.Fatalf("podman volume create: %v\n%s", err, out)
	}
	e.t.Cleanup(func() { _ = exec.Command("podman", "volume", "rm", "-f", name).Run() })
	return name
}

func volumeExists(name string) bool {
	return exec.Command("podman", "volume", "exists", name).Run() == nil
}

func TestPruneProject(t *testing.T) {
	e := newProject(t, openNetwork+"caches = [\"go\"]\n")
	e.mustRun(e.root, "", "--", "true")
	decoy := e.decoyVolume()
	wantVolumes := []string{"asb-" + e.id + "-go-build", "asb-" + e.id + "-go-mod", "asb-" + e.id + "-home"}
	if got := e.volumes(); !slices.Equal(got, wantVolumes) {
		t.Fatalf("volumes = %q", got)
	}
	out, err := exec.Command("podman", "volume", "inspect", "--format", `{{index .Labels "`+labels.Project+`"}}`, wantVolumes[0]).Output()
	if err != nil || strings.TrimSpace(string(out)) != e.root {
		t.Errorf("volume project label = %q, %v; want %s", out, err, e.root)
	}

	// Without a terminal, prune refuses unless told not to ask.
	if r := e.run(e.root, "", "prune", "all"); r.code == 0 || !strings.Contains(r.stderr, "--yes") {
		t.Errorf("prune without --yes and without a terminal: exit %d, stderr %q", r.code, r.stderr)
	}

	// The running sandbox keeps itself and its volumes.
	got := e.mustRun(e.root, "", "prune", "all", "-y")
	if _, state := e.container(); state != "running" {
		t.Errorf("sandbox state after prune without --force = %q\n%s", state, got)
	}
	if vs := e.volumes(); !slices.Equal(vs, wantVolumes) {
		t.Errorf("volumes after prune without --force = %q\n%s", vs, got)
	}

	got = e.mustRun(e.root, "", "prune", "all", "-y", "--force")
	if id, _ := e.container(); id != "" {
		t.Errorf("sandbox still exists after prune --force\n%s", got)
	}
	if vs := e.volumes(); len(vs) != 0 {
		t.Errorf("volumes after prune --force = %q\n%s", vs, got)
	}
	if !volumeExists(decoy) {
		t.Error("prune removed an unlabelled volume")
	}
	// The next run starts from scratch.
	e.mustRun(e.root, "", "--", "true")
}

func TestPruneProjectImageAndTrust(t *testing.T) {
	e := newProject(t, openNetwork)
	e.writeSandboxFile("Containerfile", strings.Replace(projectContainerfile, "%s", "prune", 1))
	e.mustTrust()
	e.mustRun(e.root, "", "--", "true")
	tags := e.projectImageTags()
	if len(tags) != 1 {
		t.Fatalf("project image tags = %q", tags)
	}
	out, err := exec.Command("podman", "image", "inspect", "--format",
		`{{index .Labels "`+labels.Kind+`"}} {{index .Labels "`+labels.ProjectID+`"}}`, tags[0]).Output()
	if err != nil || strings.TrimSpace(string(out)) != labels.KindProject+" "+e.id {
		t.Errorf("project image labels = %q, %v", out, err)
	}

	// In use by the running sandbox: kept.
	e.mustRun(e.root, "", "prune", "images", "-y")
	if got := e.projectImageTags(); len(got) != 1 {
		t.Errorf("prune removed an image in use: %q", got)
	}

	e.mustRun(e.root, "", "stop")
	e.mustRun(e.root, "", "prune", "images", "state", "-y")
	if got := e.projectImageTags(); len(got) != 0 {
		t.Errorf("project image tags after prune = %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.dataHome, "trust", e.id)); !os.IsNotExist(err) {
		t.Errorf("trust record after prune state: %v", err)
	}
	// The approval is gone, so the config must be approved again.
	if r := e.run(e.root, "", "--", "true"); r.code == 0 || !strings.Contains(r.stderr, "agent-sandbox trust") {
		t.Errorf("run after prune state: exit %d, stderr %q", r.code, r.stderr)
	}
}

// Global scopes run as --dry-run only: a real run would touch the user's
// other projects.
func TestPruneOrphanedDryRun(t *testing.T) {
	gone := newProject(t, openNetwork)
	live := newProject(t, openNetwork)
	gone.mustRun(gone.root, "", "--", "true")
	live.mustRun(live.root, "", "--", "true")
	gone.mustRun(gone.root, "", "stop")
	if err := os.RemoveAll(gone.root); err != nil {
		t.Fatal(err)
	}

	out := live.mustRun(live.root, "", "prune", "all", "--orphaned", "--dry-run")
	if !strings.Contains(out, "asb-"+gone.id+"-home") {
		t.Errorf("the deleted project's volume is not listed:\n%s", out)
	}
	if strings.Contains(out, live.id) {
		t.Errorf("the live project is listed:\n%s", out)
	}
	if len(gone.volumes()) == 0 {
		t.Error("--dry-run removed something")
	}

	// From outside any project, too.
	out = live.mustRun(os.TempDir(), "", "prune", "volumes", "--all-projects", "--dry-run")
	for _, id := range []string{gone.id, live.id} {
		if !strings.Contains(out, "asb-"+id+"-home") {
			t.Errorf("--all-projects does not list %s:\n%s", id, out)
		}
	}
}
