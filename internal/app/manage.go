package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/hostgit"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/podman"
	"k8c.io/agent-sandbox/internal/project"
	"k8c.io/agent-sandbox/internal/sandbox"
	"k8c.io/agent-sandbox/internal/selfbin"
	"k8c.io/agent-sandbox/internal/trust"
	"k8c.io/agent-sandbox/internal/worktree"
)

// Status lists the sandboxes of this project, or of all projects, with
// their volumes in the latter case (spec 2.3, 3.5).
func Status(ctx context.Context, all bool, out io.Writer) error {
	e, err := resolve()
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	filter := map[string]string{"agent-sandbox": "1"}
	if !all {
		filter["agent-sandbox.project-id"] = e.proj.ID
	}
	cs, err := e.pm.ListContainers(ctx, filter)
	if err != nil {
		return err
	}
	binHash := ""
	if exe, err := executable(); err == nil {
		binHash, _ = selfbin.Hash(exe)
	}
	slices.SortFunc(cs, func(a, b podman.Container) int {
		return strings.Compare(a.Labels["agent-sandbox.project"]+a.Labels["agent-sandbox.name"],
			b.Labels["agent-sandbox.project"]+b.Labels["agent-sandbox.name"])
	})

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if len(cs) == 0 {
		fmt.Fprintln(out, "no sandboxes")
	} else {
		fmt.Fprintln(tw, "NAME\tSTATE\tUPTIME\tSESSIONS\tSTALE\tGIT\tPROJECT")
		for _, c := range cs {
			uptime, sessions := "-", "-"
			if c.State == "running" {
				uptime = formatUptime(time.Since(time.Unix(c.StartedAt, 0)))
				if ps, err := e.pm.Top(ctx, c.ID); err == nil {
					sessions = fmt.Sprint(countSessions(ps))
				}
			}
			stale := "no"
			if e.isStale(ctx, c, binHash) {
				stale = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				c.Labels["agent-sandbox.name"], c.State, uptime, sessions, stale,
				c.Labels["agent-sandbox.git-mode"], c.Labels["agent-sandbox.project"])
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if !all {
		return nil
	}

	vs, err := e.pm.ListVolumes(ctx, map[string]string{"agent-sandbox": "1"})
	if err != nil {
		return err
	}
	byProject := map[string][]string{}
	for _, v := range vs {
		id := v.Labels["agent-sandbox.project-id"]
		byProject[id] = append(byProject[id], v.Name)
	}
	if len(byProject) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	fmt.Fprintln(tw, "PROJECT-ID\tVOLUMES")
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range byProject {
			if !yield(k) {
				return
			}
		}
	}) {
		names := byProject[id]
		slices.Sort(names)
		fmt.Fprintf(tw, "%s\t%s\n", id, strings.Join(names, " "))
	}
	fmt.Fprintln(tw, "\t(remove with: podman volume rm $(podman volume ls -q --filter label=agent-sandbox.project-id=ID))")
	return tw.Flush()
}

// isStale reports whether a sandbox runs an older binary, or an image whose
// tag a newer build has taken over.
func (e *env) isStale(ctx context.Context, c podman.Container, binHash string) bool {
	if binHash != "" && c.Labels["agent-sandbox.binary"] != binHash {
		return true
	}
	tags, err := e.pm.ImageTags(ctx, c.Labels["agent-sandbox.image-id"])
	return err != nil || len(tags) == 0
}

// countSessions counts session processes in podman top output: everything
// but PID 1 and the proxy, which run as nobody and 65533.
func countSessions(ps []podman.Process) int {
	n := 0
	for _, p := range ps {
		if p.PID == "1" || p.User == "nobody" || p.User == "65534" || p.User == "65533" {
			continue
		}
		n++
	}
	return n
}

func formatUptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Truncate(time.Second).String()
	case d < time.Hour:
		return d.Truncate(time.Minute).String()
	default:
		return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
	}
}

// Stop stops a sandbox of this project now, or all of them, instead of
// waiting for the idle timeout.
func Stop(ctx context.Context, name string, all bool, out io.Writer) error {
	e, err := resolve()
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	filter := map[string]string{"agent-sandbox.project-id": e.proj.ID}
	if !all {
		if err := project.ValidateName(name); err != nil {
			return err
		}
		filter = sandbox.Lookup(e.proj.ID, name)
	}
	cs, err := e.pm.ListContainers(ctx, filter)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		fmt.Fprintln(out, "no sandbox to stop")
		return nil
	}
	for _, c := range cs {
		if err := e.stopContainer(ctx, c); err != nil {
			return err
		}
		fmt.Fprintf(out, "stopped %s\n", c.Labels["agent-sandbox.name"])
	}
	return nil
}

func (e *env) stopContainer(ctx context.Context, c podman.Container) error {
	if err := e.pm.Stop(ctx, c.ID, 10); err != nil {
		return err
	}
	return e.pm.Remove(ctx, c.ID)
}

// WorktreeList shows this project's sandbox worktrees (spec 5.3).
func WorktreeList(ctx context.Context, out io.Writer) error {
	e, err := resolve()
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	dir := filepath.Dir(worktree.Path(e.paths.DataDir, e.proj.ID, "x"))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0) {
		fmt.Fprintln(out, "no worktrees")
		return nil
	}
	if err != nil {
		return err
	}
	git := hostgit.Runner{}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBRANCH\tAHEAD\tDIRTY\tRUNNING\tPATH")
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		path := filepath.Join(dir, name)
		branch := worktree.Branch(name)
		ahead := "?"
		if b, err := git.Run(ctx, e.proj.Root, "rev-list", "--count", "HEAD.."+branch); err == nil {
			ahead = strings.TrimSpace(string(b))
		}
		dirty := "?"
		if d, err := worktreeDirty(ctx, git, path); err == nil {
			dirty = yesNo(d)
		}
		running := "no"
		if cs, err := e.pm.ListContainers(ctx, sandbox.Lookup(e.proj.ID, name)); err == nil {
			for _, c := range cs {
				if c.State == "running" {
					running = "yes"
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, branch, ahead, dirty, running, path)
	}
	return tw.Flush()
}

// worktreeDirty runs git status in a worktree the agent controls; the
// hardened runner keeps its hooks and fsmonitor from running.
func worktreeDirty(ctx context.Context, git hostgit.Runner, path string) (bool, error) {
	out, err := git.Run(ctx, path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// WorktreeRm removes a sandbox worktree. It refuses while the sandbox runs
// or the worktree is dirty, unless forced. The branch stays unless
// deleteBranch is set.
func WorktreeRm(ctx context.Context, name string, force, deleteBranch bool, out io.Writer) error {
	if err := project.ValidateName(name); err != nil {
		return err
	}
	e, err := resolve()
	if err != nil {
		return err
	}
	if err := e.prepare(ctx); err != nil {
		return err
	}
	cs, err := e.pm.ListContainers(ctx, sandbox.Lookup(e.proj.ID, name))
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.State == "running" && !force {
			return fmt.Errorf("sandbox %s is running; 'agent-sandbox stop -n %s' first, or use --force", name, name)
		}
		if err := e.stopContainer(ctx, c); err != nil {
			return err
		}
	}

	git := hostgit.Runner{}
	path := worktree.Path(e.paths.DataDir, e.proj.ID, name)
	if _, err := os.Lstat(path); err == nil {
		dirty, err := worktreeDirty(ctx, git, path)
		if err != nil {
			return err
		}
		if dirty && !force {
			return fmt.Errorf("worktree %s has uncommitted changes; commit them, or use --force to discard them", path)
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if _, err := git.Run(ctx, e.proj.Root, append(args, path)...); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed worktree %s\n", path)
	} else {
		if _, err := git.Run(ctx, e.proj.Root, "worktree", "prune"); err != nil {
			return err
		}
	}
	if deleteBranch {
		branch := worktree.Branch(name)
		if _, err := git.Run(ctx, e.proj.Root, "branch", "-D", branch); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted branch %s\n", branch)
	}
	return nil
}

// InitOptions are the init flags.
type InitOptions struct {
	// Base, if not empty (one of config.Bases), is written to config.toml.
	Base string
	// Caches, if DetectCaches is false, are the cache presets written to
	// config.toml; empty leaves the caches key commented out.
	Caches []string
	// DetectCaches picks the cache presets from the project's files instead.
	DetectCaches bool
}

// cacheMarkers maps each cache preset to the files at the project root that
// select it in init's detection.
var cacheMarkers = []struct {
	preset string
	files  []string
}{
	{"go", []string{"go.mod", "go.work"}},
	{"npm", []string{"package.json"}},
	{"pip", []string{"requirements.txt", "pyproject.toml", "setup.py", "setup.cfg", "Pipfile"}},
}

// detectCaches returns the cache presets whose marker files exist at the
// project root. It only stats the root: no subdirectories, no git.
func detectCaches(root string) []string {
	var caches []string
	for _, m := range cacheMarkers {
		for _, f := range m.files {
			if fi, err := os.Stat(filepath.Join(root, f)); err == nil && fi.Mode().IsRegular() {
				caches = append(caches, m.preset)
				break
			}
		}
	}
	return caches
}

// Init scaffolds .agent-sandbox/ and records it as trusted (spec 3.4). A
// non-empty base (one of config.Bases) is written to config.toml; otherwise
// the base key stays commented out and the global config or the default
// applies. Cache presets are written the same way, either given or detected
// from the project's files.
func Init(_ context.Context, o InitOptions, out io.Writer) error {
	base := o.Base
	if base != "" && !slices.Contains(config.Bases, base) {
		return fmt.Errorf("--base %q is not one of %s", base, strings.Join(config.Bases, ", "))
	}
	var caches []string
	for _, c := range o.Caches {
		if c = strings.TrimSpace(c); c == "" || slices.Contains(caches, c) {
			continue
		}
		if _, ok := config.CachePresets[c]; !ok {
			return fmt.Errorf("--caches %q is not one of %s", c, strings.Join(cachePresetNames(), ", "))
		}
		caches = append(caches, c)
	}
	e, err := resolve()
	if err != nil {
		return err
	}
	if o.DetectCaches {
		caches = detectCaches(e.proj.Root)
	}
	dir := filepath.Join(e.proj.Root, trust.DirName)
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists; edit it, then run 'agent-sandbox trust'", dir)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{"config.toml": scaffoldConfig(base, caches), "Containerfile": scaffoldContainerfile(base)}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	c, err := trust.Evaluate(e.proj.Root, e.trustStore())
	if err != nil {
		return err
	}
	defer c.Discard()
	if c.State == trust.Untrusted {
		if err := c.Approve(); err != nil {
			return err
		}
	}
	if len(caches) > 0 {
		how := ""
		if o.DetectCaches {
			how = " (detected; override with --caches)"
		}
		fmt.Fprintf(out, "caches: %s%s\n", strings.Join(caches, ", "), how)
	}
	fmt.Fprintf(out, "created %s/config.toml and %s/Containerfile (trusted); after editing them, run 'agent-sandbox trust'\n", dir, dir)
	return nil
}

// cachePresetNames returns the known cache presets, sorted.
func cachePresetNames() []string {
	names := make([]string, 0, len(config.CachePresets))
	for k := range config.CachePresets {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// baseTemplateLine and cachesTemplateLine are lines of configTemplate that
// init can set.
const (
	baseTemplateLine   = `# base          = "fedora:45"         # fedora:45 | debian:12 | ubuntu:26.04 | alpine:3`
	cachesTemplateLine = `# caches        = ["go"]              # go | npm | pip`
)

const configTemplate = `# agent-sandbox project config. Changes need approval: 'agent-sandbox trust'.
# The global config is ~/.config/agent-sandbox/config.toml; lists add up,
# other keys here override it.

` + baseTemplateLine + `
# command       = ["claude"]          # default command; bash -l if unset
# network       = "restricted"        # restricted | open | allowlist
# allow_cidrs   = []                  # extra reachable private addresses
# allow_domains = []                  # allowlist mode, on top of the agent API defaults
# git           = "ro"                # ro | worktree
# idle_timeout  = "10m"
# readonly      = []                  # extra read-only paths, project-relative
# worktree_copy = []                  # worktree mode: untracked files copied into a new worktree
# env           = {}
# mounts        = []                  # [{ src = "~/.kube/kind.yaml", dst = "~/.kube/config", ro = true }]
` + cachesTemplateLine + `
# resources     = { memory = "8g", cpus = 4, pids = 4096 }
# strict        = false               # refuse to start while scan findings are unresolved
# git_user_name  = ""                 # worktree mode commit identity, default "agent-sandbox (<name>)"
# git_user_email = ""                 # default "agent-sandbox@localhost"
`

const containerfileTemplate = `# Project image, built on the agent-sandbox base image (%s with git,
# ripgrep, gcc, node and the agents). RUN steps run as root at build time;
# sessions run as your uid.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# %s
`

// scaffoldConfig returns the config.toml init writes: configTemplate, with
// the base and caches keys set if not empty.
func scaffoldConfig(base string, caches []string) string {
	cfg := configTemplate
	if base != "" {
		cfg = setTemplateLine(cfg, baseTemplateLine, `"fedora:45"`, strconv.Quote(base))
	}
	if len(caches) > 0 {
		quoted := make([]string, len(caches))
		for i, c := range caches {
			quoted[i] = strconv.Quote(c)
		}
		cfg = setTemplateLine(cfg, cachesTemplateLine, `["go"]`, "["+strings.Join(quoted, ", ")+"]")
	}
	return cfg
}

// setTemplateLine uncomments line in cfg and replaces its example value,
// keeping the trailing comment aligned where the new value fits.
func setTemplateLine(cfg, line, example, value string) string {
	i, j := strings.Index(line, example), strings.LastIndex(line, " # ")
	set := line[:i] + fmt.Sprintf("%-*s", j-i, value) + line[j:]
	return strings.Replace(cfg, line, strings.TrimPrefix(set, "# "), 1)
}

// scaffoldContainerfile returns the Containerfile init writes, with an
// example in the base's package manager. An empty base is the default.
func scaffoldContainerfile(base string) string {
	if base == "" {
		base = config.Bases[0]
	}
	name, example := "Fedora", "RUN dnf -y install golang && dnf clean all"
	switch image.Distro(base) {
	case "debian", "ubuntu":
		name = map[string]string{"debian": "Debian", "ubuntu": "Ubuntu"}[image.Distro(base)]
		example = "RUN apt-get update && apt-get install -y --no-install-recommends golang && rm -rf /var/lib/apt/lists/*"
	case "alpine":
		name, example = "Alpine", "RUN apk add --no-cache go"
	}
	return fmt.Sprintf(containerfileTemplate, name, example)
}
