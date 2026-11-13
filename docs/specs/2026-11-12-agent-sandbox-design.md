# agent-sandbox: design

Date: 2026-11-12
Status: approved design, revised 2026-11-13 after security review; not yet implemented
Module: `k8c.io/agent-sandbox` (Go 1.27, single module)

## 1. Purpose and threat model

`agent-sandbox` is a CLI wrapper around rootless podman. It runs AI coding agents (`claude`, `opencode`, `codex`, ...)
in permissive mode, in a per-project container that can see only the project.

The adversary is the agent itself, which may be wrong, prompt-injected, or overeager. The agent may do anything inside
the project. Outside the project it should be able to do as little as possible:

- no access to host secrets (`~/.ssh`, `~/.gnupg`, cloud credentials, other repos, host agent config);
- no path to code execution on the host, either directly or by planting files that the host runs later;
- no lateral movement to the host or LAN by default;
- optionally, no exfiltration (allowlist network mode);
- no persistent state inside the sandbox beyond what the user put in the image, the project, and the project's own
  named volumes;
- no state shared between the sandboxes of different projects, so an agent in project A cannot plant anything that
  runs in project B's sandbox.

Non-goals: protecting against kernel or podman escapes (rootless podman, user namespaces, seccomp and SELinux are the
layers we rely on), multi-user setups, Docker support, macOS.

Primary target host: Fedora, rootless podman 6.x, SELinux enforcing, ext4/btrfs.

## 2. CLI and project identity

### 2.1 Project resolution

- Project root: walk up from `$PWD` to the first directory containing a `.git` entry (a directory or a file). If none
  is found, use `$PWD`. Do this in Go and **never invoke host `git` for discovery**, because a hostile repo's
  `core.fsmonitor` and similar settings must never run on the host. Where host `git` is unavoidable (worktree mode),
  it is always run hardened (section 5.4).
- The project root is refused when it is `$HOME` or above it (a dotfiles repo with `~/.git` would otherwise make the
  whole home the project), when it overlaps the agent-sandbox config dir, the data dir, `$XDG_RUNTIME_DIR`, `~/.ssh`
  or `~/.gnupg`, or when it contains `:` or `,` (podman's `-v` syntax cannot express them).
- Project ID: `<basename(root)>-<hex(sha256(abs root path))[:8]>`. Two checkouts of the same repo get two separate
  sandboxes.
- Sandbox name: `-n/--name NAME`, default `default`.
- Container name: `asb-<project-id>-<name>`.
- Container labels (used for lookup and never parsed back from names):
  - `agent-sandbox=1`
  - `agent-sandbox.project=<abs root>`
  - `agent-sandbox.project-id=<id>`
  - `agent-sandbox.name=<name>`
  - `agent-sandbox.image-id=<podman image ID the container was started from>`
  - `agent-sandbox.binary=<sha256 of the agent-sandbox binary>`
  - `agent-sandbox.git-mode=ro|worktree`
- Volumes created by agent-sandbox carry `agent-sandbox=1` and `agent-sandbox.project-id=<id>`, so
  `podman volume ls --filter label=agent-sandbox.project-id=<id>` finds a project's volumes for cleanup.

### 2.2 Paths inside the container

- `ro` mode: the project is mounted at its **host absolute path**, so error messages, `file:line` links and Go paths
  match the host.
- `worktree` mode: the worktree is mounted at **its own real host path**, and the main `<root>/.git` is mounted at its
  real host path (see section 5).
- The cwd is preserved relative to the root: running from `<root>/pkg/tasks` gives a container workdir of
  `<mounted root>/pkg/tasks`. If the relative dir does not exist in the worktree, fall back to the worktree root.
- `$HOME` inside the container is the host user's home path (what `--userns=keep-id` sets up), so the project mount
  nests under it as on the host.

### 2.3 Commands

```
agent-sandbox [-n NAME] [--strict] [--gc] [-- CMD ARGS...]   # main entry: ensure image, ensure running, exec
agent-sandbox init                                    # scaffold .agent-sandbox/{config.toml,Containerfile}; auto-trust
agent-sandbox trust                                   # show diff of .agent-sandbox/ vs last approved snapshot, approve
agent-sandbox build [--no-cache] [--pull]             # force image build; --pull refreshes base + agent versions
agent-sandbox status [--all]                          # sandboxes for this project (or all, plus volumes): name, state, uptime, sessions, stale, git mode
agent-sandbox stop [-n NAME | --all]                  # stop now instead of waiting for idle; --all = every sandbox of this project
agent-sandbox worktree list                           # name, branch, ahead of HEAD, dirty, running
agent-sandbox worktree rm NAME [--force] [--delete-branch]
```

- Without `-- CMD`, run `command` from the config, or `bash -l` if it is unset. Unknown positional arguments are an
  error, so they are never treated as a command.
- A TTY is allocated (`-it`) only when stdin is a terminal, so `echo ... | agent-sandbox -- claude -p` works.
- The exit code of the exec'd command is passed through.
- An `asb` symlink to the binary behaves the same.
- `status`: sessions are the container's processes other than PID 1 and the proxy (from `podman top`). A sandbox is
  stale when it runs another agent-sandbox binary, or an image whose tag a newer build has taken.
- `init` writes a fully commented `config.toml` and a `Containerfile` with only `ARG BASE_IMAGE` / `FROM ${BASE_IMAGE}`,
  then records them as trusted; it refuses if `.agent-sandbox/` exists.
- `worktree rm` with `--force` also stops a running sandbox and discards uncommitted changes. If the worktree
  directory is already gone, it runs `git worktree prune`.

### 2.4 Main flow

1. Resolve the project root, project ID and sandbox name.
2. Load and merge the config. Check trust (section 3.4); fail if not trusted.
3. In worktree mode, ensure the worktree exists (section 5).
4. Compute the image hash and build the base and project images if missing (section 3.3). Builds run under
   `flock` on `$XDG_RUNTIME_DIR/agent-sandbox/build-<project-id>.lock` (and `build-base.lock` for the base image), so
   parallel invocations build each tag once.
5. Take `flock` on `$XDG_RUNTIME_DIR/agent-sandbox/<container>.lock`.
6. Find the container by labels:
   - running with a different git mode, or a different (or no) `agent-sandbox.config` label: refuse and ask for
     `podman stop <container>`. The label is a sha256 of the `podman run` argv without the labels and the image, so it
     covers the network mode, `allow_cidrs`, `allow_domains`, `idle_timeout`, mounts and deny-list overlays,
     resources and the secrets env file. A sandbox started under an older policy would not enforce the current one;
   - running with the same image ID and binary hash: use it;
   - running with a different image ID or binary hash: warn
     `sandbox runs an older image or agent-sandbox binary; 'agent-sandbox stop' to pick it up` and use it anyway;
   - exited (idle exit, `_init` failure, crash): if its exit code is non-zero, print its `podman logs`; remove it and
     continue as if absent;
   - absent: ensure the project volumes exist (section 3.5), `podman run` (section 4.1), then wait for ready
     (section 4.2).
7. Release the lock. `podman exec` the command (section 4.3).
8. After the exec returns, run the post-session scan (section 7) and exit with the command's exit code.

If the container vanishes between step 6 and the exec (the idle race), retry steps 5–7 exactly once.

## 3. Configuration, images, trust

### 3.1 Config files

- Global: `~/.config/agent-sandbox/config.toml`. Always trusted, never visible in the container.
- Directories follow `XDG_CONFIG_HOME` / `XDG_DATA_HOME`. `AGENT_SANDBOX_CONFIG_DIR` and `AGENT_SANDBOX_DATA_DIR`
  override only agent-sandbox's own directories (the integration tests use them): changing the XDG variables would
  also move podman's configuration and image store.
- Project: `<root>/.agent-sandbox/config.toml`. Gated by trust. In `ro` mode it is visible read-only in the
  container (deny-list, section 5.2). In worktree mode the host root's copy, which is the one actually used, is not
  mounted; the worktree's own checked-out copy is overlaid read-only like any deny-list path.
- Merging: project scalars override global ones. Lists are concatenated and deduplicated. Maps are merged with the
  project key winning.
- Config is **always read from the host project root**, never from a worktree's copy, and as approved: the project
  config is loaded from the trust snapshot (section 3.4), not the live file, which the agent of a running sandbox may
  have changed since the trust check.

```toml
command       = ["claude"]          # default exec target
network       = "restricted"        # restricted (default) | open | allowlist
allow_cidrs   = []                  # restricted mode: extra reachable private CIDRs / host IPs
allow_domains = []                  # allowlist mode; merged with global defaults
git           = "ro"                # ro (default) | worktree
idle_timeout  = "10m"
readonly      = []                  # extra read-only overlays (project-relative), on top of the built-in deny-list
worktree_copy = []                  # worktree mode: untracked files/dirs copied from root into a new worktree
env           = {}                  # extra env for sessions (reserved keys refused, see validation)
mounts        = []                  # [{ src = "~/.kube/kind.yaml", dst = "~/.kube/config", ro = true }]
caches        = ["go"]              # presets, see 3.5; volumes are per project
resources     = { memory = "8g", cpus = 4, pids = 4096 }
strict        = false               # see section 7
git_user_name  = ""                 # worktree mode commit identity; default "agent-sandbox (<name>)"
git_user_email = ""                 # default "agent-sandbox@localhost"
```

The built-in global defaults for `allow_domains` cover only the agent API and auth endpoints: `api.anthropic.com`,
`claude.ai`, `console.anthropic.com` (Claude Code); `api.openai.com`, `auth.openai.com`, `chatgpt.com` (codex);
`opencode.ai`, `models.dev` (opencode's service and model catalog). They live in `internal/config/domains.go`.
Keep the list in one Go file so it is easy to audit. Telemetry endpoints are excluded on purpose. Shared third-party
services such as `sentry.io` are open exfiltration channels, because anyone can create a project there. Sessions set
`DISABLE_TELEMETRY=1` / `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` so agents do not hang on blocked telemetry.

Validation:

- Unknown keys are an error.
- `network`, `git` and `caches` values must be from the known sets.
- `mounts[].src` is resolved (`~` expanded, symlinks resolved) and then refused when:
  - it is `/` or `$HOME` itself;
  - it is under `/etc`, `/usr`, `/boot`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`, `$XDG_RUNTIME_DIR` (the podman
    socket there is a full host escape, and `ro` does not stop `connect()` on a socket), `~/.ssh`, `~/.gnupg`,
    `~/.config`, `~/.local/share/containers` or the agent-sandbox data dir `~/.local/share/agent-sandbox`;
  - it is an **ancestor** of the project root or of the data dir (mounting `~/src` read-write would bypass the `.git`
    and deny-list overlays), or lies **inside** the project root (mounting `<root>/.git` separately would do the
    same);
  - the mount is read-write and `src` is a dotfile under `$HOME` or lies inside a dot-directory under `$HOME`
    (`~/.bashrc`, `~/.profile`, `~/.local/bin`, ...), or overlaps a directory on the host `$PATH` (`~/bin`);
  - it contains `:` or `,`.
- `mounts[].dst` must be absolute after `~` expansion. It must not be `/` or `$HOME` (the home volume), must not lie
  inside the project root or the agent-sandbox data dir (worktrees are mounted from there), and must neither lie inside
  nor contain `/usr/local/bin/agent-sandbox`, `/cache`, `/run/asb`, `/proc`, `/sys` or `/dev`: a mount on `/run`
  could hold a stale `/run/asb/ready` and let a session start before the network rules are applied.
- `env` must not set reserved keys: `HOME`, `PATH`, `USER`, `GIT_CONFIG_*`, `HTTP_PROXY`, `HTTPS_PROXY`,
  `http_proxy`, `https_proxy`, `NO_PROXY`, `no_proxy`, or any env var a cache preset sets.

### 3.2 Secrets

`~/.config/agent-sandbox/secrets.env` (global only, must be mode 0600, otherwise refuse) is passed with
`--env-file`. Since home volumes are per project (section 3.5), this is the way to authenticate agents once for all
projects: it holds `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) or `ANTHROPIC_API_KEY`, plus the
equivalent tokens for codex and opencode. Project config cannot reference other env files. The agent can read these
tokens (section 11).

### 3.3 Images

**Base image**

- Its Containerfile is embedded in the binary (`go:embed`). It can be overridden by
  `~/.config/agent-sandbox/base/Containerfile`, and the build context is that directory.
- Fedora-based. Contents: git, ripgrep, fd-find, jq, curl, make, gcc, procps-ng, less, which, ca-certificates, plus
  the agents `claude`, `opencode` and `codex` **baked into the image with auto-update disabled** (for example
  `DISABLE_AUTOUPDATER=1` in the image env). Agents live in the image rather than the home volume, so an agent
  cannot replace its own binary persistently.
- Tag: `localhost/agent-sandbox-base:<hash>`, where `<hash>` is sha256 of the context files.
- `build --pull` passes `--pull=always --no-cache` to refresh both the Fedora base and the agent versions. The tag
  stays the same but the image ID changes, which is why stale detection compares image IDs, not tags.

**Project image**

- Built only if `<root>/.agent-sandbox/Containerfile` exists. It must use:

  ```Dockerfile
  ARG BASE_IMAGE
  FROM ${BASE_IMAGE}
  ```

  The wrapper passes `--build-arg BASE_IMAGE=localhost/agent-sandbox-base:<hash>`.
- The final stage must be `FROM ${BASE_IMAGE}` (or `$BASE_IMAGE`); earlier stages of a multi-stage build may use
  other images. Anything else is refused before building.
- Build context: **the trusted snapshot of `<root>/.agent-sandbox/`** (section 3.4), never the live directory and
  never the whole repo, so the image is built from exactly what was approved.
- Tag: `localhost/asb-<project-id>:<hash>`, where `<hash>` is sha256 of the base image ID plus all context files
  (sorted path, mode and content). Tags use the first 16 hex characters.
- `build --pull` refreshes the base image; the project build then only drops its cache (`--no-cache`), since
  `--pull=always` would try to pull the local base image from a registry.
- After a successful build, remove older `localhost/asb-<project-id>:*` tags that no running container uses (podman
  refuses to remove an image a container uses).
- `build` is gated by trust like the main command; it never builds an unapproved Containerfile.
- If there is no project Containerfile, run the base image directly.

Build output is streamed to the terminal. A failed build exits non-zero and never touches a running container.

### 3.4 Trust

- `.agent-sandbox/` is first copied to a staging dir in the store; hashing, the diff, approval and the project
  image build all use copies, so what is approved is exactly what is used. Only regular files and directories are
  allowed: a symlink (or the directory itself being one) is refused, so the build context cannot reach outside.
- The trust hash is sha256 over every file in the copy (sorted relative path, mode and content).
- Store: `~/.local/share/agent-sandbox/trust/<project-id>/`, containing `hash` plus `snapshot/`, a copy of the last
  approved files. Approval swaps the staging dir in as `snapshot/` and writes `hash` last, so an interrupted
  approval asks again.
- If the hash is missing or different:
  - TTY: show a unified diff against the snapshot (everything is new on first sight), print a summary of the
    security-relevant settings (`network`, `allow_cidrs`, `allow_domains`, `git`, `mounts`, `readonly`, `env`,
    `worktree_copy`, the Containerfile) and ask `y/N`. On yes, update the store.
  - No TTY: exit 1 with `untrusted sandbox config; run 'agent-sandbox trust'`.
- The prompt needs a terminal on both stdin and stderr.
- `agent-sandbox trust` runs the same prompt explicitly.
- `agent-sandbox init` writes the files and records them as trusted.
- If the project has no `.agent-sandbox/`, it is implicitly trusted (only global config applies). A `config.toml`
  that appears after the check is ignored for that run.

### 3.5 Volumes and caches

All volumes are **per project** and shared only by the named sandboxes of that one project. Nothing writable is shared
between projects: a shared home would let an agent in project A plant `~/.claude/settings.json` hooks, `mcpServers` in
`~/.claude.json`, `~/.bashrc` or `~/.gitconfig` that then run in project B's sandbox, and a shared `GOCACHE` or
`GOMODCACHE` could be poisoned the same way.

- `asb-<project-id>-home`, mounted at `$HOME`. It holds agent settings, transcripts and shell history for this
  project. Agent authentication comes from `secrets.env` (section 3.2), not from logins stored in the volume.
- Home seed: if `~/.config/agent-sandbox/home/` exists (global, trusted), its contents are copied into a home volume
  when the volume is created, for example a `~/.claude.json` with onboarding completed, agent settings, or a
  `.gitconfig`. Later changes to the seed do not reach existing volumes.
- Cache presets map to per-project named volumes and env vars:
  - `go` → `asb-<project-id>-go-mod` at `/cache/go-mod` (`GOMODCACHE`), `asb-<project-id>-go-build` at
    `/cache/go-build` (`GOCACHE`)
  - `npm` → `asb-<project-id>-npm` at `/cache/npm` (`npm_config_cache`)
  - `pip` → `asb-<project-id>-pip` at `/cache/pip` (`PIP_CACHE_DIR`)
- Creation, on demand, under the container lock: `podman volume create` with the labels from section 2.1. A fresh
  volume is owned by container root under keep-id, and `:U` on the main container would chown it to `--user 0:0`
  again, so ownership is fixed by a one-shot container (verified on podman 6.1):

  ```
  podman run --rm --userns=keep-id --user <uid>:<gid> --network none
    -v <volume>:/v:U [-v <seed copy>:/seed:ro,Z] <image> sh -c '[cp -R --preserve=mode,timestamps /seed/. /v/ &&] touch /v/.agent-sandbox-volume'
  ```

  The seed is copied without `cp -a`: that would also preserve the staged copy's private `:Z` SELinux label, which
  the main container cannot read.

  The marker file matters: podman chowns a volume that is still empty to the user of the next container that mounts
  it (the main container's `--user 0:0`), which would undo `:U`. Verified on podman 6.1.

  For the seed mount, the wrapper copies the seed dir to a private temp dir first and mounts that copy with `:Z`, so
  the real config dir is never relabeled.
- Volumes outlive containers. `status --all` lists them per project; removal is plain `podman volume rm` using the
  labels.

## 4. Runtime

### 4.1 `podman run`

```
podman run -d
  --name asb-<project-id>-<name> --label ...
  --userns=keep-id --user 0:0
  --cap-drop=all --cap-add=NET_ADMIN,SETUID,SETGID
  --security-opt no-new-privileges
  --pids-limit N --memory M --cpus C
  --hostname asb-<basename>
  --network pasta
  -v <project root | worktree>:<same path>:z           # see 4.4 / section 5 for the full mount plan
  -v <read-only overlays>...:ro
  -v <staged agent-sandbox binary>:/usr/local/bin/agent-sandbox:ro,z
  -v asb-<project-id>-home:$HOME
  -v asb-<project-id>-<cache>:/cache/...
  --env-file ~/.config/agent-sandbox/secrets.env
  <image> /usr/local/bin/agent-sandbox _init --network=<mode> --idle=<dur> --uid=<host uid> [--allow-cidr ...]
```

- No `--rm`: with `--rm` the container and its logs are gone before the wrapper can print why `_init` failed.
  Exited containers are removed by the wrapper (section 2.4 step 6), and by `stop`.
- Podman sorts mounts by destination, so the home volume at `$HOME` is mounted before the project and worktree
  mounts nested under it.
- `--user 0:0` with keep-id is container root, which maps to a subuid and never to host root.
- The binary mounted into the container is a copy of the running binary (`os.Executable()`, symlinks resolved),
  staged at `~/.local/share/agent-sandbox/bin/<sha256[:16]>/agent-sandbox` and mounted `:ro,z`. The container cannot
  execute a `user_home_t` binary (it dies with SIGSEGV, verified), and relabeling the original in place would fail
  for root-owned installs such as `/usr/local/bin`. The copy is content-addressed, so a running sandbox keeps the
  binary it started with; copies of other versions older than a day are pruned. It must be built with
  `CGO_ENABLED=0`.
- SELinux stays enforcing. The project or worktree, the main `.git` in worktree mode, and extra `mounts` get `:z`
  (a one-time relabel to `container_file_t`). Read-only overlay mounts of paths already under a `:z` mount do not need
  their own relabel. Section 3.1 lists the refused mount sources.

### 4.2 `_init` (PID 1)

1. Create `/run/asb/ready` empty (truncating any stale copy) and keep its descriptor. An empty file means "not
   ready".
2. Gather inputs from the container's network namespace: the resolver addresses in `/etc/resolv.conf`, and the
   address and prefix length of every interface other than `lo`. Pasta copies the host's addresses into the container
   with their prefix lengths (verified: the container gets `192.168.18.0/24` as a link route), so these prefixes are
   the host's on-link LAN subnets, including global IPv6 /64s. Then build and apply the nftables ruleset for the
   network mode via netlink (`github.com/google/nftables`). The image does not need `nft`.
3. In allowlist mode, start the proxy child (`agent-sandbox _proxy`) as uid/gid 65533.
4. Drop privileges permanently: setgroups([]), setresgid/setresuid to 65534, clear the permitted, effective,
   inheritable and ambient capability sets on every thread, set `PR_SET_NO_NEW_PRIVS`, then verify the result in
   `/proc/self/status` and exit non-zero on any mismatch. The bounding set stays: dropping from it needs
   `CAP_SETPCAP`, which the container does not grant, and PID 1 never execs afterwards. The agent's UID then cannot signal it, kill it,
   or change the rules. PID 1 and the proxy use different UIDs, so a compromised proxy cannot signal PID 1 either.
5. Write `ready` into `/run/asb/ready` through the descriptor from step 1. This is the last setup step: after the
   drop PID 1 cannot create files in `/run/asb`, and the container has no `CAP_CHOWN`, hence the early descriptor.
6. Supervise:
   - reap zombies (PID 1 duty);
   - every 5 s, scan `/proc`. A session is any process other than PID 1 and the proxy's process tree;
   - when no session process has existed for a continuous `idle_timeout`, exit 0. The exited container is removed by
     the wrapper's next run or by `stop`;
   - if the proxy exits, exit non-zero (fail closed rather than run without a proxy);
   - SIGTERM: exit promptly (used by `stop`).

If any of steps 1 to 4 fails, exit non-zero before writing `ready`. **Never run unfiltered.**

The wrapper waits for ready by polling `podman exec <c> test -s /run/asb/ready` (non-empty, not just present) for up
to 10 s. On timeout, or if the container exits first, it prints `podman logs`, then stops and removes the container.

### 4.3 Sessions

```
podman exec [-it] --user <uid>:<gid> --workdir <mapped cwd>
  --env TERM --env COLORTERM --env LANG
  --env <config env> --env <cache env> [--env HTTP(S)_PROXY/NO_PROXY in allowlist mode]
  --env GIT_CONFIG_COUNT=... (worktree mode, see 5.3)
  <container> <cmd...>
```

Exec'd processes run as the host UID with no capabilities. `no-new-privileges` blocks setuid binaries and `sudo`.
Wrapper-set env (proxy, `GIT_CONFIG_*`, cache vars) is passed after config `env`, and config `env` cannot set those
keys anyway (section 3.1).

### 4.4 Network modes

All modes use `--network pasta`. Restriction is enforced by nftables in the container's own network namespace, in the
`output` hook (IPv4 and IPv6).

- **open:** no rules. The host is reachable through `host.containers.internal` (section 11).
- **restricted (default):** the *private set* is 0.0.0.0/8, 127.0.0.0/8 (except via `lo`), 10.0.0.0/8,
  172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 224.0.0.0/4, 255.255.255.255, `::1` (except `lo`),
  `fc00::/7`, `fe80::/10`, `ff00::/8`, `::ffff:0:0/96`, `64:ff9b::/96`, `64:ff9b:1::/48`, **plus the on-link prefix
  of every non-`lo` interface** (from `_init` step 1; prefixes already inside the fixed ranges are skipped). The
  interface prefixes cover LAN hosts with public IPv4 or global IPv6 addresses, and the host's other global addresses
  on the same subnet, none of which the fixed ranges catch. **Plus the host's own addresses:** pasta copies only one
  host interface into the container, but it connects on the container's behalf from the host, so a public address
  the host has on another interface (a second NIC, an alias, a VPN) would reach host services bound to it, and its
  subnet would reach that LAN. The wrapper therefore reads the host's non-loopback interface addresses (pure Go,
  `net.Interfaces`) and passes each as `_init --host-addr=<addr>/<bits>`. `_init` rejects the address and, unless the
  prefix is wider than /16 (IPv4) or /32 (IPv6), its whole prefix. Host addresses are left out of the
  `agent-sandbox.config` hash (section 2.4): they change too often, for example temporary IPv6 addresses. An address
  the host gains after the sandbox started is not blocked until the sandbox restarts (section 11). Rules, in order:
  1. accept `oifname "lo"`;
  2. accept ICMPv6 neighbor discovery (router solicit, neighbor solicit, neighbor advert). Rejecting `ff00::/8` and
     `fe80::/10` would otherwise break IPv6 entirely; these messages cannot carry data to the LAN, and pasta answers
     them itself;
  3. accept DNS (udp/tcp 53) to pasta's DNS forwarding address only: the link-local (169.254.0.0/16) nameserver(s)
     in `/etc/resolv.conf`. Other nameservers podman lists there (for example the LAN router) are not accepted. If
     there is no link-local nameserver, `_init` fails;
  4. accept `allow_cidrs`;
  5. reject the private set, the on-link prefixes and the host addresses with ICMP admin-prohibited
     (`reject with icmpx admin-prohibited`). Reject, not drop: a drop in the output hook leaves `connect()` hanging
     until the TCP timeout (verified), so every blocked connection would stall the agent; reject fails it at once
     with "No route to host";
  6. accept everything else (chain policy).

  `host.containers.internal`, the pasta gateway and the LAN are therefore unreachable unless listed in `allow_cidrs`.
  `_init` prints the applied ruleset to `podman logs`.
- **allowlist:** one container, no sidecar:
  - the proxy runs as uid 65533 on `127.0.0.1:3128`;
  - rules: accept `lo` and ICMPv6 neighbor discovery for everyone; for `meta skuid 65533` (the proxy, including its
    DNS) apply the restricted rules above and then accept, so an allowed domain that resolves to a private or on-link
    address (DNS rebinding, a broad wildcard) still cannot reach the host or LAN; reject everything else. Sessions
    therefore reach only loopback, where the proxy listens: they cannot resolve names or connect anywhere directly;
  - sessions get `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` set to `http://127.0.0.1:3128`, and
    `NO_PROXY`/`no_proxy=localhost,127.0.0.1`;
  - the proxy is an HTTP CONNECT proxy (plain HTTP absolute-URI requests are also forwarded, minus hop-by-hop
    headers). It matches the target host against `allow_domains` plus the built-in defaults: exact match
    (case-insensitive, trailing dot ignored), or `*.suffix`, which matches subdomains only, not `suffix` itself. Other
    hosts get 403, failed upstream connections 502; denials are logged to `podman logs`. There is no TLS
    interception. Connections to IP literals are denied unless the IP is in `allow_cidrs`;
  - `_init` starts the proxy as uid/gid 65533 with no supplementary groups and no capabilities (it execs as a
    non-root uid with no ambient set, under no_new_privs), waits up to 5 s for it to listen, and exits non-zero if
    the proxy ever exits, so the sandbox never runs without it. The proxy is not counted as a session. `_proxy`
    refuses to run as root;
  - tools that ignore the proxy fail closed, by design.

## 5. Git modes

### 5.1 `git = "ro"` (default)

- The project root is mounted read-write. `<root>/.git` is overlaid read-only (whether it is a directory or a file).
- The agent edits files. The user stages and commits on the host.

### 5.2 Built-in read-only deny-list (both modes)

These are overlaid read-only inside the mounted tree (project root or worktree) when they exist:

```
.agent-sandbox/
.git                      (ro mode; in worktree mode the .git *file*)
.claude/settings.json
.claude/settings.local.json
.claude/hooks/
.mcp.json
.opencode/
.codex/
.envrc
.vscode/
.idea/
.husky/
.pre-commit-config.yaml
lefthook.yml
.lefthook.yml
mise.toml
.mise.toml
.nvim.lua
.exrc
<core.hooksPath>          (if set in <root>/.git/config and it points inside the mounted tree)
<git config includes>     (include.path / includeIf.*.path targets inside the mounted tree)
```

The added entries are files the host runs without the user deliberately running project code: hook frameworks run on
the host commit (which in `ro` mode is where commits happen), and `mise`/editors evaluate them on `cd` or open.
`core.hooksPath` is read by parsing git config in Go, never by running `git`: `/etc/gitconfig`, the global configs
(`$XDG_CONFIG_HOME/git/config`, `~/.gitconfig`) and the repository's own (through a `.git` file's `gitdir:` and
`commondir` when `.git` is a file), following `include.path` and `includeIf.*.path` unconditionally. Every
`core.hooksPath` value from any level that resolves inside the project is added (relative values resolve against the
project root, as git does), not just the effective one: over-approximating is the safe side. A configured hooks
directory that does not exist yet is created empty on the host, so it can be overlaid: host git runs whatever would
appear there. Every `include.path` and `includeIf.*.path` target from any level that resolves inside the project
is added as well, unless an entry already covers it: host git reads those files, so a writable one could set
`core.fsmonitor`, `core.pager` or an alias that runs on the host. A missing include target is created empty for the
same reason.

A deny-list path reached through a symlink (the path itself or a parent) is refused and the sandbox does not start.
A bind mount follows symlinks, so overlaying `.envrc -> ~/.ssh/id_ed25519` would expose the target inside the
sandbox, and the symlink itself could not be protected.

`readonly` from the config extends the list. Paths that do not exist are skipped. This is a known gap: the agent can
create them, and newly created files are not protected. The post-session scan (section 7) reports any deny-list path
that exists but was not overlaid, so creation is at least detected. Document the gap in the README.

### 5.3 `git = "worktree"`

**Creation** (on the host, the first time `-n NAME` starts in worktree mode):

0. Refuse worktree mode if `<root>/.git/config` sets `extensions.refStorage=reftable` (parsed in Go). With reftable,
   refs live in `.git/reftable/` and the per-path openings below cannot confine the agent to `asb/*`.
1. `git -C <root> worktree add <data>/worktrees/<project-id>/<name> -b asb/<name> HEAD`, run hardened (section 5.4).
   `<data>` is `~/.local/share/agent-sandbox`. If branch `asb/<name>` already exists, check it out instead of
   creating it.
2. Copy each `worktree_copy` entry from `<root>` into the worktree. Skip missing entries with a warning, and never
   follow symlinks out of the root. The destination side gets the same rule: an existing `asb/<name>` branch may hold
   symlinks the agent committed, so an entry whose destination is or lies under a symlink in the checkout is refused
   with a warning, and all writes go through an `os.Root` at the worktree.
3. If the main config has `extensions.worktreeConfig=true`, create an empty `.git/worktrees/<name>/config.worktree`
   so it can be overlaid read-only.

Worktree mode needs the main checkout: `<root>/.git` must be a directory. Creation runs under a per-sandbox lock so
parallel invocations do not race on `git worktree add`. The admin dir is taken from the worktree's `.git` file and
must lie directly under `<root>/.git/worktrees/`.

If the worktree directory exists but git does not know it, or the reverse: error, suggest `git worktree prune`, and
never delete anything.

**Mounts.** The worktree is mounted at its real host path. Only `<root>/.git` is mounted from the main checkout, at
its real host path; the main checkout's files are not mounted. `.git` is **read-only by default, with narrow
writable openings**:

| Path (under `<root>/.git` unless noted)                           | Mode | Reason                                                |
|-------------------------------------------------------------------|------|-------------------------------------------------------|
| `.git/`                                                           | ro   | base layer; unknown/future paths stay protected       |
| `objects/`                                                        | rw   | new loose objects from commits                        |
| `objects/pack/`, `objects/info/`                                  | ro   | history untouchable; blocks `alternates` injection    |
| `refs/heads/asb/`, `logs/refs/heads/asb/`                         | rw   | the agent can move only `asb/*` branches              |
| `worktrees/<name>/`                                               | rw   | HEAD, index, per-worktree refs, ORIG_HEAD, logs       |
| `worktrees/<name>/commondir`, `worktrees/<name>/gitdir`           | ro   | stops redirecting host git run inside the worktree    |
| `worktrees/<name>/config.worktree` (if created)                   | ro   | config injection                                      |
| `<worktree>/.git` (file)                                          | ro   | stops redirecting host git run inside the worktree    |

Before mounting, create on the host any of `refs/heads/asb/`, `logs/refs/heads/asb/`, `objects/pack/` and
`objects/info/` that are missing. A read-only overlay can only be mounted on a path that exists; without
`objects/info/`, the agent could create `objects/info/alternates` through the writable `objects/`. Mount order
matters: the ro base first, then the more specific mounts.

Session env sets, through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`: `gc.auto=0`,
`maintenance.auto=0`, `user.name=agent-sandbox (<name>)` and `user.email=agent-sandbox@localhost` (overridable with
`git_user_name` / `git_user_email`). These are conveniences, not security: the agent can unset them.

The mount plans of `ro` and `worktree` mode differ, so a running sandbox whose `git-mode` label differs from the
config is not used: the run fails and asks to stop the sandbox first.

**Works:** commit, amend, rebase, reset, branching under `asb/`, and log/diff/blame over all history.

**Fails cleanly:** `git stash` (shared `refs/stash`), tags, branches outside `asb/`, submodule operations, repack/gc.

**Residual risk** (document it): the agent can overwrite or delete loose objects the host has not packed yet. A
`--gc` flag runs `git gc` on the host before starting, which shrinks this window to zero.

**Review:** branch `asb/<name>` is visible on the host immediately, and `git worktree list` shows the worktree.

**Lifecycle:** the worktree outlives the container, and restarting `-n NAME` resumes on it.

- `worktree list`: name, branch, commits ahead of host `HEAD`, dirty flag, whether a container is running.
- `worktree rm NAME`: refuses if the container is running or the worktree is dirty unless `--force`. Runs
  `git worktree remove`. Keeps branch `asb/<name>` unless `--delete-branch`.

Not in v1: syncing new host commits into an existing worktree.

### 5.4 Running host `git`

Every host `git` invocation (worktree add/list/remove, the ahead count, the dirty check, `--gc`) goes through one
helper that sets `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0` and passes `-c core.hooksPath=/dev/null
-c core.fsmonitor=false`. The dirty check in particular runs inside a worktree the agent controls.

## 6. Failure handling

Fail closed, and never fall back to less isolation.

| Situation                                   | Behavior                                                                        |
|---------------------------------------------|---------------------------------------------------------------------------------|
| podman missing or < 5.0                     | hard error with the detected version                                            |
| config invalid / unknown key                | error naming file, key, reason                                                  |
| mount source refused / reserved env key     | error naming the entry and the rule it hit                                      |
| trust not approved                          | exit 1 with diff (TTY) or `run 'agent-sandbox trust'`                          |
| secrets.env has the wrong mode              | refuse with chmod hint                                                          |
| image build fails                           | stream output, exit non-zero, running container untouched                       |
| `_init` cannot apply nftables               | init exits non-zero; wrapper prints logs + `modprobe nf_tables` hint, removes container |
| no link-local nameserver in resolv.conf     | init exits non-zero (same handling as above)                                    |
| proxy exits (allowlist)                     | init exits non-zero; sessions die; next run prints logs and starts fresh        |
| ready not seen within 10 s                  | print logs, stop and remove container, exit non-zero                            |
| worktree mode on a reftable repo            | error explaining why; suggest `ro` mode                                         |
| container vanished before exec              | one retry of ensure-running + exec under the lock                               |
| worktree dir/registration mismatch          | error, suggest `git worktree prune`, delete nothing                             |
| post-session scan finds paths               | loud warning with paths; `--strict` refuses the next start                      |

## 7. Post-session scan

An agent can create `some/dir/.git/` with a hostile `config` (`core.fsmonitor`) or hooks. The host shell prompt
(fish runs `git` on `cd`) or the user would then run it on the host. It can likewise create a deny-list path that did
not exist when the container started (section 5.2), such as `.envrc` or `.pre-commit-config.yaml`, which is then not
protected.

- After every session exits, walk the mounted tree (project root or worktree) on the host and report:
  - every `.git` entry other than the expected top-level one. If the project had no `.git` at container start
    (root fell back to `$PWD`), a top-level `.git` is reported too. Exception: a `.git` *file* whose `gitdir:` points
    inside the protected top-level git dir (submodules, worktrees) is not reported, since what it points to is
    read-only for the agent; otherwise every repo with submodules would warn after every session;
  - every deny-list path (built-in, `core.hooksPath`, `readonly`) that exists but is not overlaid in the running
    container. The overlaid set comes from `podman inspect` of the container (this run's mount plan if it is gone);
  - every directory the walk cannot read, since it could hide a nested `.git`.
- The walk uses `lstat` and never follows symlinks. It does not descend into the top-level `.git`, and skips
  `node_modules` and `vendor`.
- Record the findings in `~/.local/share/agent-sandbox/state/<project-id>/findings` (path and reason per line).
  Findings are cumulative: recorded ones stay while their path exists, because a planted `.envrc` is overlaid
  read-only in the next sandbox and would no longer be reported, yet the host would still run it. The user clears
  them by deleting the paths or the state file.
- Print a warning listing the paths and advising the user to inspect or delete them before `cd`-ing there,
  committing, or opening the project in an editor.
- With `--strict`, or `strict = true` in config, refuse to start while that state file lists paths that still exist.

## 8. Code layout

```
cmd/agent-sandbox/main.go
internal/project     # root discovery, IDs, cwd mapping
internal/config      # TOML load/merge/validate, defaults (incl. allow_domains)
internal/trust       # hash, snapshot, diff, prompt
internal/image       # base (go:embed Containerfile) + project build, hashing, pruning
internal/podman      # thin CLI wrapper, --format json; no podman Go bindings
internal/mounts      # mount plan per git mode and deny-list (security-critical, pure functions)
internal/worktree    # git worktree add/list/rm, worktree_copy
internal/sandbox     # ensure-running, flock, exec, ready wait, stale detection
internal/initd       # _init: nftables, privilege drop, supervisor, reaper
internal/proxy       # _proxy: CONNECT allowlist proxy
internal/scan        # post-session scan (nested .git, unprotected deny-list paths) + state
internal/hostgit     # hardened host git runner, .git/config parser (hooksPath, includes, refStorage, worktreeConfig)
```

- Build: `CGO_ENABLED=0 go build -o dist/agent-sandbox ./cmd/agent-sandbox`.
- `internal/mounts` and the nftables ruleset builder in `internal/initd` must be pure. They take inputs and return a
  plan or ruleset struct, so golden tests can cover them without podman.

## 9. Testing

**Unit tests** (`go test ./...`):

- project root discovery (a `.git` dir, a `.git` file, none), ID hashing, cwd mapping;
- config merging, validation and mount-source refusal (including symlink tricks, ancestors of the project root and
  data dir, `$XDG_RUNTIME_DIR`, rw dotfiles under `$HOME`), reserved `env` keys;
- `.git/config` parsing: `core.hooksPath` (relative, absolute, via `[include]`), include targets inside the project, `extensions.refStorage`;
- trust hashing (order, mode and content sensitivity) and the snapshot diff;
- mount plans: golden files of the full `podman run` argv for ro and worktree mode, for each network mode;
- nftables rulesets: golden per mode, including `allow_cidrs`, on-link prefixes (IPv4 and global IPv6), resolver
  selection from a `resolv.conf` with both link-local and LAN nameservers;
- proxy host matching (exact, wildcard, IP literal, CONNECT parsing, plain HTTP);
- idle detection against a fake `/proc` tree;
- post-session scan against a fake tree: nested `.git`, missing top-level `.git`, new deny-list paths, symlink loops.

**Integration tests** (`//go:build integration`, real rootless podman, run with `go test -tags integration ./test/...`):
each escape attempt must fail.

- `curl` to the host LAN IP, `host.containers.internal`, the pasta gateway, and a LAN resolver on port 53
  (restricted mode); DNS through pasta's resolver still works;
- `curl https://example.com` (allowlist mode), while an allowed domain succeeds; an allowed test domain that
  resolves to a private address is refused even through the proxy;
- the session user can write its home volume; two projects get different home volumes, and a file written to `$HOME`
  in one is absent in the other; a home seed is copied into a new volume;
- `nft flush ruleset`, `kill 1`, `kill -9 <proxy pid>` as the session user;
- write to `.git/hooks/x`, `.git/config`, `.claude/settings.json`, `.agent-sandbox/config.toml`;
- worktree mode: update `refs/heads/main`, write `objects/info/alternates` (in a repo that had no `objects/info/`
  before), edit `worktrees/<name>/commondir`, edit the worktree `.git` file; a commit on `asb/<name>` succeeds and is
  visible on the host; a reftable repo is refused;
- an `_init` failure leaves logs that the wrapper prints;
- `sudo` and setuid binaries do not raise privileges;
- the idle timeout removes the container; 5 parallel invocations produce exactly one container;
- a nested `.git` and a new `.envrc` created in a session are reported after the session ends.

## 10. Suggested implementation order

1. `project`, `config`, `podman` wrapper, and a minimal `sandbox` path: open network, ro git mode, base image only,
   per-project volumes with the ownership fix, no init (`sleep infinity`), exec works end to end.
2. `initd`: privilege drop, idle supervisor, ready file. Switch the container entrypoint to `_init`.
3. Restricted network ruleset, then golden tests and integration escape tests.
4. `trust`, project images, image hashing and pruning, stale detection.
5. Deny-list overlays (including `core.hooksPath`) and the post-session scan.
6. Worktree mode.
7. Allowlist mode: proxy plus ruleset.
8. `status`, `stop`, `worktree list/rm`, `init` scaffolding, README with residual risks (section 11).

## 11. Residual risks

The README states these plainly:

- Kernel and podman escapes are out of scope (section 1).
- The agent can read the tokens in `secrets.env`. In `open` and `restricted` modes it can send them anywhere. They are
  shared by all projects, so a leaked token is a leak for all of them.
- `allowlist` reduces exfiltration but does not stop it: the agent can send data through an allowed API, for example
  with an attacker's API key.
- `open` mode can reach the host through `host.containers.internal`, and the LAN.
- `restricted` and `allowlist` block the host's addresses as of sandbox start. An address the host gains later (a VPN
  coming up, another network) is reachable from a running sandbox until it is restarted.
- Deny-list paths that did not exist at container start are not protected; the post-session scan only detects them.
- An approved project Containerfile runs its `RUN` steps on the host with podman's default (unrestricted) build
  network. The trust prompt says so; review it like any script you run.
- The agent can change any project file, including build scripts, `Makefile`, `package.json` scripts and test code
  that the user later runs on the host. Review diffs before running project code on the host.
- Worktree mode: the agent can overwrite or delete loose objects the host has not packed yet (`--gc` closes this).
- Within one project, all named sandboxes share the home and cache volumes, so state planted by one session persists
  into later sessions of the same project.
- A session can leave a background process running, which keeps the container from going idle; `stop` ends it.
