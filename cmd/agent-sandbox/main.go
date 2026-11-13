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

// commands are the implementations the CLI dispatches to; tests replace them.
type commands struct {
	run          func(context.Context, app.RunOptions) (int, error)
	build        func(context.Context, image.BuildOptions) error
	trust        func(context.Context) error
	status       func(ctx context.Context, all bool, out io.Writer) error
	stop         func(ctx context.Context, name string, all bool, out io.Writer) error
	worktreeList func(ctx context.Context, out io.Writer) error
	worktreeRm   func(ctx context.Context, name string, force, deleteBranch bool, out io.Writer) error
	scaffold     func(ctx context.Context, out io.Writer) error
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, commands{
		run:          app.Run,
		build:        app.Build,
		trust:        app.Trust,
		status:       app.Status,
		stop:         app.Stop,
		worktreeList: app.WorktreeList,
		worktreeRm:   app.WorktreeRm,
		scaffold:     app.Init,
	}))
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
	var strict, gc, replace bool
	root := &cobra.Command{
		Use:           "agent-sandbox [-n NAME] [--strict] [--gc] [--replace] [-- CMD ARGS...]",
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
			c, err := cmds.run(cmd.Context(), app.RunOptions{Name: name, Cmd: args, Strict: strict, GC: gc, Replace: replace})
			*code = c
			return err
		},
	}
	root.Flags().StringVarP(&name, "name", "n", "default", "sandbox name")
	root.Flags().BoolVar(&strict, "strict", false, "refuse to start while findings of an earlier post-session scan still exist")
	root.Flags().BoolVar(&gc, "gc", false, "worktree mode: run git gc on the host first, so the agent cannot touch unpacked objects")
	root.Flags().BoolVar(&replace, "replace", false, "stop a running sandbox whose config, git mode, image or binary differs (ending its sessions) and start a new one")
	root.AddCommand(
		newScaffoldCmd(cmds), newTrustCmd(cmds.trust), newBuildCmd(cmds.build),
		newStatusCmd(cmds), newStopCmd(cmds), newWorktreeCmd(cmds),
		newPID1Cmd(code), newProxyCmd(),
	)
	return root
}

func newScaffoldCmd(cmds commands) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create .agent-sandbox/{config.toml,Containerfile} and trust them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmds.scaffold(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func newStatusCmd(cmds commands) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "status [--all]",
		Short: "List this project's sandboxes (--all: every project's, plus volumes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmds.status(cmd.Context(), all, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "all projects, plus their volumes")
	return cmd
}

func newStopCmd(cmds commands) *cobra.Command {
	var name string
	var all bool
	cmd := &cobra.Command{
		Use:   "stop [-n NAME | --all]",
		Short: "Stop a sandbox of this project now (--all: every sandbox of this project)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmds.stop(cmd.Context(), name, all, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "default", "sandbox name")
	cmd.Flags().BoolVar(&all, "all", false, "every sandbox of this project")
	cmd.MarkFlagsMutuallyExclusive("name", "all")
	return cmd
}

func newWorktreeCmd(cmds commands) *cobra.Command {
	wt := &cobra.Command{
		Use:   "worktree",
		Short: "Manage the git worktrees of worktree-mode sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List worktrees: name, branch, commits ahead of HEAD, dirty, running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmds.worktreeList(cmd.Context(), cmd.OutOrStdout())
		},
	}
	var force, deleteBranch bool
	rm := &cobra.Command{
		Use:   "rm NAME [--force] [--delete-branch]",
		Short: "Remove a worktree; refuses while it runs or is dirty, unless --force",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmds.worktreeRm(cmd.Context(), args[0], force, deleteBranch, cmd.OutOrStdout())
		},
	}
	rm.Flags().BoolVar(&force, "force", false, "stop a running sandbox and discard uncommitted changes")
	rm.Flags().BoolVar(&deleteBranch, "delete-branch", false, "also delete branch asb/NAME")
	wt.AddCommand(list, rm)
	return wt
}

// newPID1Cmd is the container's PID 1 (spec 4.2). Hidden: users never run it.
func newPID1Cmd(code *int) *cobra.Command {
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
	cmd.Flags().StringArrayVar(&o.HostAddrs, "host-addr", nil, "host interface address (addr/bits) to block in restricted rules")
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

func newTrustCmd(trustFn func(context.Context) error) *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Review .agent-sandbox/ against the last approved version and approve it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return trustFn(cmd.Context())
		},
	}
}

func newBuildCmd(buildFn func(context.Context, image.BuildOptions) error) *cobra.Command {
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
