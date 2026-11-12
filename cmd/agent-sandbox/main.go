// Command agent-sandbox runs AI coding agents in a per-project rootless podman
// sandbox. See docs/specs/2026-11-12-agent-sandbox-design.md.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"k8c.io/agent-sandbox/internal/app"
	"k8c.io/agent-sandbox/internal/image"
)

type (
	runFunc   func(context.Context, app.RunOptions) (int, error)
	buildFunc func(context.Context, image.BuildOptions) error
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, app.Run, app.Build))
}

// run executes the CLI and returns the process exit code: the session's exit
// code, or 1 for wrapper errors.
func run(args []string, stderr io.Writer, runFn runFunc, buildFn buildFunc) int {
	code := 0
	root := newRootCmd(&code, runFn, buildFn)
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

func newRootCmd(code *int, runFn runFunc, buildFn buildFunc) *cobra.Command {
	var name string
	root := &cobra.Command{
		Use:           "agent-sandbox [-n NAME] [-- CMD ARGS...]",
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
			c, err := runFn(cmd.Context(), app.RunOptions{Name: name, Cmd: args})
			*code = c
			return err
		},
	}
	root.Flags().StringVarP(&name, "name", "n", "default", "sandbox name")
	root.AddCommand(newBuildCmd(buildFn))
	return root
}

func newBuildCmd(buildFn buildFunc) *cobra.Command {
	var opts image.BuildOptions
	cmd := &cobra.Command{
		Use:   "build [--no-cache] [--pull]",
		Short: "Force a base image build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return buildFn(cmd.Context(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.NoCache, "no-cache", false, "build without the layer cache")
	cmd.Flags().BoolVar(&opts.Pull, "pull", false, "refresh the Fedora base and the agent versions (implies --no-cache)")
	return cmd
}
