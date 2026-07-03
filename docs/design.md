# Taugres Design

This document describes the design of Taugres as built. `taugres`/Taugres is a
working project name; the CLI binary is `tau`.

## What it is

A tool for managing reproducible, deterministic development environments, in the
spirit of `direnv`, but
Starlark-configured and able to provision tools/packages. When you `cd` into a
project it configures your shell — environment variables, `PATH`, aliases, shell
functions, activation hooks — and, on request, installs the tools and packages
the project declares. Leaving the project restores the prior shell state.

Supported shells: **bash, zsh, fish**. Platforms: **Linux, macOS**.

## Performance requirement

Performance is the most important product requirement; reconfiguring the shell
must feel instant. Hard target: **a common directory change ≤ 20ms**.

How the design meets it:

- The shell hook's hot path (nothing changed) is **pure shell** — a config-dir
  walk plus a few `stat`s (~6ms measured; see `internal/cli/hook_perf_test.go`).
  It never evaluates Starlark, hashes files, or spawns a subprocess.
- Expensive work (Starlark evaluation, package installs, network) happens only
  in `tau sync`, which the hook invokes **only when a cheap staleness check
  trips**.
- Activation is delegated to `tau activate` (a subprocess) only on real
  activation *events* — entering/switching projects or after a change — not on
  every prompt.

## Non-goals

Taugres does **not**:

- run WASM components or maintain a Taugres package registry;
- implement first-class `github_release`/archive/curl-pipe-shell providers
  (delegated to mise or language managers instead);
- install system packages (apt/dnf/pacman/Homebrew) or manage services;
- mutate global shell startup files beyond the one manually installed hook;
- execute arbitrary shell code while *evaluating* config (config eval only
  mutates an in-memory plan);
- upload telemetry (there is none — see below).

## Implementation

Go, using the standard library (`flag` for the CLI — no Cobra/Viper) and
`go.starlark.net` for config evaluation. Go suits fast startup, static binaries,
Linux/macOS support, shell-script generation, and subprocess integration with
`mise`/`pip`/`npm`.

Telemetry was considered (local-only JSONL) and **removed**: nothing consumed
it, so it was dead weight. If a hosted endpoint is ever added, it can return.

### Go package layout

```text
cmd/tau                         # entrypoint
internal/
  cli/          # command routing + command implementations
  discover/     # workspace.tg/project.tg discovery, repo-root resolution
  paths/        # //-anchored path resolution
  config/       # Starlark evaluation; the shell.*/mise/pip/npm builtins
  model/        # normalized Plan (+ SourceFunc, HookScript, tool decls)
  render/       # Plan -> activate/deactivate scripts (bash/zsh/fish)
  shellhook/    # `tau hook <shell>` integration snippet (bash/zsh/fish)
  state/        # .taugres/gen metadata, manifest, staleness checks
  lock/         # .taugres.lock (version pinning)
  trust/        # allow/deny records outside the repo
  validate/     # plan validation for `tau check`
  ui/           # progress reporter + spinner
  tools/
    toolenv/    # shared helpers (Reporter, IsExecutable, ...)
    mise/       # mise integration
    pip/        # pip integration
    npm/        # npm integration
```

`render` and `shellhook` are separate because they have distinct
responsibilities (env-activation scripts vs. the cd-integration snippet). They
both branch bash/zsh/fish and could later merge into an `internal/shell/`
umbrella (with a shared shell list + quoting); that's an optional consolidation,
not a requirement. `internal/tools/` is reserved for integrations with external
tool managers — shell rendering is core output and deliberately stays out of it.

## Project layout

```text
project/
  workspace.tg          # committed root config + repository-root marker
  .taugres.lock         # committed lockfile (pinned tool/package versions)
  taugres/lib/*.tg      # optional committed Starlark helper modules
  service-a/project.tg  # optional nested project config
  .taugres/             # generated/local, git-ignored as a whole
    gen/                # activate/deactivate.{bash,zsh,fish} + manifest
    tools/
      pip/              # project-local pip virtualenv
      npm/              # project-local npm prefix
```

`.gitignore` needs only `.taugres/`. The lockfile is a separate root file
(`.taugres.lock`) so the generated directory can be ignored without exceptions.
**Trust records live outside the repo** (see Trust), so a checkout never carries
its own approval.

Config file names are fixed; Taugres does not discover arbitrary `.tg` files as
entrypoints:

- `workspace.tg` — repository-root marker and shared root config;
- `project.tg` — nested/secondary project config;
- `*.tg` elsewhere (e.g. `taugres/lib/`) — helper modules, only reachable via
  `load(...)`.

A directory must not contain both `workspace.tg` and `project.tg`.

## Core model

```text
configure -> sync -> activate
```

- **Configure** — write `workspace.tg` (and optional `project.tg` files).
- **Sync** (`tau sync`) — evaluate Starlark, validate, install declared
  tools/packages, and generate shell scripts. This is the only phase that may
  hit the network. Requires trust.
- **Activate** — the shell hook activates a *trusted* project by `eval`ing
  `tau activate`'s output; leaving restores prior state.

The hook can **auto-sync on `cd`**: a cheap staleness check decides whether to
run `tau sync --if-stale` before activating, so editing config takes effect on
the next prompt without a manual step (see Shell hook).

## Starlark configuration model

Starlark gives real functions, conditionals, deterministic evaluation, and
`load(...)` imports. The API is side-effect style: calls mutate an in-memory
plan, never the host. Host access is limited to two read-only probes for
conditional config — `exists(path)` and `which(name)` — which can read the
filesystem/PATH but never run commands or write anything.

All shell-facing configuration is grouped under the `shell` namespace; external
tool managers keep their own namespaces.

```python
project("my-app")

# Environment. Values expand $VAR / ${VAR} against earlier shell.env entries
# and the process environment.
shell.env("DATABASE_URL", "postgres://localhost/app")
shell.env("BIN", "$HOME/.local/bin")
shell.unset("PYTHONPATH")

# PATH — repository-root anchored with //.
shell.path.prepend("//node_modules/.bin")
shell.path.append("//scripts")

# Aliases.
shell.alias("ll", "ls -lah")

# Shell functions (mutate the caller shell): inline content or a file body.
shell.fn("croot", shells = ["bash", "zsh"], content = "cd $TAUGRES_PROJECT_ROOT")
shell.fn("croot", shells = ["fish"], file = "//bin/croot.fish")

# Raw activation setup, like flake.nix's shellHook.
shell.hook(shells = ["bash", "zsh"], content = "mkdir -p .cache")

# Tools/packages (see Package management). Single or an array of specs.
mise.tool(["go@1.26.2", "python"])
pip.install(["ruff@0.6.9", "rich"])
npm.install("typescript")

# Platform conditionals.
if platform.os == "linux":
    shell.env("TAUGRES_PLATFORM", "linux")
```

### API summary

```python
project(name)

shell.env(name, value)          # value expands $VAR/${VAR} (earlier env, then process env)
shell.unset(name)
shell.alias(name, value)
shell.path.prepend(entry)       # //-anchored or absolute
shell.path.append(entry)
shell.fn(name, shells=[...], content=... | file=...)   # exactly one of content/file
shell.hook(shells=[...], content=... | file=...)       # raw activation snippet

# Tools/packages: a "name@version" spec (bare name = latest) or a list of them.
# "@" is the uniform pin separator (translated to pip's "==" internally); the
# last "@" wins so npm scoped names stay intact.
mise.tool("go@1.26.2")   | mise.tool(["go@1.26.2", "python"])
pip.install("ruff@0.6.9") | pip.install(["ruff@0.6.9", "rich"])   # Python via pip
uv.install("ruff@0.6.9")  | uv.install(["ruff@0.6.9", "rich"])    # Python via uv (faster)
npm.install("typescript") | npm.install(["typescript@5.6.2", "@scope/x@1"])

platform.os                     # "linux" | "macos"
platform.arch                   # "x86_64" | "aarch64" | ...

exists("//go.mod")              # bool: root-anchored/absolute path on disk?
which("docker")                 # abs path of a PATH binary, or None

load("//taugres/lib/x.tg", "sym")   # root-anchored
load("./lib/x.tg", "sym")           # relative to the importing file
```

There is intentionally **no `bin()` builtin and no automatic `bin/`**: expose
project commands explicitly with `shell.path.prepend("//bin")`. This keeps PATH
composition predictable and the model uniform.

`shell.hook` bodies run at activation *after* env/PATH/aliases/functions, in
declaration order, inside the trust gate; they are not undone on deactivation
(fire-and-forget, like a function body). They are the escape hatch for
imperative setup.

### Root-anchored paths and imports

Config must not depend on the process cwd. Path arguments
(`shell.path.*`, `shell.fn`/`shell.hook` `file=`) are anchored:

- `//foo/bar` → `<repo-root>/foo/bar`;
- absolute paths are allowed;
- bare/relative paths (`foo`, `./foo`, `../foo`) are rejected.

`load(...)` is more flexible: besides root-anchored `//…` it also accepts
**relative** imports (`./x.tg`, `../x.tg`) resolved against the importing file's
directory. Remote (`https://`) imports are **not supported yet** and produce a
clear error.

Discovery:

1. walk upward from cwd to the first `project.tg`/`workspace.tg` — the active
   config;
2. from there, walk upward to the first `workspace.tg` — the repository root;
3. if none, the active project root is also the repo root.

For nested projects, `//` is the repository root while `TAUGRES_PROJECT_ROOT` is
the active project directory.

### Reusable helpers

```python
# taugres/lib/node.tg
def node_project():
    shell.env("COREPACK_ENABLE_DOWNLOAD_PROMPT", "0")
    shell.path.prepend("//node_modules/.bin")
    shell.alias("pn", "pnpm")
```

```python
# workspace.tg
load("//taugres/lib/node.tg", "node_project")   # or "./taugres/lib/node.tg"
project("my-node-app")
node_project()
```

### Built-in environment variables

Activation sets:

```sh
TAUGRES_ACTIVE=1
TAUGRES_ROOT=/repo                    # repository root; anchor for //
TAUGRES_REPO_ROOT=/repo               # alias for TAUGRES_ROOT
TAUGRES_PROJECT_ROOT=/repo/service-a  # active project root
TAUGRES_CONFIG=/repo/service-a/project.tg
TAUGRES_LOCK=/repo/service-a/.taugres.lock
TAUGRES_STATE=/repo/service-a/.taugres
```

## Activation / deactivation

Generated `activate.<shell>` sets env/PATH/aliases/functions and runs hooks,
saving prior values into uniquely-prefixed helper variables. `deactivate.<shell>`
restores them. Collisions are handled conservatively: existing
aliases/functions are not overwritten (a warning is printed).

The shell hook tracks the active project. Entering a different project sources
the previous project's deactivate script (which matches how it was activated),
then activates the new one. A mid-session config change re-syncs and re-activates
in place (deactivating with the *old* script before regenerating so removed
vars/PATH don't leak).

## Shell hook

Install once per shell:

```sh
eval "$(tau hook zsh)"     # ~/.zshrc
eval "$(tau hook bash)"    # ~/.bashrc  (double quotes required)
tau hook fish | source     # ~/.config/fish/config.fish
```

Wiring: zsh uses `chpwd`; fish uses `--on-variable PWD`; bash has no native
dir-change hook, so the snippet installs itself into `PROMPT_COMMAND`,
preserving any existing scalar or array (bash ≥ 5.1) value.

On each prompt the hook:

1. walks up to the nearest config dir (pure shell);
2. computes a cheap staleness signal from a set of independent **checks**, using
   only shell builtins (no subprocess on the common path);
3. if stale, runs `tau sync --if-stale` — guarded by a per-shell token
   (`_TAU_TRIED`) so a persistently failing sync is not retried until the inputs
   change (no re-sync storm);
4. (re)activates via `eval "$(tau activate <shell>)"` when entering/switching or
   when the generated env changed (tracked by `_TAU_ACT_TOKEN`).

### Staleness checks

Staleness is a set of independent dimensions, all recorded in one file —
`.taugres/gen/manifest` — written at the end of a sync. It is a greppable,
line-based format so the shell hook reads it with pure builtins (no JSON parse,
no subprocess) while Go reads the same file:

```
input:<sha256>:<abs-path>     a config input (config file, load(...) module, shell.fn/shell.hook file)
tooldir:<abs-path>            a tool bin dir that must exist
probe:<kind>|<arg>|<result>   an exists()/which() observation
toolsig:<mgr>:<sha256>        per-manager fingerprint of its tools + locked versions
```

| Dimension | Drift when… |
| --- | --- |
| input | a config input's mtime is newer than the manifest (hook) or its content hash changed (`tau status`) |
| tooldir | a recorded tool bin dir (mise store, pip/uv venv, npm prefix) is missing |
| probe | an `exists()`/`which()` result changed (a probed file appeared/vanished, a binary was installed/removed) |

The first three are the **env-trigger** dimensions the hook reads to decide
*whether* to sync. The `toolsig` lines are **Go-only** (the hook ignores unknown
tags) and drive *what work* a sync does — see Per-manager staleness.

The manifest's own mtime is the "last synced" anchor. On each prompt the hook
makes **one pass** over the file, dispatching by line tag with builtins only
(`-nt`, `-d`, `command -v`); the Go evaluators (`NeedsSync`) run the same three
dimensions **concurrently**. Only on the stale path does it read mtimes (`stat`)
to build the `_TAU_TRIED` retry token, into which the probe signal is folded so a
genuine change forces exactly one resync (no storm). Adding a dimension is a new
line tag plus a case in the dispatch.

Because everything lives in one file written last, a failed/partial sync never
marks the environment fresh, and a second shell entering during an in-progress
sync blocks on the sync lock rather than racing to a half-written env.

Critically, the hook **never sources an in-repo script for the trust decision**;
it delegates activation to `tau activate`, which refuses to emit anything for an
untrusted project. See Trust.

## Trust

Approving a project with `tau allow` records trust **outside the repo**, under
`${XDG_CONFIG_HOME:-~/.config}/taugres/trust/<sha256(configPath)>.json`. This is
the security boundary: a cloned repo cannot grant itself trust, so it cannot run
code on `cd` until the user explicitly allows it on this machine — even if the
repo ships a pre-generated `.taugres/gen/activate.*`. The hook enforces this by
delegating to `tau activate` (which checks the global record) instead of
sourcing any repo file directly.

Trust is **allow-once and path-based**: later edits to a trusted config do not
require re-approval. This is a deliberate convenience trade-off versus direnv's
content-based model (which re-prompts on every `.envrc` edit). `tau deny`
revokes; `tau prune` removes records whose config path no longer exists (the one
place global state can otherwise leak, since trust outlives deleted projects).

## Nested projects

Default behavior is **independent nearest-project activation**, not implicit
merging: in `repo/` activate `workspace.tg`; entering `repo/service-a`
deactivate the parent and activate `service-a/project.tg`; moving back reverses
it. This keeps one active environment at a time, deterministic and secure
(entering a child does not implicitly source parent configs). Share setup
explicitly via `load(...)`. Opt-in inheritance could be added later with clear
conflict rules; it is intentionally absent now.

## Package management

Package management is built. The direction is to **delegate to existing tools**,
not to build a Taugres package universe:

```text
Starlark declarations -> .taugres.lock -> mise/pip/npm install -> PATH -> fast activation
```

Goals: no system-wide installs, project-scoped exposure, independently pinned
versions, deterministic-enough installs, fast activation, no
Taugres-maintained release/archive provider.

### mise (hard dependency)

`mise` provisions runtimes and standalone tools (`mise.tool("go@1.26.2")`).
It is a **hard dependency** for any tool/package: `pip` and `npm` run on the
`python`/`node` that mise provides, so declaring `pip.install`/`npm.install`
implies an implicit `mise.tool("python")`/`mise.tool("node")`. mise is a
documented prerequisite (install: `curl https://mise.run | sh`), not something
tau bootstraps — and only the `mise` binary on PATH is needed, since tau uses
mise's store directly and never relies on `mise activate`. tau caps install
parallelism at `mise.jobs(n)` (default 10) via `mise install --jobs n`, to limit
bursts of unauthenticated GitHub API calls from the aqua/ubi backends.

Tools are exposed the way **`mise activate`** does — by prepending each tool's
real install bin dir (e.g. `~/.local/share/mise/installs/node/22.11.0/bin`) to
the activation `PATH`. There is **no symlink/wrapper farm**: an earlier farm
approach broke tools that resolve files relative to their invocation path
(notably `npm`, a node script loading `../lib/node_modules/...`). Prepending the
real dir avoids that class of bug entirely. `mise where` yields both the bin dir
and the resolved version.

### pip / uv / npm

- **pip** installs into a project-local virtualenv at `.taugres/tools/pip`
  (bin prepended), using the mise-provided Python. Never touches system Python.
- **uv** is the faster modern alternative: `uv.install(...)` creates and installs
  into a venv at `.taugres/tools/uv` with `uv pip`, using the mise-provided
  Python and an implicit mise `uv` (falling back to `uv` on PATH). Same venv/PATH
  model as pip.
- **npm** installs into a project-local prefix at `.taugres/tools/npm` via
  `npm install -g --prefix`, using mise-provided node; CLIs run directly (like
  npx).

### Execution

Installs run during `tau sync` only. mise installs all tools in one batched
invocation (mise parallelizes internally); pip, uv, and npm run **concurrently** with
each other (independent prefixes) after mise completes. Installs are
**best-effort**: a failure is reported but does not abort — the shell env (vars,
PATH, aliases, functions) is always generated so the shell still works, and the
tool can be retried. Tool output is shown only with `tau sync --verbose`;
otherwise a single spinner line reports progress.

**Per-manager staleness.** A sync often runs for a reason unrelated to tools (a
probe flip, an edited alias, `--if-stale`). Each manager therefore carries a
**signature** in the manifest — `toolsig:<mgr>:<sha256>`, a hash of its declared
tools/packages joined with their locked versions. A manager is fresh (its
install skipped, touching no network) when `--update` is unset, it was not added
or dropped since the last sync, its signature is unchanged, and its bin dirs are
present; when every manager is fresh the whole install phase is skipped and only
the shell scripts regenerate. This is a distinct *group* from the env-trigger
checks (the input/tooldir/probe dimensions in `internal/state`): the env checks
decide *whether* to sync; the signatures decide *what work* the sync does.
`--update` forces every manager.

The signatures subsume per-manager freshness, so there are no separate `Fresh()`
helpers. mise stays offline too: its store bin dirs are recovered from the
recorded `tooldir:` lines (the pip/uv/npm dirs are deterministic project-local
paths, so subtracting them leaves the mise dirs) and reused for PATH when mise is
fresh — so an unchanged mise is never re-probed with `mise where`.

If `mise` is missing, tool installs are skipped with a clear message and the env
is still generated.

### Lockfile and determinism

`.taugres.lock` (committed, JSON) pins versions and makes sync reproducible by
default. Each entry records the **requested** spec and the **resolved** concrete
version:

```json
{
  "lockfileVersion": 1,
  "mise": { "go": { "requested": "1.26.2", "resolved": "1.26.2" },
            "python": { "requested": "", "resolved": "3.12.7" } },
  "pip":  { "ruff": { "requested": "0.6.9", "resolved": "0.6.9" } },
  "npm":  { "typescript": { "requested": "", "resolved": "5.6.2" } }
}
```

Resolution rules per entry:

- **default** — if the lock has an entry whose requested spec still matches the
  config, install the locked resolved version (reproducible). Editing the
  config's version re-resolves that entry automatically.
- **`tau sync --update`** — re-resolve **all unpinned** entries (no version in
  config) to latest and rewrite the lock; pinned entries are controlled by
  editing the config.
- **`tau update <name>...`** — scope the re-resolve to specific unpinned
  entries. It drops those entries from the lock and runs a normal sync, so only
  the named ones re-resolve to latest while everything else stays locked; a
  name pinned in the config is refused. The manager is inferred from the config;
  a `<manager>:name` prefix (`mise`/`pip`/`npm`/`uv`) disambiguates a name
  declared under two managers, while a non-manager prefix (a mise backend like
  `go:goose`) is treated as a bare name. With no names it is `sync --update`.

Determinism is at the **version + package-manager-lockfile level**, not
Nix-style bit-for-bit. Artifact hashing is deliberately avoided: it is
platform-specific (breaks a shared committed lock across OSes) and shallow for
pip/npm (the ecosystem lockfiles already hash the full graph). The right future
hash is of the *ecosystem lockfile* (`package-lock.json`, `uv.lock`) once frozen
dependency installs land — see Deferred.

### Garbage collection

Taugres GCs what it exclusively owns and can reason about; it delegates the rest:

- **On sync** — dropping a `pip.install`/`npm.install` uninstalls the package
  from its project-local prefix (or removes the prefix entirely if none remain)
  and prunes the lock entry. Dropping a `mise.tool` prunes its lock entry only —
  mise's store is shared across projects, so pruning it belongs to `mise prune`,
  not Taugres.
- **PATH entries** need no cleanup — scripts are regenerated from the current
  config each sync.
- **`tau clean`** removes the regenerable `.taugres/` (keeps `.taugres.lock`, so
  the rebuild reinstalls the same versions); `tau clean --lock` also drops the
  lockfile for a from-scratch re-resolve.
- **`tau prune`** removes orphaned trust records.

## Normalized plan

Evaluating the active config produces a normalized `model.Plan` (absolute
paths), consumed by the renderers:

```json
{
  "repoRoot": "/repo",
  "projectRoot": "/repo",
  "configPath": "/repo/workspace.tg",
  "stateDir": "/repo/.taugres",
  "projectName": "demo",
  "envSet": { "DATABASE_URL": "postgres://localhost/app" },
  "envUnset": ["PYTHONPATH"],
  "pathPrepend": ["/repo/node_modules/.bin"],
  "pathAppend": ["/repo/scripts"],
  "aliases": { "ll": "ls -lah" },
  "sourceFuncs": { "croot": [{ "shells": ["bash","zsh"], "content": "..." }] },
  "hooks": [{ "shells": ["bash","zsh"], "content": "mkdir -p .cache" }],
  "miseTools": [{ "name": "go", "version": "1.26.2" }],
  "pipPackages": [{ "name": "ruff", "version": "0.6.9" }],
  "npmPackages": [{ "name": "typescript", "version": "" }],
  "pipDir": "/repo/.taugres/tools/pip",
  "npmDir": "/repo/.taugres/tools/npm"
}
```

mise tool bin dirs are added to PATH at sync time (their versioned store paths
aren't known at eval), so they don't appear in the eval-time `pathPrepend`.

## Commands

```sh
tau init [--nested]        # create workspace.tg (or project.tg)
tau check                  # evaluate + validate config
tau sync [--update]        # install tools/packages and generate scripts (needs trust)
tau sync --verbose         # print every step and tool output
tau update [name...]       # re-resolve unpinned tools/packages to latest (all, or just those named)
tau status                 # active project, sync state, tools, trust
tau allow                  # trust the active project (once)
tau deny                   # revoke trust
tau clean [--lock]         # remove .taugres/; --lock also drops .taugres.lock
tau prune                  # remove orphaned trust records
tau hook <shell>           # print the shell hook (bash|zsh|fish)
tau activate <shell>       # print the activation script for a trusted project
tau version
```

## Future, explicitly deferred

- **Frozen ecosystem-dependency installs** (`npm ci`, `uv sync --frozen`, …) and
  hashing those lockfiles in `.taugres.lock` for real supply-chain integrity;
  more language managers (Bundler, Cargo, Go modules, Composer).
- **Remote (`https://`) Starlark imports**, with content pinned by sha256 in the
  lock (portable, tamper-evident) and cache-first fetching.
- **`tau doctor`** — host requirement checks (mise/python/node presence).
- **Golden tests** for generated scripts.
- A strict mode (record mise/manager versions, require managers via mise).
- **Opt-in nested inheritance** with explicit conflict rules.
- Direct `github_release`/archive providers and a Taugres registry remain
  intentionally out of scope.

## Open questions

1. Should trust move from allow-once (path-based) toward direnv-style
   content-based re-approval, or stay convenient?
2. Should `render` + `shellhook` consolidate into a single `internal/shell/`
   package (shared shell list + quoting)?
3. When remote imports land, is cache-first + lock-pinned the right default, and
   should uncached remote imports fetch during `tau check`/`allow` or only sync?
