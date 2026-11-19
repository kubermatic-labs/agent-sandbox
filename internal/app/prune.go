package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"k8c.io/agent-sandbox/internal/flock"
	"k8c.io/agent-sandbox/internal/labels"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/paths"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/prune"
	"k8c.io/agent-sandbox/internal/scan"
	"k8c.io/agent-sandbox/internal/trust"
	"k8c.io/agent-sandbox/internal/worktree"
)

// PruneOptions are the prune command's arguments.
type PruneOptions struct {
	prune.Kinds
	Scope  prune.Scope
	Base   bool // also base images, which every project shares
	Force  bool // also running containers, ending their sessions
	DryRun bool // only list what would be removed
	Yes    bool // do not ask before removing
}

// Prune removes agent-sandbox's containers, volumes, images and data-dir
// state, of this project, of deleted projects or of all projects (spec 2.5).
func Prune(ctx context.Context, o PruneOptions, out io.Writer) error {
	if o.Base && !o.Images {
		return errors.New("--base removes base images; add images (or all)")
	}
	if !o.DryRun && !o.Yes && !interactive() {
		return errors.New("prune asks before removing anything, which needs a terminal; pass --yes, or --dry-run to only list")
	}
	var e *env
	var err error
	if o.Scope == prune.ScopeProject {
		e, err = resolve()
	} else {
		// Other projects' resources: this may run outside any project.
		var p paths.Paths
		p, err = paths.FromEnv(os.Getenv)
		e = &env{paths: p, pm: podman.Client{R: podman.CLI{}}}
	}
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	confirm := func(n int) (bool, error) {
		if o.Yes {
			return true, nil
		}
		return askYesNo(os.Stdin, os.Stderr, fmt.Sprintf("Remove these %d? [y/N] ", n))
	}
	return e.prune(ctx, o, out, os.Stderr, confirm)
}

// prune lists and plans without the prune lock, so a pending confirmation
// does not hold up sandbox starts. After confirmation it takes the lock,
// plans again and removes what was both listed and is still removable:
// a sandbox started in the meantime keeps what it uses.
func (e *env) prune(ctx context.Context, o PruneOptions, out, log io.Writer, confirm func(n int) (bool, error)) error {
	opts := prune.Options{Kinds: o.Kinds, Scope: o.Scope, ProjectID: e.proj.ID, Base: o.Base, Force: o.Force, Exists: rootExists}
	inv, err := e.pruneInventory(ctx)
	if err != nil {
		return err
	}
	plan := prune.Make(inv, opts)
	if err := printPlan(out, plan, o.DryRun); err != nil {
		return err
	}
	if len(plan.Remove) == 0 || o.DryRun {
		return nil
	}
	ok, err := confirm(len(plan.Remove))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("nothing removed")
	}

	unlock, ok, err := flock.TryLock(e.paths.PruneLock())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(log, "agent-sandbox: waiting for other agent-sandbox commands (sandbox starts, builds, trust prompts) to finish")
		if unlock, err = flock.Lock(e.paths.PruneLock()); err != nil {
			return err
		}
	}
	defer unlock()
	if inv, err = e.pruneInventory(ctx); err != nil {
		return err
	}
	now := map[string]prune.Item{}
	for _, it := range prune.Make(inv, opts).Remove {
		now[itemKey(it)] = it
	}
	failed := 0
	for _, listed := range plan.Remove {
		it, ok := now[itemKey(listed)]
		if !ok {
			fmt.Fprintf(log, "agent-sandbox: kept %s %s: it changed since it was listed\n", listed.Kind, listed.Name)
			continue
		}
		if err := e.pruneItem(ctx, it); err != nil {
			fmt.Fprintf(log, "agent-sandbox: could not remove %s %s: %v\n", it.Kind, it.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "removed %s %s\n", it.Kind, it.Name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d could not be removed", failed, len(plan.Remove))
	}
	return nil
}

// pruneInventory lists everything prune may remove.
func (e *env) pruneInventory(ctx context.Context) (prune.Inventory, error) {
	var inv prune.Inventory
	var err error
	if inv.Containers, err = e.pm.ListContainers(ctx, labels.Owned()); err != nil {
		return inv, err
	}
	if inv.Volumes, err = e.pm.ListVolumes(ctx, labels.Owned()); err != nil {
		return inv, err
	}
	if inv.Images, err = e.pm.ListImages(ctx); err != nil {
		return inv, err
	}
	inv.Dirs, err = pruneDirs(e.paths.DataDir)
	return inv, err
}

// pruneDirs lists the per-project directories in the data dir. Empty
// worktree dirs are left out: there is nothing to warn about.
func pruneDirs(dataDir string) ([]prune.Dir, error) {
	var dirs []prune.Dir
	for _, src := range []struct {
		kind prune.Kind
		dir  string
	}{
		{prune.Trust, trust.StoresDir(dataDir)},
		{prune.Findings, scan.StatesDir(dataDir)},
		{prune.Worktrees, worktree.ProjectsDir(dataDir)},
	} {
		entries, err := os.ReadDir(src.dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			path := filepath.Join(src.dir, ent.Name())
			if src.kind == prune.Worktrees {
				if sub, err := os.ReadDir(path); err == nil && len(sub) == 0 {
					continue
				}
			}
			dirs = append(dirs, prune.Dir{Kind: src.kind, ProjectID: ent.Name(), Path: path})
		}
	}
	return dirs, nil
}

func (e *env) pruneItem(ctx context.Context, it prune.Item) error {
	switch it.Kind {
	case prune.Container:
		if it.Stop {
			if err := e.pm.Stop(ctx, it.Ref, 10); err != nil {
				return err
			}
		}
		return e.pm.Remove(ctx, it.Ref)
	case prune.Volume:
		return e.pm.VolumeRemoveUnused(ctx, it.Ref)
	case prune.Image:
		return e.pm.ImageRemove(ctx, it.Ref)
	case prune.Trust:
		return trust.NewStore(e.paths.DataDir, it.ProjectID).Forget()
	case prune.Findings:
		return os.RemoveAll(it.Ref)
	}
	return fmt.Errorf("prune cannot remove a %s", it.Kind)
}

func itemKey(it prune.Item) string {
	return string(it.Kind) + "\x00" + it.Ref
}

// rootExists reports whether a project root still exists; anything but a
// clear "does not exist" counts as existing.
func rootExists(root string) bool {
	_, err := os.Lstat(root)
	return !errors.Is(err, fs.ErrNotExist)
}

func printPlan(w io.Writer, p prune.Plan, dryRun bool) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	home := false
	if len(p.Remove) == 0 {
		fmt.Fprintln(tw, "nothing to remove")
	} else {
		if dryRun {
			fmt.Fprintln(tw, "Would remove:")
		} else {
			fmt.Fprintln(tw, "To remove:")
		}
		for _, it := range p.Remove {
			name := it.Name
			switch {
			case it.Stop:
				name += " (running: stopped first, ending its sessions)"
			case it.Kind == prune.Volume && it.Name == mounts.VolumeName(it.ProjectID, "home"):
				name += " (agent state and history)"
				home = true
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", it.Kind, name, projectOf(it))
		}
	}
	if len(p.Keep) > 0 {
		fmt.Fprintln(tw, "Kept:")
		for _, it := range p.Keep {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", it.Kind, it.Name, projectOf(it), it.Reason)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if home {
		fmt.Fprintln(w, "A home volume holds the agents' settings, sessions and history; logins come from secrets.env.")
	}
	if len(p.Unknown) > 0 {
		fmt.Fprintf(w, "Kept everything of %d project(s) whose directory was never recorded (agent-sandbox before prune): %s.\n"+
			"Remove it with --all-projects, or delete their volumes with podman volume rm.\n",
			len(p.Unknown), strings.Join(p.Unknown, ", "))
	}
	return nil
}

func projectOf(it prune.Item) string {
	switch {
	case it.Project != "":
		return it.Project
	case it.ProjectID != "":
		return it.ProjectID
	}
	return "(all projects)"
}

// askYesNo prompts on out and reads one answer line from in.
func askYesNo(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
