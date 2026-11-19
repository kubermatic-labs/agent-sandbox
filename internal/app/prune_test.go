package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/paths"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/podman/podmantest"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/prune"
	"k8c.io/agent-sandbox/internal/scan"
	"k8c.io/agent-sandbox/internal/trust"
	"k8c.io/agent-sandbox/internal/worktree"
)

// pruneWorld is one project with an exited sandbox, its home volume and
// project image, plus data-dir state. States are what successive ps calls
// report for the sandbox.
type pruneWorld struct {
	e      *env
	fake   *podmantest.Fake
	states []string
}

func newPruneWorld(t *testing.T, states ...string) *pruneWorld {
	t.Helper()
	root := "/src/p"
	id := project.ComputeID(root)
	data := t.TempDir()
	w := &pruneWorld{
		e: &env{
			paths: paths.Paths{DataDir: data, RuntimeDir: t.TempDir()},
			proj:  project.Project{Root: root, ID: id},
		},
		states: states,
	}
	for _, f := range []string{
		filepath.Join(trust.NewStore(data, id).Dir(), "hash"),
		scan.StatePath(data, id),
		filepath.Join(worktree.Path(data, id, "dev"), "file"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := json.Marshal(labels.Container(project.Project{Root: root, ID: id}, "default", "ro", "img1", ""))
	if err != nil {
		t.Fatal(err)
	}
	w.fake = &podmantest.Fake{Handler: func(args []string) podmantest.Result {
		switch args[0] {
		case "ps":
			state := w.states[0]
			if len(w.states) > 1 {
				w.states = w.states[1:]
			}
			return podmantest.Result{Out: fmt.Sprintf(`[{"Id":"c1","Names":["asb-%s-default"],"State":%q,"Labels":%s}]`, id, state, l)}
		case "volume":
			if args[1] == "ls" {
				return podmantest.Result{Out: fmt.Sprintf(`[{"Name":"asb-%s-home","Labels":%s}]`, id, l)}
			}
		case "images":
			return podmantest.Result{Out: fmt.Sprintf(`[{"Id":"img1","RepoTags":["localhost/asb-%s:abc"],"Containers":1},
				{"Id":"img2","RepoTags":["docker.io/library/alpine:3"],"Containers":0}]`, id)}
		}
		return podmantest.Result{}
	}}
	w.e.pm = podman.Client{R: w.fake}
	return w
}

var pruneAll = PruneOptions{Kinds: prune.Kinds{Containers: true, Volumes: true, Images: true, State: true}}

func (w *pruneWorld) removals() [][]string {
	var out [][]string
	for _, c := range w.fake.Calls() {
		switch {
		case c[0] == "rm", c[0] == "stop", c[0] == "rmi", c[0] == "volume" && c[1] == "rm":
			out = append(out, c)
		}
	}
	return out
}

func TestPruneRemovesAfterConfirmation(t *testing.T) {
	w := newPruneWorld(t, "exited")
	var out, log bytes.Buffer
	asked := 0
	err := w.e.prune(context.Background(), pruneAll, &out, &log, func(n int) (bool, error) {
		asked = n
		return true, nil
	})
	if err != nil {
		t.Fatalf("prune: %v\n%s", err, log.String())
	}
	if asked != 5 {
		t.Errorf("confirmation asked for %d items, want 5\n%s", asked, out.String())
	}
	id := w.e.proj.ID
	want := [][]string{
		{"rm", "--force", "--time", "0", "--ignore", "c1"},
		{"volume", "rm", "asb-" + id + "-home"}, // never --force: that would remove containers using it
		{"rmi", "localhost/asb-" + id + ":abc"},
	}
	if diff := cmp.Diff(want, w.removals()); diff != "" {
		t.Errorf("removals (-want +got):\n%s", diff)
	}
	data := w.e.paths.DataDir
	if _, err := os.Stat(trust.NewStore(data, id).Dir()); !os.IsNotExist(err) {
		t.Errorf("trust record still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(scan.StatePath(data, id))); !os.IsNotExist(err) {
		t.Errorf("findings still exist: %v", err)
	}
	if _, err := os.Stat(worktree.Path(data, id, "dev")); err != nil {
		t.Errorf("worktree removed: %v", err)
	}
	for _, s := range []string{"agent state and history", "worktrees are never pruned", "removed volume asb-" + id + "-home"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "alpine") {
		t.Errorf("a foreign image was listed:\n%s", out.String())
	}
}

func TestPruneDryRunAndDecline(t *testing.T) {
	for _, tt := range []struct {
		name    string
		opts    PruneOptions
		answer  bool
		wantErr bool
	}{
		{"dry run", PruneOptions{Kinds: pruneAll.Kinds, DryRun: true}, true, false},
		{"declined", pruneAll, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newPruneWorld(t, "exited")
			var out bytes.Buffer
			err := w.e.prune(context.Background(), tt.opts, &out, &out, func(int) (bool, error) {
				if tt.opts.DryRun {
					t.Error("dry run asked for confirmation")
				}
				return tt.answer, nil
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v", err)
			}
			if r := w.removals(); len(r) != 0 {
				t.Errorf("removed %q", r)
			}
			if _, err := os.Stat(trust.NewStore(w.e.paths.DataDir, w.e.proj.ID).Dir()); err != nil {
				t.Errorf("trust record removed: %v", err)
			}
		})
	}
}

// A sandbox that starts while the user reads the list keeps its container
// and what it uses; the rest of the confirmed list is still removed.
func TestPruneReplansAfterConfirmation(t *testing.T) {
	w := newPruneWorld(t, "exited", "running")
	var out, log bytes.Buffer
	err := w.e.prune(context.Background(), pruneAll, &out, &log, func(int) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if r := w.removals(); len(r) != 0 {
		t.Errorf("removed from a running sandbox: %q", r)
	}
	for _, s := range []string{"kept container", "kept volume", "kept image"} {
		if !strings.Contains(log.String(), s) {
			t.Errorf("log lacks %q:\n%s", s, log.String())
		}
	}
	if _, err := os.Stat(trust.NewStore(w.e.paths.DataDir, w.e.proj.ID).Dir()); !os.IsNotExist(err) {
		t.Errorf("trust record not removed: %v", err)
	}
}

func TestPruneForceStopsRunning(t *testing.T) {
	w := newPruneWorld(t, "running")
	o := PruneOptions{Kinds: prune.Kinds{Containers: true}, Force: true}
	if err := w.e.prune(context.Background(), o, &bytes.Buffer{}, &bytes.Buffer{}, func(int) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"stop", "--ignore", "--time", "10", "c1"}, {"rm", "--force", "--time", "0", "--ignore", "c1"}}
	if diff := cmp.Diff(want, w.removals()); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestPruneDirs(t *testing.T) {
	data := t.TempDir()
	for _, d := range []string{trust.NewStore(data, "a-1").Dir(), filepath.Dir(scan.StatePath(data, "b-2")), filepath.Dir(worktree.Path(data, "c-3", "x"))} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pruneDirs(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []prune.Dir{
		{Kind: prune.Trust, ProjectID: "a-1", Path: trust.NewStore(data, "a-1").Dir()},
		{Kind: prune.Findings, ProjectID: "b-2", Path: filepath.Dir(scan.StatePath(data, "b-2"))},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("an empty worktree dir is not listed (-want +got):\n%s", diff)
	}
}

func TestAskYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "": false} {
		var out bytes.Buffer
		if got, err := askYesNo(strings.NewReader(in), &out, "ok? "); got != want || err != nil || out.String() != "ok? " {
			t.Errorf("askYesNo(%q) = %v, %v", in, got, err)
		}
	}
}

func TestPruneBaseNeedsImages(t *testing.T) {
	err := Prune(context.Background(), PruneOptions{Kinds: prune.Kinds{Volumes: true}, Base: true}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--base") {
		t.Errorf("err = %v", err)
	}
}
