# Getting started with tau

tau configures your shell per directory from a Starlark config (`workspace.tg`
or a nested `project.tg`) and, on request, provisions the tools/packages the
project declares. See the [README](../README.md) for install + a quick start,
and [design.md](design.md) for the full design.

## The shell hook

The quickest way is `tau setup`, which appends the hook to your shell's startup
file (idempotently). It defaults to the current shell (`$SHELL`); pass one to
override:

```sh
tau setup            # current shell
tau setup bash       # a specific shell
```

If `mise` (used to provision tools) is not on `PATH`, `tau setup` also offers to
install it: it downloads mise's installer from `https://mise.run` and runs it,
only after you confirm (no `curl` needed). `tau setup --yes` installs it without
prompting; the by-hand equivalent is `curl https://mise.run | sh`.

Or install it by hand, once per shell:

```sh
eval "$(tau hook zsh)"      # ~/.zshrc     (double quotes required)
eval "$(tau hook bash)"     # ~/.bashrc    (double quotes required)
tau hook fish | source      # ~/.config/fish/config.fish
```

For zsh/bash the double quotes are required — without them the shell word-splits
the hook and it silently fails to install. On bash the hook installs itself into
`PROMPT_COMMAND` so it runs on every `cd` (bash has no native directory-change
hook); it preserves any existing `PROMPT_COMMAND` (scalar or the bash 5.1+ array
form), but to be safe put the `eval` line **at the end of `~/.bashrc`**, after
your own `PROMPT_COMMAND` setup. zsh uses `precmd` (avoiding a duplicate
`chpwd` invocation); fish uses the `fish_prompt` event.

### Auto-sync on `cd`

The hook is a tiny shim: inside a project it runs one `tau hook-env`, which
checks whether any config input changed since the last sync (or a generated
script / tool dir went missing) and, only if needed, runs `tau sync` for you
before re-activating — so editing `workspace.tg`, a loaded module, or a
`shell.fn`/`shell.hook` file takes effect on your next prompt with no manual
step, even without leaving the project. Prompts outside any project spawn
nothing. Concurrent syncs are serialized by a per-project lock; a failing sync
isn't retried until inputs change (see [Performance](#performance)).

## Trust

`tau allow` trusts a project once; later edits to its config do **not** require
re-approving. Run `tau deny` to revoke.

Trust is recorded **outside the repo** (under
`${XDG_CONFIG_HOME:-~/.config}/taugres/trust/`, keyed by config path) — a cloned
repo cannot grant itself trust, so it can't run code on `cd` until you explicitly
`tau allow` on your machine. The shell hook delegates activation to
`tau hook-env`, which refuses to emit activation code for an untrusted project;
the shell never sources an in-repo file that could be forged. `tau prune`
removes trust records for projects that no longer exist.

## Config example (`workspace.tg`)

```python
project("my-app")

shell.env("DATABASE_URL", "postgres://localhost/app")
shell.unset("PYTHONPATH")

# Store a command's output in a variable. Runs at sync (baked, default) or, with
# dynamic=True, in the shell on each activation. Never runs during evaluation.
shell.env("GIT_SHA", shell.exec("git rev-parse --short HEAD"))

# Load KEY=VALUE pairs from a .env file (values literal, no $ expansion).
shell.dotenv("//.env")

# Paths are repository-root anchored with //.
shell.path.prepend("//node_modules/.bin")
shell.path.append("//scripts")

shell.alias("ll", "ls -lah")

# Shell functions: body from a file...
shell.fn("croot", shells = ["bash", "zsh"], file = "//bin/croot.sh")
# ...or inline:
shell.fn("hi", shells = ["bash", "zsh"], content = "echo hello $1")

# Raw setup run at activation (after env/PATH/aliases), like flake.nix shellHook.
shell.hook(shells = ["bash", "zsh"], content = "mkdir -p .cache")

if platform.os == "linux":
    shell.env("TAUGRES_PLATFORM", "linux")
```

All shell-facing configuration lives under the `shell` namespace (`shell.env`,
`shell.unset`, `shell.alias`, `shell.path.prepend/append`, `shell.fn`,
`shell.hook`, `shell.dotenv`, `shell.exec`); tool managers keep their own (`mise.tool`, `pip.install`,
`uv.install`, `npm.install`). Expose project commands by adding their directory
to `PATH` explicitly, e.g. `shell.path.prepend("//bin")`. Reusable helpers can be
loaded: `load("//taugres/lib/node.tg", "node_project")`.

### Conditional config: `exists`, `which`, and `env`

Three read-only host probes let a config adapt to what's on the machine:

```python
# exists(path) -> bool: is a root-anchored ("//…") or absolute path on disk?
if exists("//go.mod"):
    mise.tool("go@1.26.2")

# which(name) -> the absolute path of a binary on PATH, or None.
if which("docker"):
    shell.alias("dc", "docker compose")

go = which("go")   # "/usr/bin/go" or None
if go:
    shell.env("GOBIN", go)

# env(name, default="") -> a process env var's value, or the default when unset.
if env("CI"):
    mise.jobs(4)
shell.env("PROFILE", env("TAU_PROFILE", "dev"))
```

They only *read* the environment — never run a command or write anything.
Because their result depends on the host filesystem/PATH/environment, treat them
as an escape hatch: a config that branches on them is only as reproducible as
what it probes.

Probe results are recorded at sync time and re-checked on every in-project
prompt, so creating the probed file, installing the binary, or changing the env
var auto-syncs on your next prompt just like a config edit does. (`which`
detection is by presence; a binary that *moves* without a presence change is
picked up by `tau status`/manual sync. `env` values are recorded as a hash, so
secrets never land in `.taugres/`. Avoid probing a variable tau itself mutates,
like `PATH`, or it will re-sync on every activation.)

## Tools and packages

Declare tools/packages and `tau sync` installs them into `.taugres/` and adds
them to `PATH` automatically on activation. Activation itself never calls these
managers, keeping `cd` fast. Resolved versions are pinned in `.taugres.lock`.

Each call takes a `"name@version"` spec (bare `name` = latest) or a list of them.
`@` is the uniform pin separator (translated to pip's `==` internally; the last
`@` wins, so npm scoped names like `@angular/cli` stay intact):

```python
# Runtimes/CLIs via mise (https://mise.jdx.dev)
mise.tool(["node@22.11.0", "ripgrep"])   # ripgrep unpinned -> latest
mise.tool("go@1.26.2")                   # single spec

# Python packages via pip, into a project-local venv (.taugres/tools/pip)
pip.install(["ruff@0.6.9", "rich"])

# ...or via uv (faster; manages the venv itself), into .taugres/tools/uv
uv.install(["ruff@0.6.9", "rich"])

# Node packages via npm, into a project-local prefix (.taugres/tools/npm).
# Their CLIs become runnable directly — like npx, but resolved locally.
npm.install(["typescript@5.6.2", "@angular/cli@17"])
```

How each is exposed on PATH (activation prepends real bin dirs directly, the way
`mise activate` works — no symlink/wrapper farm):

- **mise** installs into its user-global store; tau prepends each tool's real
  store bin dir (e.g. `~/.local/share/mise/installs/node/22.11.0/bin`).
- **pip** installs into a project-local venv at `.taugres/tools/pip`; tau
  prepends `.taugres/tools/pip/bin` (which also gives the project its own
  `python`). Never touches system Python.
- **uv** is the faster modern alternative to pip: `uv.install(...)` creates and
  installs into a venv at `.taugres/tools/uv` (implies an implicit mise `uv` +
  `python`); tau prepends `.taugres/tools/uv/bin`. (Mixing `pip` and `uv` makes
  two separate venvs — prefer one.)
- **npm** installs into a project-local prefix at `.taugres/tools/npm` via
  `npm install -g --prefix`; tau prepends `.taugres/tools/npm/bin`.

### mise backends: install almost anything

Beyond its short names, `mise.tool(...)` accepts mise's backends, so most tools
need no dedicated tau builtin — prefix the spec with the backend:

```python
mise.tool([
    "go:github.com/pressly/goose/v3/cmd/goose",   # a Go module's binary
    "cargo:ripgrep",                               # a Rust crate
    "npm:typescript",                              # an npm global
    "pipx:ruff",                                   # an isolated Python CLI
    "ubi:cli/cli",                                 # a GitHub release (owner/repo)
    "aqua:pressly/goose",                          # via the aqua registry
])
```

`@version` still applies (`"cargo:ripgrep@14.1.1"`). Backends that hit the GitHub
API (`ubi`/`aqua`/github releases) need `GITHUB_TOKEN`/`MISE_GITHUB_TOKEN` set to
avoid unauthenticated rate limits. tau also caps how many tools mise installs in
parallel — default 16, override with `mise.jobs(n)` — which passes `--jobs n` to
`mise install` and helps avoid bursts of unauthenticated API calls.

**mise is a hard dependency** for tools/packages: `pip`/`uv`/`npm` run on the
`python`/`node` that mise provisions (declaring `pip.install`/`uv.install`/
`npm.install` implies an implicit `mise.tool("python")`/`mise.tool("node")`, and
`uv.install` also an implicit `uv`). Those implicit runtimes default to latest;
to **pin the version they run on**, just declare the runtime yourself and it
replaces the implicit one:

```python
pip.install("ruff@0.6.9")
mise.tool("python@3.12.7")   # pip/uv run on this Python
npm.install("typescript")
mise.tool("node@22.11.0")    # npm runs on this Node
```

Tool output is shown only with `tau sync --verbose`; otherwise a single progress
line is shown. Installs are best-effort — if one fails, the shell env is still
generated and the failure is reported.

Note the distinction from committed scripts: your own `bin/` scripts are exposed
explicitly with `shell.path.prepend("//bin")`, while everything under `.taugres/`
is generated, git-ignored, and auto-prepended by tau.

### Lockfile & versions

Versions are pinned by default in a committed `.taugres.lock` (a root file,
*not* under the ignored `.taugres/`). On first sync tau records each tool's
resolved concrete version; subsequent syncs install exactly that, so an unpinned
`mise.tool("node")` stays put instead of drifting to "latest". Each entry stores
both the requested spec and the resolved version, so editing a version in the
config automatically re-resolves that entry.

- `tau sync` — reproducible: install locked versions. Skips a manager whose
  declared set is unchanged and whose bin dirs are present.
- `tau sync --force [mgr...]` — reinstall even when unchanged, at the **locked**
  versions (no re-resolve). With no names it forces every manager; otherwise just
  the named ones (`mise`/`pip`/`npm`/`uv`) — e.g. `tau sync --force pip` when a
  venv got corrupted. tau-owned prefixes (pip/uv/npm) are wiped and rebuilt; mise
  runs `mise install --force`.
- `tau sync --update` — re-resolve **all unpinned** tools/packages to their
  latest and rewrite the lock. (Pinned entries are controlled by editing the
  config.)
- `tau update <name>...` — re-resolve just the named unpinned entries to their
  latest, leaving everything else at its locked version, and report `old -> new`.
  The manager is inferred from where you declared the tool; qualify as
  `<manager>:name` (`pip:ruff`, `uv:ruff`, `mise:node`, `npm:typescript`) to
  disambiguate a package declared under two managers. Updating a pinned entry is
  refused (edit its version in the config instead); `tau update` with no names is
  equivalent to `tau sync --update`.

### Removing tools (GC)

Dropping a `mise.tool`/`pip.install`/`uv.install`/`npm.install` from the config
and running `tau sync` cleans up: its PATH entry is gone (scripts are
regenerated), its lock entry is pruned, and pip/uv/npm packages are uninstalled
from their project-local prefix (a fully-removed manager's dir is deleted). mise
tools live in mise's shared store, so only their lock entry is dropped.

## Commands

| Command | Description |
| --- | --- |
| `tau init [--nested]` | create `workspace.tg` (or `project.tg`) |
| `tau check` | evaluate + validate config, report warnings/errors |
| `tau sync [--verbose] [--update] [--force [mgr...]]` | evaluate config, install tools, generate shell scripts (requires trust); `--force` reinstalls even if unchanged |
| `tau update [name...]` | re-resolve unpinned tools/packages to latest (all, or just those named; `<manager>:name` to disambiguate) |
| `tau exec [--] <cmd>...` | run a command with the project env/PATH applied, no shell hook (requires trust) |
| `tau status` | show active project, sync state, and trust |
| `tau hook <shell>` | print the shell hook (bash, zsh, fish) |
| `tau hook-env <shell>` | used by the hook: print env/activation commands for this prompt |
| `tau activate [shell]` | print the activation script for a trusted project (default: `$SHELL`) |
| `tau deactivate [shell]` | print the deactivation script for a trusted project (default: `$SHELL`) |
| `tau allow` / `tau deny` | trust / revoke trust for the active config |
| `tau clean [--lock\|--cache]` | remove `.taugres/`; `--lock` also drops `.taugres.lock`; `--cache` drops only the sync cache (next sync re-derives, no reinstall) |
| `tau prune` | remove trust records for projects that no longer exist |
| `tau version` | print version |

## Performance

Prompts outside any project are pure shell — a config-dir walk, no subprocess
(<1ms). Inside a project, each prompt runs one `tau hook-env`, which does the
staleness check in-process and prints nothing when the state is unchanged
(~1–3ms; see `internal/cli/hook_perf_test.go`).

Real work happens only on real events:

- **On a change**, `hook-env` runs a sync (which *does* evaluate Starlark and
  may run mise/pip/npm). The staleness check trips when any recorded **config
  input** — the active config file, a `load(...)` module, or a
  `shell.fn`/`shell.hook` file — is newer than the last completed sync, or a
  generated script / tool directory is missing, or an `exists()`/`which()`
  probe flipped.
- **On (re)activation** (entering/switching projects, or after a change),
  `hook-env` emits the activation script for the shell to `eval` — after
  enforcing trust. This is the security boundary: the shell never sources an
  in-repo file that tau did not vouch for.

So you **always get the latest on the next prompt after an edit**, even without
leaving the project. A persistently failing sync is not retried until the inputs
change again (tracked in the session token, once per shell), so there is no
re-sync storm.

## Root-anchored paths

Path arguments (`shell.path.prepend/append`, `shell.fn file=`) are repo-anchored
for portability:

- `//foo/bar` → `<repo-root>/foo/bar`
- absolute paths are allowed
- bare/relative paths (`foo`, `./foo`, `../foo`) are rejected

`load(...)` is more flexible: besides root-anchored `//taugres/lib/foo.tg`, it
also accepts **relative** imports (`./lib/foo.tg`, `../shared/foo.tg`) resolved
against the importing file's directory — natural for composing modules. Remote
(`https://…`) imports are not supported yet.

For nested projects `//` still points at the repository root (nearest
`workspace.tg`), while `TAUGRES_PROJECT_ROOT` is the active project directory.

## Not yet implemented (deferred per plan)

- frozen/lockfile-based dependency installs (npm ci, uv sync --frozen, …) and
  hashing the ecosystem lockfiles
- remote (`https://`) Starlark imports
