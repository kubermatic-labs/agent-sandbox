// Command agent-sandbox runs AI coding agents in a per-project rootless podman
// sandbox. See docs/specs/2026-11-12-agent-sandbox-design.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"k8c.io/agent-sandbox/internal/app"
	"k8c.io/agent-sandbox/internal/config"
	"k8c.io/agent-sandbox/internal/image"
	"k8c.io/agent-sandbox/internal/initd"
	"k8c.io/agent-sandbox/internal/proxy"
)

type (
	runFunc   func(context.Context, app.RunOptions) (int, error)
	buildFunc func(context.Context, image.BuildOptions) error
	trustFunc func(context.Context) error
)

// commands are the implementations the CLI dispatches to; tests replace them.
type commands struct {
	run   runFunc
	build buildFunc
	trust trustFunc
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, commands{run: app.Run, build: app.Build, trust: app.Trust}))
}

// run executes the CLI and returns the process exit code: the session's exit
// code, or 1 for wrapper errors.
func run(args []string, stderr io.Writer, cmds commands) int {
	code := 0
	root := newRootCmd(&code, cmds)
	root.SetArgs(args)
	root.SetErr(stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintf(stderr, "agent-sandbox: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func newRootCmd(code *int, cmds commands) *cobra.Command {
	var name string
	var strict, gc bool
	root := &cobra.Command{
		Use:           "agent-sandbox [-n NAME] [--strict] [-- CMD ARGS...]",
		Short:         "Run AI coding agents in a per-project rootless podman sandbox",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Unknown positional arguments are an error, so they are never
		// mistaken for a command (spec 2.3).
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
				return fmt.Errorf("unexpected argument %q; put the command after --", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = nil
			}
			c, err := cmds.run(cmd.Context(), app.RunOptions{Name: name, Cmd: args, Strict: strict, GC: gc})
			*code = c
			return err
		},
	}
	root.Flags().StringVarP(&name, "name", "n", "default", "sandbox name")
	root.Flags().BoolVar(&strict, "strict", false, "refuse to start while findings of an earlier post-session scan still exist")
	root.Flags().BoolVar(&gc, "gc", false, "worktree mode: run git gc on the host first, so the agent cannot touch unpacked objects")
	root.AddCommand(newBuildCmd(cmds.build), newTrustCmd(cmds.trust), newInitCmd(code), newProxyCmd())
	return root
}

// newInitCmd is the container's PID 1 (spec 4.2). Hidden: users never run it.
func newInitCmd(code *int) *cobra.Command {
	var o initd.Options
	cmd := &cobra.Command{
		Use:    "_init",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			*code = initd.Main(o, cmd.ErrOrStderr())
			return nil
		},
	}
	cmd.Flags().StringVar(&o.Network, "network", "", "network mode")
	cmd.Flags().DurationVar(&o.Idle, "idle", 0, "exit after this long without a session")
	cmd.Flags().IntVar(&o.UID, "uid", 0, "host uid the sessions run as")
	cmd.Flags().StringArrayVar(&o.AllowCIDRs, "allow-cidr", nil, "extra reachable CIDR")
	cmd.Flags().StringArrayVar(&o.AllowDomains, "allow-domain", nil, "allowlist mode: allowed host")
	return cmd
}

// newProxyCmd is the allowlist proxy (spec 4.4), started by _init as its
// own uid. Hidden: users never run it.
func newProxyCmd() *cobra.Command {
	var listen string
	var domains, cidrs []string
	cmd := &cobra.Command{
		Use:    "_proxy",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getuid() == 0 {
				return errors.New("_proxy must not run as root")
			}
			p := proxy.Policy{Domains: domains}
			for _, c := range cidrs {
				prefix, err := config.ParseCIDR(c)
				if err != nil {
					return err
				}
				p.CIDRs = append(p.CIDRs, prefix)
			}
			return proxy.ListenAndServe(listen, proxy.New(p, cmd.ErrOrStderr()))
		},
	}
	cmd.Flags().StringVar(&listen, "listen", initd.ProxyAddr, "listen address")
	cmd.Flags().StringArrayVar(&domains, "allow-domain", nil, "allowed host: exact, or *.suffix")
	cmd.Flags().StringArrayVar(&cidrs, "allow-cidr", nil, "allowed IP literal range")
	return cmd
}

func newTrustCmd(trustFn trustFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Review .agent-sandbox/ against the last approved version and approve it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return trustFn(cmd.Context())
		},
	}
}

func newBuildCmd(buildFn buildFunc) *cobra.Command {
	var opts image.BuildOptions
	cmd := &cobra.Command{
		Use:   "build [--no-cache] [--pull]",
		Short: "Force a build of the base image and the project image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return buildFn(cmd.Context(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.NoCache, "no-cache", false, "build without the layer cache")
	cmd.Flags().BoolVar(&opts.Pull, "pull", false, "refresh the Fedora base and the agent versions (implies --no-cache)")
	return cmd
}
