# Getting started with tau

tau configures your shell per directory from a Starlark config (`workspace.tg`
or a nested `project.tg`) and, on request, provisions the tools/packages the
project declares. See the [README](../README.md) for install + a quick start,
and [design.md](design.md) for the full design.

## The shell hook

Install once per shell:

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
your own `PROMPT_COMMAND` setup. zsh uses `chpwd`; fish uses `--on-variable PWD`.

### Auto-sync on `cd`

On every prompt/`cd` the hook does a cheap pure-shell check (is any config input
newer than the last sync, or a generated script / tool dir missing?) and, only
if needed, runs `tau sync` for you before re-activating — so editing
`workspace.tg`, a loaded module, or a `shell.fn`/`shell.hook` file takes effect
on your next prompt with no manual step, even without leaving the project. The
common case (nothing changed) never shells out to `tau`, keeping the hot path
fast. Concurrent syncs are serialized by a per-project lock; a failing sync isn't
retried until inputs change (see [Performance](#performance)).

## Trust

`tau allow` trusts a project once; later edits to its config do **not** require
re-approving. Run `tau deny` to revoke.

Trust is recorded **outside the repo** (under
`${XDG_CONFIG_HOME:-~/.config}/taugres/trust/`, keyed by config path) — a cloned
repo cannot grant itself trust, so it can't run code on `cd` until you explicitly
`tau allow` on your machine. The shell hook delegates activation to
`tau activate`, which refuses to emit anything for an untrusted project; it never
sources an in-repo file that could be forged. `tau prune` removes trust records
for projects that no longer exist.

## Config example (`workspace.tg`)

```python
project("my-app")

shell.env("DATABASE_URL", "postgres://localhost/app")
shell.unset("PYTHONPATH")

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
`shell.hook`); tool managers keep their own (`mise.tool`, `pip.install`,
`uv.install`, `npm.install`). Expose project commands by adding their directory
to `PATH` explicitly, e.g. `shell.path.prepend("//bin")`. Reusable helpers can be
loaded: `load("//taugres/lib/node.tg", "node_project")`.

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
parallel — default 10, override with `mise.jobs(n)` — which passes `--jobs n` to
`mise install` and helps avoid bursts of unauthenticated API calls.

**mise is a hard dependency** for tools/packages: `pip`/`uv`/`npm` run on the
`python`/`node` that mise provisions (declaring `pip.install`/`uv.install`/
`npm.install` implies an implicit `mise.tool("python")`/`mise.tool("node")`).
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

- `tau sync` — reproducible: install locked versions.
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
| `tau sync [--verbose] [--update]` | evaluate config, install tools, generate shell scripts (requires trust) |
| `tau update [name...]` | re-resolve unpinned tools/packages to latest (all, or just those named; `<manager>:name` to disambiguate) |
| `tau status` | show active project, sync state, and trust |
| `tau hook <shell>` | print the shell hook (bash, zsh, fish) |
| `tau activate <shell>` | print the activation script for a trusted project |
| `tau allow` / `tau deny` | trust / revoke trust for the active config |
| `tau clean [--lock]` | remove `.taugres/`; `--lock` also drops `.taugres.lock` |
| `tau prune` | remove trust records for projects that no longer exist |
| `tau version` | print version |

## Performance

On a typical prompt/`cd` where nothing changed, the shell hook does almost no
work: it walks up to the nearest config directory and a few `stat`s decide there
is nothing to do (~6ms — see `internal/cli/hook_perf_test.go`). It never parses
the manifest or hashes files inline, and spawns no subprocess.

It shells out only on real events:

- **On a change**, it runs `tau sync --if-stale` (which *does* evaluate Starlark
  and may run mise/pip/npm). The staleness check trips when any recorded
  **config input** — the active config file, a `load(...)` module, or a
  `shell.fn`/`shell.hook` file — is newer than the last completed sync, or a
  generated script / tool directory is missing.
- **On (re)activation** (entering/switching projects, or after a change), it
  runs `tau activate`, which enforces trust and emits the script to `eval`. This
  is the security boundary — the hook never sources an in-repo file directly.

So you **always get the latest on the next prompt after an edit**, even without
leaving the project. A persistently failing sync is not retried until the inputs
change again (a per-shell guard), so there is no re-sync storm.

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
