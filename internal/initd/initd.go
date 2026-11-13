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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8c.io/agent-sandbox/internal/config"
)

const (
	// ReadyFile is created once setup is complete.
	ReadyFile = "/run/asb/ready"
	// NobodyID is the uid and gid PID 1 drops to.
	NobodyID = 65534
)

// Options are the _init flags plus test hooks.
type Options struct {
	Network    string
	Idle       time.Duration
	UID        int      // host uid the sessions run as
	AllowCIDRs []string // restricted mode, milestone 3

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
	case "open", "restricted":
	case "allowlist":
		return fmt.Errorf("network mode %q is not supported by this build", o.Network)
	default:
		return fmt.Errorf("unknown network mode %q", o.Network)
	}
	if o.Idle <= 0 {
		return errors.New("--idle must be positive")
	}
	if o.UID <= 0 || o.UID == NobodyID {
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
	if err := setup(o, log); err != nil {
		fmt.Fprintf(log, "agent-sandbox _init: %v\n", err)
		return 1
	}
	return supervise(o, sigs, log)
}

func setup(o Options, log io.Writer) error {
	if err := o.Validate(); err != nil {
		return err
	}
	// 1. Network rules. Open mode has none; allowlist is refused by
	//    Validate until it is implemented.
	if o.Network == "restricted" {
		rules, err := restrictedRules(o)
		if err != nil {
			return err
		}
		if err := applyRules(rules); err != nil {
			return fmt.Errorf("applying nftables rules: %w (is nf_tables available on the host? try: sudo modprobe nf_tables)", err)
		}
		fmt.Fprintf(log, "agent-sandbox _init: network rules:\n%s", Render(rules))
	}
	// 2. Allowlist proxy: not yet.
	// 3. Ready file.
	if err := os.MkdirAll(filepath.Dir(o.ReadyFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(o.ReadyFile, nil, 0o644); err != nil {
		return err
	}
	// 4. Drop privileges for good.
	if err := DropPrivileges(NobodyID, NobodyID); err != nil {
		return fmt.Errorf("dropping privileges: %w", err)
	}
	return nil
}

func supervise(o Options, sigs <-chan os.Signal, log io.Writer) int {
	tick := time.NewTicker(o.Interval)
	defer tick.Stop()
	idle := idleTracker{timeout: o.Idle, lastActive: time.Now()}
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGCHLD {
				reap()
				continue
			}
			fmt.Fprintf(log, "agent-sandbox _init: %v, exiting\n", sig)
			return 0
		case now := <-tick.C:
			reap()
			n, err := CountSessions(o.ProcDir, nil)
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

// restrictedRules gathers the inputs from the container and builds the rules.
func restrictedRules(o Options) ([]Rule, error) {
	rc, err := os.ReadFile(o.ResolvConf)
	if err != nil {
		return nil, err
	}
	onLink, err := OnLinkPrefixes()
	if err != nil {
		return nil, fmt.Errorf("reading interface addresses: %w", err)
	}
	in := NetInputs{Resolvers: ParseResolvConf(rc), OnLink: onLink}
	for _, c := range o.AllowCIDRs {
		p, err := config.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		in.AllowCIDRs = append(in.AllowCIDRs, p)
	}
	return RestrictedRules(in)
}

// reap collects every exited child (PID 1 inherits orphans).
func reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}
