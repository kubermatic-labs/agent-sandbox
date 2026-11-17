// Package sandbox starts sandbox containers and execs sessions in them
// (spec 2.4, 4.1, 4.3).
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

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

// ConfigLabel records ConfigHash on the container.
const ConfigLabel = "agent-sandbox.config"

// hostAddrFlag passes a host address to _init. It is left out of ConfigHash:
// host addresses change all the time (temporary IPv6 addresses, another
// Wi-Fi), and a running sandbox cannot learn new ones anyway (spec 4.4).
const hostAddrFlag = "--host-addr="

// ConfigHash identifies everything a running sandbox enforces: the podman
// run argv without the labels, the image and the host addresses (network mode, allowed CIDRs and
// domains, idle timeout, mounts and deny-list overlays, resources, env
// file). A running sandbox with another hash was started under another
// policy and must not be reused (spec 2.4). Image and binary changes are
// only stale, see the image-id and binary labels.
func ConfigHash(in RunInput) string {
	in.Image = ""
	h := sha256.New()
	for _, a := range runArgs(in, nil) {
		if strings.HasPrefix(a, hostAddrFlag) {
			continue
		}
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RunArgs returns the podman run argv (spec 4.1), labelled with ConfigHash.
// There is deliberately no --rm: the wrapper removes exited containers
// itself after printing their logs.
func RunArgs(in RunInput) []string {
	labels := maps.Clone(in.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ConfigLabel] = ConfigHash(in)
	return runArgs(in, labels)
}

func runArgs(in RunInput, labels map[string]string) []string {
	args := []string{"run", "-d", "--name", in.Container}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", k+"="+labels[k])
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
// hostAddrs are the host's own interface addresses (initd.InterfaceAddrs on
// the host), blocked in restricted and allowlist mode.
func InitCommand(cfg config.Config, uid int, hostAddrs []netip.Prefix) []string {
	cmd := []string{
		mounts.BinaryPath, "_init",
		"--network=" + cfg.Network,
		"--idle=" + cfg.IdleTimeout.String(),
		"--uid=" + strconv.Itoa(uid),
	}
	for _, c := range cfg.AllowCIDRs {
		cmd = append(cmd, "--allow-cidr="+c)
	}
	if cfg.Network != "open" {
		for _, a := range hostAddrs {
			cmd = append(cmd, hostAddrFlag+a.String())
		}
	}
	if cfg.Network == "allowlist" {
		for _, d := range cfg.AllowDomainsWithDefaults() {
			cmd = append(cmd, "--allow-domain="+d)
		}
	}
	return cmd
}

// ProxyEnv points sessions at the allowlist proxy (spec 4.4). Tools that
// ignore it fail closed: nothing else is reachable.
func ProxyEnv(addr string) []string {
	u := "http://" + addr
	return []string{
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u, "http_proxy=" + u, "https_proxy=" + u,
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
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

// passEnv are taken from the host environment when set there. ZELLIJ is a
// bare marker (the zellij socket is not mounted): agents need it to know they
// run inside zellij, e.g. Claude Code must not use DECSTBM scroll regions
// there, or scrolling leaves stale lines on screen.
var passEnv = []string{"TERM", "COLORTERM", "LANG", "ZELLIJ"}

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

// GitSessionEnv configures git for worktree-mode sessions through
// GIT_CONFIG_COUNT (spec 5.3): no automatic gc or maintenance (they would
// try to repack into the read-only objects/pack), and a commit identity.
// These are conveniences, not security: the agent can unset them.
func GitSessionEnv(name, userName, userEmail string) []string {
	if userName == "" {
		userName = "agent-sandbox (" + name + ")"
	}
	if userEmail == "" {
		userEmail = "agent-sandbox@localhost"
	}
	kv := [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "0"}, {"user.name", userName}, {"user.email", userEmail}}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(kv))}
	for i, p := range kv {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
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
