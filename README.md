# agent-sandbox

Run AI coding agents (`claude`, `opencode`, `pi`, ...) in permissive mode inside a per-project rootless podman
container that can see only the project.

The agent may do anything inside the project. Outside it, it should be able to do as little as possible: no host
secrets (`~/.ssh`, cloud credentials, other repos), no way to plant files the host later runs, no access to the host or
LAN by default, and optionally no exfiltration. The design and its reasoning are in
[docs/specs/2026-11-12-agent-sandbox-design.md](docs/specs/2026-11-12-agent-sandbox-design.md).

## Requirements

- Linux with rootless podman 5.0 or newer (developed on Fedora with podman 6, SELinux enforcing)
- the `nf_tables` kernel module (`sudo modprobe nf_tables` if `_init` cannot apply its rules)
- Go 1.27 to build

## Install

```sh
just build                       # CGO_ENABLED=0 go build -o dist/agent-sandbox ./cmd/agent-sandbox
install -m 0755 dist/agent-sandbox ~/.local/bin/
ln -s agent-sandbox ~/.local/bin/asb     # optional short name
```

Agents authenticate through `~/.config/agent-sandbox/secrets.env` (mode 0600), since each project gets its own home
volume:

```sh
claude setup-token               # on the host, once
printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "<token>" > ~/.config/agent-sandbox/secrets.env
chmod 600 ~/.config/agent-sandbox/secrets.env
```

Add `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and similar there as needed.

## Usage

```sh
cd ~/src/myproject
agent-sandbox -- claude                  # start (or reuse) the sandbox and run claude in it
agent-sandbox                            # run `command` from the config, or bash -l
echo "fix the tests" | agent-sandbox -- claude -p
agent-sandbox -n feature -- claude       # a second, independent sandbox of the same project
```

The first run builds the base image (Fedora with git, ripgrep, gcc, node and the three agents), which takes a few
minutes. The sandbox keeps running between sessions and stops itself after `idle_timeout` (10 minutes) without one.
If the config changes while it runs (network, mounts, `readonly`, resources, ...), new sessions are refused until you
stop it, so a sandbox never enforces an older policy.

| Command | |
|---|---|
| `agent-sandbox [-n NAME] [--strict] [--gc] [-- CMD ARGS...]` | run a session; the exit code is the command's |
| `agent-sandbox init` | create `.agent-sandbox/{config.toml,Containerfile}` and trust them |
| `agent-sandbox trust` | review `.agent-sandbox/` against the last approved version and approve it |
| `agent-sandbox build [--no-cache] [--pull]` | rebuild images; `--pull` refreshes image and the agents |
| `agent-sandbox status [--all]` | this project's sandboxes (`--all`: every project's, plus volumes) |
| `agent-sandbox stop [-n NAME \| --all]` | stop a sandbox of this project now (`--all`: all of this project's) |
| `agent-sandbox worktree list` | worktree-mode worktrees: branch, commits ahead, dirty, running |
| `agent-sandbox worktree rm NAME [--force] [--delete-branch]` | remove a worktree (the branch stays by default) |

Inside the sandbox the project is at the same path as on the host, the working directory follows yours, and `$HOME`
is the project's own home volume.

## Configuration

Global: `~/.config/agent-sandbox/config.toml` (always trusted). Project: `<root>/.agent-sandbox/config.toml`. Project
values override global ones; lists are concatenated. Unknown keys are errors.

```toml
command       = ["claude"]          # default command; bash -l if unset
network       = "restricted"        # restricted (default) | open | allowlist
allow_cidrs   = []                  # extra reachable private addresses (e.g. a local registry)
allow_domains = []                  # allowlist mode, on top of the built-in agent API endpoints
git           = "ro"                # ro (default) | worktree
idle_timeout  = "10m"
readonly      = []                  # extra read-only paths, project-relative
worktree_copy = []                  # worktree mode: untracked files copied into a new worktree (.env.local)
env           = {}                  # extra session environment
mounts        = []                  # [{ src = "~/.kube/kind.yaml", dst = "~/.kube/config", ro = true }]
caches        = ["go"]              # go | npm | pip: per-project cache volumes
resources     = { memory = "8g", cpus = 4, pids = 4096 }
strict        = false               # refuse to start while scan findings are unresolved
git_user_name  = ""                 # worktree mode commit identity, default "agent-sandbox (<name>)"
git_user_email = ""                 # default "agent-sandbox@localhost"
```

`mounts` refuses anything that would undo the sandbox: `/`, `$HOME`, system dirs, `$XDG_RUNTIME_DIR` (the podman
socket), `~/.ssh`, `~/.gnupg`, `~/.config`, podman's storage, parents of the project, and read-write mounts of
dotfiles or `$PATH` directories.

`~/.config/agent-sandbox/home/`, if it exists, is copied into each new home volume (agent settings, a
`.claude.json` with onboarding done, a `.gitconfig`). `~/.config/agent-sandbox/base/Containerfile` replaces the
built-in base image.

### Project image

`.agent-sandbox/Containerfile` extends the base image for one project:

```Dockerfile
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
RUN dnf -y install golang && dnf clean all
```

### Trust

`.agent-sandbox/` comes with the repository, so it is untrusted until you approve it. When it is new or changed,
agent-sandbox shows a diff and the security-relevant settings and asks; without a terminal it refuses with
`run 'agent-sandbox trust'`. The project image is built and `config.toml` is read from the approved copy, never
from the live directory. Symlinks are not allowed in `.agent-sandbox/`.

## Network modes

- **restricted** (default): the internet is reachable; the host, the LAN, link-local and private ranges, and the
  host's on-link subnets (including global IPv6 /64s), and every address the host has on any interface, with its
  subnet, are rejected. DNS goes only through pasta's forwarder.
  `allow_cidrs` opens exceptions. Blocked connections fail at once with "No route to host".
- **allowlist**: sessions reach nothing but a proxy on `127.0.0.1:3128` (set as `HTTPS_PROXY`), which allows the
  agents' API and login endpoints plus `allow_domains` (`example.com`, or `*.example.com` for subdomains). There is no
  TLS interception. Tools that ignore the proxy fail. Denied hosts show in `podman logs <container>`.
- **open**: no filtering.

Rules are applied with nftables inside the container's network namespace before any session starts; the session user
has no capabilities and cannot change them.

## Git modes

- **ro** (default): the project is writable, `.git` is read-only. The agent edits files; you stage and commit on the
  host.
- **worktree**: the sandbox works in its own worktree (`~/.local/share/agent-sandbox/worktrees/...`) on branch
  `asb/<name>`, which you can see and review on the host at once. The agent can commit, amend, rebase and reset there,
  and create `asb/*` branches. It cannot move other branches, create tags, stash, change the repository config or
  hooks, or touch packed history. Not supported for reftable repositories.

In both modes these are read-only inside the sandbox when they exist: `.agent-sandbox/`, `.git`, `.envrc`,
`.claude/settings.json`, `.claude/settings.local.json`, `.claude/hooks`, `.mcp.json`, `.opencode`, `.codex`,
`.vscode`, `.idea`, `.husky`, `.pre-commit-config.yaml`, `lefthook.yml`, `mise.toml`, `.nvim.lua`, `.exrc`, the
directory `core.hooksPath` points to, git config files pulled in with `include.path`/`includeIf` (created empty if
missing), and `readonly` from the config. A symlinked entry stops the sandbox from starting.

## After each session

agent-sandbox scans the project for nested `.git` directories (their config and hooks would run when you `cd` there)
and for protected files the agent created, prints a warning, and records the findings in
`~/.local/share/agent-sandbox/state/<project>/findings`. Inspect or delete the paths; delete the findings file once
reviewed. With `--strict` or `strict = true`, agent-sandbox refuses to start while findings remain.

## Residual risks

agent-sandbox narrows what an agent can do; it does not make running one risk-free.

- Kernel and podman escapes are out of scope. The layers are rootless podman, user namespaces, seccomp and SELinux.
- The agent can read the tokens in `secrets.env`. In `open` and `restricted` mode it can send them anywhere. They are
  shared by all projects, so a leaked token is a leak for all of them.
- `allowlist` reduces exfiltration but does not stop it: the agent can send data through an allowed API, for example
  with an attacker's API key.
- `open` mode can reach the host through `host.containers.internal`, and the LAN.
- `restricted` and `allowlist` block the host's own addresses as they were when the sandbox started. An address the
  host gains later (a VPN coming up, another network) stays reachable until the sandbox restarts (`stop`).
- Protected paths that did not exist when the sandbox started are not protected; the post-session scan only detects
  them.
- An approved project Containerfile runs its `RUN` steps on the host with podman's unrestricted build network. Review
  it like any script you run.
- The agent can change any project file, including build scripts, `Makefile`, `package.json` scripts and tests that
  you later run on the host. Review diffs before running project code on the host.
- Worktree mode: the agent can overwrite or delete loose objects the host has not packed yet. `--gc` packs them first.
- Named sandboxes of one project share its home and cache volumes, so state one session plants persists into later
  sessions of the same project (never of another project).
- A session can leave a background process running, which keeps the sandbox from going idle; `stop` ends it.

## Development

```sh
just test          # unit tests
just integration   # real rootless podman; each escape attempt must fail
just lint
```

The integration tests use `AGENT_SANDBOX_CONFIG_DIR` and `AGENT_SANDBOX_DATA_DIR` to keep their state out of yours;
they never change `XDG_*`, which would also move podman's own configuration and image store.
