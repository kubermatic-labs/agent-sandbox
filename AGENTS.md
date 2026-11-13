# AGENTS.md

`agent-sandbox` is a Go CLI that runs AI coding agents in a per-project rootless podman sandbox. The adversary is the
agent: a change that weakens isolation is a bug even if the tests pass.

## Source of truth

- `docs/specs/2026-11-12-agent-sandbox-design.md` is the approved design. Package comments cite it as `(spec 4.4)`;
  keep comments and the spec in sync when behavior changes.
- `README.md` is the user-facing contract: CLI, config keys, residual risks.
- `cmd/agent-sandbox/main.go` is the CLI; `internal/app` wires the commands. `_init` (container PID 1) and `_proxy`
  (allowlist proxy) are hidden subcommands, not user API.

## Commands

- `just build` builds `dist/agent-sandbox` with `CGO_ENABLED=0`. This is required: the binary is copied into the
  container, so do not add cgo code.
- `just test` runs unit tests. Focused: `go test ./internal/mounts -run TestName`.
- `just integration` needs real rootless podman (>= 5), the `nf_tables` module, and a base-image build on the first
  run (network, minutes). It uses `-tags integration`; `just test` does not run it.
  Focused: `go test -tags integration ./test -run TestName`.
- `just lint` is the gate: gofmt (must print nothing), `go vet` with and without the integration tag, golangci-lint v2.
  There is no CI; run `just lint && just test`, plus integration tests when touching runtime or isolation.
- Golden files (`internal/sandbox/testdata`, `internal/initd/testdata`) are rewritten with
  `go test ./internal/sandbox ./internal/initd -update` (flag after the package list). Review the diff: these are the
  podman mount plan and nftables ruleset, both security-critical.

## Rules that are easy to break

- `internal/mounts` and `internal/initd/netrules.go` must stay pure: no podman, syscalls or host probing, so their
  plans remain golden-testable. Treat changes there as security changes and update the integration escape tests.
- Never invoke host `git` for project discovery or inspection; root discovery is pure Go. Every host `git` call must
  go through `internal/hostgit.Runner`, which disables system config, hooks and fsmonitor.
- Fail closed. `_init` must exit non-zero before writing `/run/asb/ready` if the network rules or proxy cannot be set
  up; never add an unfiltered fallback. Podman access goes through the thin CLI wrapper in `internal/podman` (JSON
  output), not Go bindings. `podman run` deliberately has no `--rm`.
- The project is mounted at its real host absolute path and the cwd is mapped into it. Do not switch to
  `/workspace`-style paths: error messages and `file:line` links depend on the host paths.
- Adding a config key touches several places: `internal/config/config.go`, `validate.go`, `README.md`, the design
  spec, and `configTemplate` in `internal/app/manage.go`.
- Integration tests isolate state with `AGENT_SANDBOX_CONFIG_DIR` and `AGENT_SANDBOX_DATA_DIR`; never change
  `XDG_CONFIG_HOME`/`XDG_DATA_HOME` (podman keeps its image store there). Tests clean up by
  `agent-sandbox.project-id` labels.
