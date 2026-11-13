// Package initd is the sandbox container's PID 1, `agent-sandbox _init`
// (spec 4.2). It applies the network rules, writes the ready file, drops
// privileges permanently, then supervises: it reaps zombies and exits once
// no session has existed for the idle timeout.
//
// It fails closed: if any setup step fails it exits non-zero before writing
// the ready file, so no session ever runs unfiltered.
package initd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8c.io/agent-sandbox/internal/config"
)

const (
	// ReadyFile is non-empty once setup is complete (see setup).
	ReadyFile = "/run/asb/ready"
	// NobodyID is the uid and gid PID 1 drops to.
	NobodyID = 65534
	// ProxyID is the uid and gid of the allowlist proxy. It differs from
	// PID 1's, so a compromised proxy cannot signal PID 1.
	ProxyID = 65533
	// ProxyAddr is where the allowlist proxy listens.
	ProxyAddr = "127.0.0.1:3128"
)

// Options are the _init flags plus test hooks.
type Options struct {
	Network      string
	Idle         time.Duration
	UID          int      // host uid the sessions run as
	AllowCIDRs   []string // extra reachable CIDRs (restricted, allowlist)
	AllowDomains []string // allowlist mode: hosts the proxy allows

	ReadyFile  string        // default ReadyFile
	ProcDir    string        // default /proc
	ResolvConf string        // default /etc/resolv.conf
	Interval   time.Duration // session scan interval, default 5s
}

func (o *Options) defaults() {
	if o.ReadyFile == "" {
		o.ReadyFile = ReadyFile
	}
	if o.ProcDir == "" {
		o.ProcDir = "/proc"
	}
	if o.ResolvConf == "" {
		o.ResolvConf = "/etc/resolv.conf"
	}
	if o.Interval == 0 {
		o.Interval = 5 * time.Second
	}
}

// Validate checks the flags. Network modes that this build cannot enforce
// are refused, never run unfiltered.
func (o Options) Validate() error {
	switch o.Network {
	case "open", "restricted", "allowlist":
	default:
		return fmt.Errorf("unknown network mode %q", o.Network)
	}
	if o.Idle <= 0 {
		return errors.New("--idle must be positive")
	}
	if o.UID <= 0 || o.UID == NobodyID || o.UID == ProxyID {
		return fmt.Errorf("--uid %d is not a valid session uid", o.UID)
	}
	for _, c := range o.AllowCIDRs {
		if _, err := config.ParseCIDR(c); err != nil {
			return fmt.Errorf("--allow-cidr %q: %w", c, err)
		}
	}
	return nil
}

// Main runs PID 1 and returns its exit code. Messages go to log, which ends
// up in podman logs.
func Main(o Options, log io.Writer) int {
	o.defaults()
	if os.Getpid() != 1 {
		fmt.Fprintln(log, "agent-sandbox _init: must run as PID 1 of a sandbox container")
		return 2
	}
	// Subscribe before setup so that a stop during setup is not lost.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD)
	proxyPID, err := setup(o, log)
	if err != nil {
		fmt.Fprintf(log, "agent-sandbox _init: %v\n", err)
		return 1
	}
	return supervise(o, proxyPID, sigs, log)
}

// setup applies the network rules, starts the proxy (allowlist mode), drops
// privileges and only then marks the sandbox ready. It returns the proxy's
// pid, or 0.
//
// Readiness is a non-empty ReadyFile. The file is created empty, as root,
// before anything else (truncating any stale copy), and its descriptor is
// kept: after the drop PID 1 cannot create files in /run/asb, and the
// container has no CAP_CHOWN to hand it the directory. Writing through the
// descriptor after the verified drop makes readiness the very last step.
func setup(o Options, log io.Writer) (int, error) {
	if err := o.Validate(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(o.ReadyFile), 0o755); err != nil {
		return 0, err
	}
	ready, err := os.OpenFile(o.ReadyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", o.ReadyFile, err)
	}
	defer func() { _ = ready.Close() }()
	// 1. Network rules. Open mode has none.
	if o.Network != "open" {
		in, err := netInputs(o)
		if err != nil {
			return 0, err
		}
		var rules []Rule
		if o.Network == "allowlist" {
			rules, err = AllowlistRules(in, ProxyID)
		} else {
			rules, err = RestrictedRules(in)
		}
		if err != nil {
			return 0, err
		}
		if err := applyRules(rules); err != nil {
			return 0, fmt.Errorf("applying nftables rules: %w (is nf_tables available on the host? try: sudo modprobe nf_tables)", err)
		}
		fmt.Fprintf(log, "agent-sandbox _init: network rules:\n%s", Render(rules))
	}
	// 2. The allowlist proxy, started while PID 1 can still set its uid.
	proxyPID := 0
	if o.Network == "allowlist" {
		var err error
		if proxyPID, err = startProxy(o); err != nil {
			return 0, err
		}
	}
	// 3. Drop privileges for good.
	if err := DropPrivileges(NobodyID, NobodyID); err != nil {
		return 0, fmt.Errorf("dropping privileges: %w", err)
	}
	// 4. Ready, last.
	if err := markReady(ready); err != nil {
		return 0, fmt.Errorf("writing %s: %w", o.ReadyFile, err)
	}
	return proxyPID, nil
}

// markReady makes the ready file non-empty, through a descriptor opened
// before the privilege drop.
func markReady(f *os.File) error {
	if _, err := f.WriteString("ready\n"); err != nil {
		return err
	}
	return f.Close()
}

// startProxy starts `agent-sandbox _proxy` as ProxyID and waits until it
// listens. The proxy gets no capabilities: it execs as a non-root uid with
// no ambient set, under no_new_privs.
func startProxy(o Options) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	args := []string{exe, "_proxy", "--listen=" + ProxyAddr}
	for _, d := range o.AllowDomains {
		args = append(args, "--allow-domain="+d)
	}
	for _, c := range o.AllowCIDRs {
		args = append(args, "--allow-cidr="+c)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer func() { _ = devnull.Close() }()
	p, err := os.StartProcess(exe, args, &os.ProcAttr{
		Dir:   "/",
		Env:   []string{"PATH=/usr/bin:/bin"},
		Files: []*os.File{devnull, os.Stderr, os.Stderr},
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: ProxyID, Gid: ProxyID, Groups: []uint32{}},
			Pdeathsig:  syscall.SIGKILL,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("starting the proxy: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", ProxyAddr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return p.Pid, nil
		}
		var ws syscall.WaitStatus
		if pid, _ := syscall.Wait4(p.Pid, &ws, syscall.WNOHANG, nil); pid == p.Pid {
			return 0, fmt.Errorf("the proxy exited during startup (%v)", ws)
		}
		if time.Now().After(deadline) {
			_ = p.Kill()
			return 0, fmt.Errorf("the proxy did not listen on %s within 5s", ProxyAddr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func supervise(o Options, proxyPID int, sigs <-chan os.Signal, log io.Writer) int {
	isProxy := func(pid int) bool { return proxyPID != 0 && pid == proxyPID }
	tick := time.NewTicker(o.Interval)
	defer tick.Stop()
	idle := idleTracker{timeout: o.Idle, lastActive: time.Now()}
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGCHLD {
				if reap(proxyPID) {
					fmt.Fprintln(log, "agent-sandbox _init: the proxy exited, exiting")
					return 1
				}
				continue
			}
			fmt.Fprintf(log, "agent-sandbox _init: %v, exiting\n", sig)
			return 0
		case now := <-tick.C:
			if reap(proxyPID) {
				fmt.Fprintln(log, "agent-sandbox _init: the proxy exited, exiting")
				return 1
			}
			n, err := CountSessions(o.ProcDir, isProxy)
			if err != nil {
				// Keep running: a scan error must not end live sessions.
				fmt.Fprintf(log, "agent-sandbox _init: scanning sessions: %v\n", err)
				continue
			}
			if idle.observe(now, n) {
				fmt.Fprintf(log, "agent-sandbox _init: no session for %v, exiting\n", o.Idle)
				return 0
			}
		}
	}
}

// netInputs gathers the rule inputs from the container.
func netInputs(o Options) (NetInputs, error) {
	rc, err := os.ReadFile(o.ResolvConf)
	if err != nil {
		return NetInputs{}, err
	}
	onLink, err := OnLinkPrefixes()
	if err != nil {
		return NetInputs{}, fmt.Errorf("reading interface addresses: %w", err)
	}
	in := NetInputs{Resolvers: ParseResolvConf(rc), OnLink: onLink}
	for _, c := range o.AllowCIDRs {
		p, err := config.ParseCIDR(c)
		if err != nil {
			return NetInputs{}, err
		}
		in.AllowCIDRs = append(in.AllowCIDRs, p)
	}
	return in, nil
}

// reap collects every exited child (PID 1 inherits orphans) and reports
// whether one of them was the proxy.
func reap(proxyPID int) bool {
	proxyDied := false
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return proxyDied
		}
		if proxyPID != 0 && pid == proxyPID {
			proxyDied = true
		}
	}
}
