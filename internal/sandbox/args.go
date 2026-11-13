// Package sandbox starts sandbox containers and execs sessions in them
// (spec 2.4, 4.1, 4.3).
package sandbox

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/mounts"
	"k8c.io/agent-sandbox/internal/project"
)

// Labels returns the container labels (spec 2.1).
func Labels(p project.Project, name, gitMode, imageID, binaryHash string) map[string]string {
	return map[string]string{
		"agent-sandbox":            "1",
		"agent-sandbox.project":    p.Root,
		"agent-sandbox.project-id": p.ID,
		"agent-sandbox.name":       name,
		"agent-sandbox.image-id":   imageID,
		"agent-sandbox.binary":     binaryHash,
		"agent-sandbox.git-mode":   gitMode,
	}
}

// Lookup returns the label filter that identifies one sandbox.
func Lookup(projectID, name string) map[string]string {
	return map[string]string{"agent-sandbox.project-id": projectID, "agent-sandbox.name": name}
}

// VolumeLabels returns the labels for a project's volumes.
func VolumeLabels(projectID string) map[string]string {
	return map[string]string{"agent-sandbox": "1", "agent-sandbox.project-id": projectID}
}

// RunInput is everything podman run needs.
type RunInput struct {
	Container string
	Labels    map[string]string
	Hostname  string
	Resources config.Resources
	Mounts    []mounts.Mount
	EnvFile   string // "" if there is no secrets file
	Image     string
	Command   []string // PID 1
}

// RunArgs returns the podman run argv (spec 4.1). There is deliberately no
// --rm: the wrapper removes exited containers itself after printing their
// logs.
func RunArgs(in RunInput) []string {
	args := []string{"run", "-d", "--name", in.Container}
	for _, k := range slices.Sorted(maps.Keys(in.Labels)) {
		args = append(args, "--label", k+"="+in.Labels[k])
	}
	args = append(args,
		"--userns=keep-id", "--user", "0:0",
		"--cap-drop=all", "--cap-add=NET_ADMIN,SETUID,SETGID",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(in.Resources.PIDs),
		"--memory", in.Resources.Memory,
		"--cpus", strconv.FormatFloat(in.Resources.CPUs, 'f', -1, 64),
		"--hostname", in.Hostname,
		"--network", "pasta",
	)
	for _, m := range in.Mounts {
		args = append(args, "-v", m.Arg())
	}
	if in.EnvFile != "" {
		args = append(args, "--env-file", in.EnvFile)
	}
	args = append(args, in.Image)
	return append(args, in.Command...)
}

// InitCommand is the container's PID 1: agent-sandbox _init (spec 4.2).
func InitCommand(cfg config.Config, uid int) []string {
	cmd := []string{
		mounts.BinaryPath, "_init",
		"--network=" + cfg.Network,
		"--idle=" + cfg.IdleTimeout.String(),
		"--uid=" + strconv.Itoa(uid),
	}
	for _, c := range cfg.AllowCIDRs {
		cmd = append(cmd, "--allow-cidr="+c)
	}
	return cmd
}

// ExecInput is everything podman exec needs.
type ExecInput struct {
	Container string
	UID, GID  int
	Workdir   string
	TTY       bool
	Env       []string // K=V, see SessionEnv
	Cmd       []string
}

// passEnv are taken from the host environment when set there.
var passEnv = []string{"TERM", "COLORTERM", "LANG"}

// sessionEnv keeps agents from waiting on blocked telemetry (spec 3.1).
var sessionEnv = []string{"DISABLE_TELEMETRY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}

// SessionEnv orders session env: config env sorted by key, then the
// wrapper's own entries, which therefore win.
func SessionEnv(cfgEnv map[string]string, wrapperEnv []string) []string {
	var env []string
	for _, k := range slices.Sorted(maps.Keys(cfgEnv)) {
		env = append(env, k+"="+cfgEnv[k])
	}
	return append(env, wrapperEnv...)
}

// ExecArgs returns the podman exec argv (spec 4.3). Stdin is always attached
// (-i) so piped input works; a TTY (-t) only when stdin is a terminal.
func ExecArgs(in ExecInput) []string {
	args := []string{"exec", "-i"}
	if in.TTY {
		args = append(args, "-t")
	}
	args = append(args, "--user", fmt.Sprintf("%d:%d", in.UID, in.GID), "--workdir", in.Workdir)
	for _, k := range passEnv {
		args = append(args, "--env", k)
	}
	for _, kv := range sessionEnv {
		args = append(args, "--env", kv)
	}
	for _, kv := range in.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, in.Container)
	return append(args, in.Cmd...)
}

// VolumeMarker is written into every volume by its init container.
const VolumeMarker = ".agent-sandbox-volume"

// VolumeInitArgs returns the one-shot podman run that gives a new volume to
// the session user (spec 3.5): under keep-id a fresh volume belongs to
// container root, and :U chowns it to --user. With a seedDir, its contents
// are copied in.
//
// The init always leaves VolumeMarker behind. Podman chowns a volume that is
// still empty to the user of the next container that mounts it, which is
// the main container's --user 0:0, undoing :U (verified on podman 6.1).
func VolumeInitArgs(volume string, uid, gid int, image, seedDir string) []string {
	args := []string{
		"run", "--rm", "--userns=keep-id", "--user", fmt.Sprintf("%d:%d", uid, gid),
		"--network", "none", "--cap-drop=all", "--security-opt", "no-new-privileges",
		"-v", volume + ":/v:U",
	}
	if seedDir == "" {
		return append(args, image, "touch", "/v/"+VolumeMarker)
	}
	return append(args, "-v", seedDir+":/seed:ro,Z", image,
		"sh", "-c", "cp -R --preserve=mode,timestamps /seed/. /v/ && touch /v/"+VolumeMarker)
}
