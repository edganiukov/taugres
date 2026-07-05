# Taugres Design

This document describes the design of Taugres as built — the *why* behind it.
For the config/syntax/CLI **reference** (the Starlark API, path rules, built-in
variables, and commands), see [reference.md](reference.md). `taugres`/Taugres is
a working project name; the CLI binary is `tau`.

## What it is

A tool for managing reproducible, deterministic development environments, in the
spirit of `direnv`, but Starlark-configured and able to provision tools/packages.
When you `cd` into a project it configures your shell — environment variables, `PATH`,
aliases, shell functions, activation hooks — and, on request, installs the tools and
packages the project declares. Leaving the project restores the prior shell state.

Supported shells: **bash, zsh, fish**. Platforms: **Linux, macOS**.

## Performance requirement

Performance is the most important product requirement; reconfiguring the shell
must feel instant. Hard target: **a common directory change ≤ 20ms**.

How the design meets it:

- Prompts outside any project are **pure shell** — a config-dir walk, no
  subprocess. In-project prompts run one warm `tau hook-env` exec (~2–3ms
  measured; see `internal/cli/hook_perf_test.go`) that does the staleness check
  in-process and prints nothing when the state is unchanged.
- Expensive work (Starlark evaluation, package installs, network) happens only
  in `tau sync`, which `hook-env` invokes **only when a cheap staleness check
  trips**.
- Activation code is emitted only on real activation *events* —
  entering/switching projects or after a change — not on every prompt.

## Non-goals

Taugres does **not**:

- implement first-class `github_release`/archive/curl-pipe-shell providers
  (delegated to mise or language managers instead);
- install system packages (apt/dnf/pacman/Homebrew) or manage services;
- mutate global shell startup files beyond the one manually installed hook;
- execute arbitrary shell code while *evaluating* config (config eval only
  mutates an in-memory plan);

## Implementation

Go, using the standard library (`flag` for the CLI — no Cobra/Viper) and
`go.starlark.net` for config evaluation. Go suits fast startup, static binaries,
Linux/macOS support, shell-script generation, and subprocess integration with
`mise`/`pip`/`npm`.

Telemetry was considered (local-only JSONL) and **removed**: nothing consumed
it, so it was dead weight. If a hosted endpoint is ever added, it can return.

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

Config file names are fixed (`workspace.tg`, `project.tg`, and `load(...)`-only
helper modules); a directory must not contain both `workspace.tg` and
`project.tg`. See [reference.md](reference.md#config-files) for the rules.

## Core model

```text
configure -> sync -> activate
```

- **Configure** — write `workspace.tg` (and optional `project.tg` files).
- **Sync** (`tau sync`) — evaluate Starlark, validate, install declared
  tools/packages, and generate shell scripts. This is the only phase that may
  hit the network. Requires trust.
- **Activate** — the shell hook activates a *trusted* project by `eval`ing
  `tau hook-env`'s output; leaving restores prior state.

The hook **auto-syncs**: a cheap staleness check inside `hook-env` decides
whether to run a sync before activating, so editing config takes effect on the
next prompt without a manual step (see Shell hook).

## Starlark configuration model

Starlark gives real functions, conditionals, deterministic evaluation, and
`load(...)` imports. The API is side-effect style: calls mutate an in-memory
plan, never the host. Host access is limited to read-only probes for conditional
config — `exists(path)`, `which(name)`, and `env(name)` — which can read the
filesystem/PATH/environment but never run commands or write anything.
`shell.dotenv(path)` likewise only *reads* a `.env` file into the plan's
environment.

Running a command is the one thing evaluation must never do — `tau
check`/`status`/`allow` evaluate *untrusted* configs, so eval-time execution
would let a clone run code on inspection. `shell.exec(command, dynamic=…)`
preserves this: it records a **deferred** directive (like `mise.tool` records a
version to resolve later) rather than executing. A static entry runs at `tau
sync` (trust-gated, after installs) and its output is baked into the activation
script; a dynamic entry is rendered as a command substitution that runs in the
shell on each activation. The result feeds `shell.env`; being deferred, it can't
be branched on at eval — that's what the probes are for.

All shell-facing configuration is grouped under the `shell` namespace; external
tool managers keep their own namespaces. The full API surface, path-anchoring
rules, reusable-helper pattern, and the built-in `TAUGRES_*` variables live in
[reference.md](reference.md#configuration-api).

Two decisions worth calling out here. There is intentionally **no `bin()`
builtin and no automatic `bin/`**: project commands are exposed explicitly with
`shell.path.prepend("//bin")`, keeping PATH composition predictable. And
`shell.hook` bodies run at activation *after* env/PATH/aliases/functions, in
declaration order, inside the trust gate; they are not undone on deactivation
(fire-and-forget, like a function body) — the escape hatch for imperative setup.

Config must not depend on the process cwd, so path arguments are root-anchored
(`//…`) or absolute and bare/relative paths are rejected; `load(...)` also
accepts relative imports against the importing file. Remote (`https://`) imports
are not supported yet (see [reference.md](reference.md#path-anchoring-and-imports)
and Deferred).

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

### Non-interactive activation (`tau exec`)

`tau exec [--] <cmd> [args...]` runs a command with the project's environment
applied — env vars (including `shell.dotenv`) and a `PATH` that includes the
provisioned tool bin dirs — **without** a shell hook. It is the shell-agnostic
slice of an activation, for editors, CI, `Makefile`s, and one-off invocations.
Shell-only features (aliases, functions, `shell.hook`) are deliberately *not*
applied: they mutate a live shell, not a spawned process.

It builds the child environment directly from the plan rather than parsing the
generated shell scripts: the ambient environment with the plan's env set/unset
applied, the `TAUGRES_*` built-ins, and `PATH` prefixed by the tool bin dirs
(mise store dirs recovered from the manifest, package dirs from the plan). It is
**trust-gated** like activation — env vars from an untrusted config could subvert
the command (`PATH`, `LD_PRELOAD`, …) — and it **auto-syncs when stale** (best
effort, like the hook) so freshly-declared tools are present, then execs the
command and propagates its exit code.

## Shell hook

Install once per shell (see [reference.md](reference.md#installing-the-shell-hook)
for the exact snippets).

Wiring: zsh uses `chpwd`+`precmd`; fish uses the `fish_prompt` event; bash has
no native prompt hook, so the snippet installs itself into `PROMPT_COMMAND`,
preserving any existing scalar or array (bash ≥ 5.1) value.

The hook itself is a **minimal shim** (direnv-style) — it holds no state machine
and parses nothing:

1. it walks up to the nearest config dir with pure shell — prompts outside any
   project (with nothing active) spawn no subprocess;
2. otherwise it runs one `tau hook-env <shell>` and `eval`s its stdout.

`tau hook-env` owns everything in Go: staleness, the retry guard, auto-sync,
trust, and activation/deactivation. Session state round-trips through the
`TAUGRES_HOOK` env var, which the eval'd output itself sets —
`<applied>|<stamp>|<fp>|<proj>` (whether this shell sourced the activate script,
the script's mtime, the failed-sync retry fingerprint, and the project root).
hook-env computes the desired state and emits at most one transition; on an
unchanged state — the common case — it prints nothing. The leading applied bit
keeps the "not trusted" notice at once per shell and lets the shim skip tau
entirely outside projects. Because the token is exported, a child shell inherits
it — but aliases/functions don't survive a fork — so the shim also keeps an
**unexported** `_TAU_APPLIED` flag and passes it back as an argument: an
inherited "applied" claim reconciles to false and the nested shell re-activates.

This is both simpler and faster than the previous pure-shell hook, which parsed
the manifest with builtins on every prompt: one warm Go exec (~2–3ms) beats the
shell loop plus the `stat` subprocess it needed for its activation stamp
(~4–5ms), and it stays flat as the manifest grows.

### Staleness checks

Staleness is a set of independent dimensions, all recorded in one file —
`.taugres/gen/manifest` — written at the end of a sync (its own mtime is the
"last synced" anchor):

```
input:<sha256>:<abs-path>     a config input (config file, load(...) module, shell.fn/shell.hook/shell.dotenv file)
tooldir:<abs-path>            a tool bin dir that must exist
probe:<kind>|<arg>|<result>   an exists()/which()/env() observation
toolsig:<mgr>:<sha256>        per-manager fingerprint of its tools + locked versions
```

| Dimension | Drift when… |
| --- | --- |
| input | a config input's mtime is newer than the manifest (`NeedsSync`) or its content hash changed (`tau status`) |
| tooldir | a recorded tool bin dir (mise store, pip/uv venv, npm prefix) is missing |
| probe | an `exists()`/`which()`/`env()` result changed (a probed file appeared/vanished, a binary was installed/removed, an env var changed) |

The first three are the **env-trigger** dimensions (`state.NeedsSync`, run
concurrently) that decide *whether* to sync. The `toolsig` lines drive *what
work* a sync does — see Per-manager staleness.

An `env()` probe records a **hash** of the observed value (not the value), so
secrets never land in the on-disk manifest and a value containing `|` can't
break the probe line. A set-but-empty value stays distinct from unset. `env()`
observations come from the environment `tau` runs in — the shell's environment at
that prompt — so changing a probed variable re-syncs on the next prompt. Probing
a variable tau itself mutates (e.g. `PATH`) would resync every activation and is
a footgun to avoid.

**Retry guard.** A failed auto-sync must not be re-run on every prompt (it
would re-evaluate Starlark and re-print its error). After a failed attempt,
`hook-env` records a fingerprint of the trigger state (`state.SyncFingerprint`:
newest input mtime, script/tool-dir presence, probe results) in the session
token and retries only when it changes — so the error prints once per shell per
state, with no on-disk state at all. Untrusted projects need no guard: trust is
re-checked in-process on every prompt (a stat), the sync is simply never
attempted, and `tau allow` therefore takes effect on the very next prompt.

Because everything lives in one file written last, a failed/partial sync never
marks the environment fresh, and a second shell entering during an in-progress
sync blocks on the sync lock rather than racing to a half-written env.

Critically, the shell **never sources an in-repo script for the trust
decision**; `tau hook-env` refuses to emit activation code for an untrusted
project. See Trust.

## Trust

Approving a project with `tau allow` records trust **outside the repo**, under
`${XDG_CONFIG_HOME:-~/.config}/taugres/trust/<sha256(configPath)>.json`. This is
the security boundary: a cloned repo cannot grant itself trust, so it cannot run
code on `cd` until the user explicitly allows it on this machine — even if the
repo ships a pre-generated `.taugres/gen/activate.*`. The hook enforces this by
delegating to `tau hook-env` / `tau activate` (which check the global record)
instead of sourcing any repo file directly.

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
implies an implicit `mise.tool("python")`/`mise.tool("node")` (and `uv.install`
an implicit `python` + `uv`). mise is a documented prerequisite (install: `curl
https://mise.run | sh`), not something tau bootstraps — and only the `mise`
binary on PATH is needed, since tau uses mise's store directly and never relies
on `mise activate`. tau caps install parallelism at `mise.jobs(n)` (default 10)
via `mise install --jobs n`, to limit bursts of unauthenticated GitHub API calls
from the aqua/ubi backends.

The implicit runtime defaults to latest (pinned in the lock on first sync). To
**pin the version a package manager runs on**, declare the runtime explicitly —
`mise.tool("python@3.12.7")`, `mise.tool("node@22.11.0")`, `mise.tool("uv@…")` —
and the implicit add is skipped (it is keyed by tool name, so any explicit
declaration wins). This keeps the model uniform: there is one way to name a
tool/version, and a runtime is just a tool you may or may not spell out.

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

## Roadmap

- **tau build <binary>**: option to build binaries from local code.
- **Remote (`https://`) Starlark imports**, with content pinned by sha256 in the
  lock (portable, tamper-evident) and cache-first fetching.
- **`tau doctor`** — host requirement checks (mise/python/node presence).
- **`tau env [--json]`** — dump the resolved environment (env vars + `PATH`) for
  editor/IDE integration, CI, and debugging. Shares the plan→environ computation
  that backs `tau exec`.
- **`tau why <VAR>`** — trace an env var or `PATH` entry back to the config line
  (or `load(...)`ed helper / `shell.dotenv` file) that produced it. Builds on the
  normalized `Plan`.
- **Auto-adopt existing version files** — read `.tool-versions` / `.nvmrc` /
  `.python-version` and feed them to `mise.tool`, as an explicit opt-in builtin
  (e.g. `mise.tool_versions("//.tool-versions")`) rather than implicit discovery,
  with the file tracked as a config input for staleness.
- A strict mode (record mise/manager versions, require managers via mise).
- **Opt-in nested inheritance** with explicit conflict rules.

## Open questions

1. When remote imports land, is cache-first + lock-pinned the right default, and
   should uncached remote imports fetch during `tau check`/`allow` or only sync?
