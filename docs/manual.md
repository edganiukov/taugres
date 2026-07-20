# Taugres Manual

tau configures your shell per directory from a Starlark config (`workspace.tg` or a nested `project.tg`) and, on
request, provisions the tools/packages the project declares. This is the full guide and reference; see the
[README](../README.md) for a quick start and [design.md](design.md) for the *why* behind the design.

## Contents

1. [Overview](#1-overview)
2. [Installation](#2-installation)
   - [2.1 Install tau](#21-install-tau)
   - [2.2 The shell hook](#22-the-shell-hook)
3. [Core concepts](#3-core-concepts)
   - [3.1 Config files and discovery](#31-config-files-and-discovery)
   - [3.2 Trust](#32-trust)
   - [3.3 Auto-sync on `cd`](#33-auto-sync-on-cd)
   - [3.4 Paths, anchoring and imports](#34-paths-anchoring-and-imports)
4. [Configuration reference](#4-configuration-reference)
   - [4.1 Environment](#41-environment)
   - [4.2 PATH](#42-path)
   - [4.3 Aliases, functions and hooks](#43-aliases-functions-and-hooks)
   - [4.4 Deferred values](#44-deferred-values)
   - [4.5 Host probes](#45-host-probes)
   - [4.6 Tools and packages](#46-tools-and-packages)
   - [4.7 Reusable helpers](#47-reusable-helpers)
   - [4.8 Platform](#48-platform)
   - [4.9 API summary](#49-api-summary)
5. [Managing tools and packages](#5-managing-tools-and-packages)
   - [5.1 How tools are exposed on PATH](#51-how-tools-are-exposed-on-path)
   - [5.2 The lockfile and versions](#52-the-lockfile-and-versions)
   - [5.3 Removing tools (GC)](#53-removing-tools-gc)
6. [Reference](#6-reference)
   - [6.1 Commands](#61-commands)
   - [6.2 Built-in environment variables](#62-built-in-environment-variables)
7. [Performance](#7-performance)
8. [Limitations](#8-limitations)

## 1. Overview

tau is a per-directory environment manager in the spirit of `direnv`, but Starlark-configured and able to provision
tools/packages. When you `cd` into a project it configures your shell — environment variables, `PATH`, aliases, shell
functions, activation hooks — and, on request, installs the tools and packages the project declares. Leaving the project
restores the prior shell state.

Supported shells: **bash, zsh, fish**. Platforms: **Linux, macOS**.

The model is three stages:

```text
configure -> sync -> activate
```

- **Configure** — write `workspace.tg` (and optional nested `project.tg` files).
- **Sync** (`tau sync`) — evaluate the Starlark config, validate it, install declared tools/packages, and generate the
  shell scripts. This is the only phase that may hit the network; it requires trust.
- **Activate** — on each prompt the shell hook activates a *trusted* project by `eval`ing `tau hook-env`'s output;
  leaving restores prior state. The hook auto-syncs first when config changed (see
  [3.3 Auto-sync on `cd`](#33-auto-sync-on-cd)).

## 2. Installation

### 2.1 Install tau

See the [README](../README.md) for installing the `tau` binary. To provision tools/packages, tau also needs
[`mise`](https://mise.jdx.dev) on `PATH` — `tau setup` offers to install it for you (below), or install it yourself with
`curl https://mise.run | sh`. Without mise, environments still activate but no tools are installed.

### 2.2 The shell hook

The quickest way is `tau setup`, which appends the hook to your shell startup file (idempotently). It defaults to the
current shell (using `$SHELL` env var) or you can pass one to override:

```sh
tau setup            # current shell
tau setup bash       # a specific shell
```

If `mise` (used to install tools) is not in `PATH`, `tau setup` also offers to install it: it downloads installer from
`https://mise.run` and runs it with your confirm. `--yes` to install it without prompting. It is an equivalent of
`curl https://mise.run | sh`.

Or configure the hook manually:

```sh
eval "$(tau hook zsh)"      # ~/.zshrc double quotes required)
eval "$(tau hook bash)"     # ~/.bashrc double quotes required)
tau hook fish | source      # ~/.config/fish/config.fish
```

On bash the hook installs itself into `PROMPT_COMMAND` so it runs on every `cd` (bash has no native directory-change
hook); it preserves any existing `PROMPT_COMMAND`, but to be safe put the `eval` line **at the end of `~/.bashrc`**,
after your own `PROMPT_COMMAND` setup. zsh uses `precmd` (avoiding a duplicate `chpwd` invocation); fish uses the
`fish_prompt` event.

## 3. Core concepts

### 3.1 Config files and discovery

Config file names are fixed; tau does not discover arbitrary `.tg` files as entrypoints:

- `workspace.tg` — repository-root marker and shared root config;
- `project.tg` — nested/secondary project config;
- `*.tg` elsewhere (e.g. `taugres/lib/`) — helper modules, only reachable via `load(...)`.

A directory must not contain both `workspace.tg` and `project.tg`.

Discovery walks upward from the current directory:

1. from cwd to the first `project.tg`/`workspace.tg` — the active config;
2. from there, upward to the first `workspace.tg` — the repository root;
3. if none, the active project root is also the repo root.

For nested projects, `//` is the repository root while `TAUGRES_PROJECT_ROOT` is the active project directory.

### 3.2 Trust

`tau allow` trusts a project once. Later edits to its config do **not** require re-approving. Run `tau deny` to revoke.

Trust is recorded **outside the repo** (under `${XDG_CONFIG_HOME:-~/.config}/taugres/trust/`, keyed by config path)
— a cloned repo cannot grant itself trust, so it can't run code on `cd` until you explicitly `tau allow` on your
machine. The shell hook delegates activation to `tau hook-env`, which refuses to emit activation code for an untrusted
project; the shell never sources an in-repo file that could be forged.

`tau prune` removes trust records for projects that no longer exist.

### 3.3 Auto-sync on `cd`

The hook is a tiny shim: inside a project it runs one `tau hook-env`, which checks whether any config input changed
since the last sync (or a generated script/tool dir is missing) and, only if needed, runs `tau sync` for you before
re-activating — so editing `workspace.tg`, a loaded module, or a `shell.fn`/`shell.hook` file takes effect on your next
prompt with no manual step, even without leaving the project. Prompts outside any project spawn nothing. Concurrent
syncs are serialized by a per-project lock; a failing sync isn't retried until inputs change (see
[7. Performance](#7-performance)).

### 3.4 Paths, anchoring and imports

Config must not depend on the process cwd, so path arguments (`shell.path.prepend/append`, and the `file=`/`path=` of
`shell.fn`/`shell.hook`/`shell.dotenv`) are anchored:

- `//foo/bar` → `<repo-root>/foo/bar`;
- absolute paths are allowed;
- bare/relative paths (`foo`, `./foo`, `../foo`) are rejected.

`load(...)` is more flexible: besides root-anchored `//taugres/lib/foo.tg` it also accepts **relative** imports
(`./lib/foo.tg`, `../shared/foo.tg`) resolved against the importing file's directory — natural for composing modules.
Remote (`https://…`) imports are **not supported yet** and produce a clear error.

## 4. Configuration reference

The API is side-effect style: calls mutate an in-memory plan. All shell-facing configuration is grouped under the
`shell` namespace; external tool managers keep their own namespaces (`mise`, `pip`, `uv`, `npm`). A representative
`workspace.tg`:

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

### 4.1 Environment

`shell.env(name, value)` sets an environment variable; string values expand `$VAR`/`${VAR}` against earlier
`shell.env` entries and the process environment. `shell.unset(name)` removes one. To store a command's output or a
tool's bin dir, pass a *deferred value* instead of a string (see [4.4 Deferred values](#44-deferred-values)).

`shell.dotenv(path)` reads a `.env` file (root-anchored `//…` or absolute) and sets each pair as if via
`shell.env(...)`. The file must exist (a missing file is an evaluation error) and is tracked as a config input, so
editing it triggers a resync. Format:

- `KEY=VALUE` per line, with an optional `export ` prefix;
- blank lines and `#` comment lines are ignored;
- a single-quoted value is literal; a double-quoted value honors `\n \t \r \\ \"` escapes; an unquoted value is taken
  verbatim after trimming surrounding spaces;
- values are **not** `$VAR`-expanded (a later `shell.env` can expand a var loaded from the file);
- `KEY` must be a valid environment variable name.

```sh
# .env
export TOKEN=abc123
DATABASE_URL=postgres://localhost/app
QUOTED="a b c"
LITERAL='keep $HOME literal'
```

### 4.2 PATH

`shell.path.prepend(entry)` and `shell.path.append(entry)` add to `PATH`; entries are root-anchored (`//…`) or absolute
(see [3.4 Paths, anchoring and imports](#34-paths-anchoring-and-imports)). Expose your own project commands explicitly,
e.g. `shell.path.prepend("//bin")` — there is intentionally no automatic `bin/`, so PATH composition stays predictable.
Tool bin dirs are prepended automatically (see [5.1](#51-how-tools-are-exposed-on-path)).

### 4.3 Aliases, functions and hooks

- `shell.alias(name, value)` — a shell alias.
- `shell.fn(name, shells=[...], content=… | file=…)` — a shell function (exactly one of `content`/`file`); the body may
  be inline or a `//`-anchored file.
- `shell.hook(shells=[...], content=… | file=…)` — a raw activation snippet.

`shell.alias` and `shell.fn` deliberately override an existing definition with the same name while the project is
active; tau saves and restores the previous definition on deactivation. In fish, where aliases are functions,
restoration runs in reverse activation order. `shell.hook` bodies run at activation *after* env/PATH/aliases/functions,
in declaration order, inside the trust gate; they are not undone on deactivation (fire-and-forget, like a function
body).

### 4.4 Deferred values

`shell.exec(command, dynamic=False, shell="")` returns a **deferred handle** for a command's output; assign it and pass
it to `shell.env` to store the result in a variable:

```python
sha = shell.exec("git rev-parse --short HEAD")
shell.env("GIT_SHA", sha)

# or inline
shell.env("NODE_V", shell.exec("node -v"))          # static: sees mise-provisioned node
shell.env("STAMP", shell.exec("date +%s", dynamic = True))
shell.env("OK", shell.exec("[[ -f go.mod ]] && echo yes", shell = "bash"))  # needs bash
```

The command runs in the project root — **never during evaluation**, so inspecting an untrusted config
(`tau check`/`status`) runs no code. When it runs depends on `dynamic`:

- **`dynamic=False`** (default): runs once at `tau sync` (trust-gated, after tool installs so provisioned tools are on
  PATH). The trimmed stdout is baked into the activation script as a normal save/restored variable — activation stays
  instant. The value refreshes on each sync but can go stale between syncs.
- **`dynamic=True`**: emitted as a command substitution (`export VAR="$(cmd)"`, fish `set -gx VAR (cmd)`) that runs in
  your shell on every activation — always fresh, at the cost of a subprocess per activation event.

`shell` picks the interpreter. The default (`""`) is **local**: the user's `$SHELL` (falling back to `sh`) for
static/`tau exec` resolution, and the activating shell for a dynamic entry. A value like `"bash"` runs the command via
`<shell> -c` — use it when the one-liner needs non-POSIX syntax. `tau exec` has no shell, so it resolves both kinds
itself before running the command.

`mise.where(name)` returns a **deferred handle** for the directory where mise installed a tool — the same dir tau
prepends to PATH. Pass it to `shell.env`:

```python
mise.tool("node@22.11.0")
shell.env("NODE_BIN", mise.where("node"))          # -> .../installs/node/22.11.0/bin
shell.env("GO_EXE", mise.where("go") + "/go")      # append a subpath with +
```

The versioned store path isn't known at evaluation, so it's resolved at sync (via `mise where`) and baked into the
activation script. The tool must be a **declared** mise tool (implicit runtimes count); referencing an undeclared one is
a `tau check` error. (For most tools this is the `bin/` dir; for archive-backend tools whose binary name differs from
the tool name it may be the install dir — it always matches what tau puts on PATH.)

**Deferred values compose with `+`.** `shell.exec(...)` and `mise.where(...)` return the same kind of *deferred value*;
`+` joins them with strings and with each other (a literal join, so include separators), and the result is another
deferred value you pass to `shell.env`:

```python
shell.env("PROMPT", "[" + shell.exec("git branch --show-current") + "]")
shell.env("TOOLS", mise.where("go") + ":" + mise.where("node"))
```

A deferred value still can't be **branched on** at eval (`if`, `==`) — it has no value yet; use the host probes
([4.5](#45-host-probes)) for that.

### 4.5 Host probes

Four read-only probes let a config adapt to what's on the machine. Unlike deferred values they return **real values at
evaluation**, so you can branch and compose on them.

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

They only *read* the environment — never run a command or write anything. Because their result depends on the host
filesystem/environment, treat them as an escape hatch: a config that branches on them is only as reproducible as what it
probes.

Probe results are recorded at sync time and re-checked on every in-project prompt, so creating the probed file,
installing the binary, or changing the env var auto-syncs on your next prompt just like a config edit does.

`which` detection is by presence; a binary that *moves* without a presence change is picked up by `tau status`/manual
sync. `env` values are recorded as a hash, so secrets never land in `.taugres/`. Avoid probing a variable tau itself
mutates, like `PATH`, or it will re-sync on every activation.

`read(path, default=...)` returns a file's contents as a string (root-anchored `//…` or absolute). Because it's a plain
value, compose with it directly:

```python
ver = read("//.python-version").strip()   # .strip() drops the trailing newline
mise.tool("python@" + ver)
if "beta" in read("//channel", default = "stable"):
    shell.env("CHANNEL", "beta")
```

A missing file returns `default` if given, else it's an error. The file is tracked as a config input (editing it
re-syncs) and its presence as a probe (so it appearing/disappearing re-syncs). Unlike `shell.exec`, `read` runs no code,
so its result is a normal string you can branch on at eval.

### 4.6 Tools and packages

Declare tools/packages with the tool-manager namespaces; `tau sync` installs them into `.taugres/` and adds them to
`PATH` automatically on activation. Activation itself never calls these managers, keeping `cd` fast. Resolved versions
are pinned in `.taugres.lock` (see [5.2 The lockfile and versions](#52-the-lockfile-and-versions)).

Each call takes a `"name@version"` spec (bare `name` = latest) or a list of them.  `@` is the uniform pin separator
(translated to pip's `==` internally; the last `@` wins, so npm scoped names like `@angular/cli` stay intact):

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

#### mise backends: install almost anything

Beyond its short names, `mise.tool(...)` accepts mise's backends, so most tools need no dedicated tau builtin — prefix
the spec with the backend:

```python
mise.tool([
    "go:github.com/pressly/goose/v3/cmd/goose",  # a Go module's binary
    "cargo:ripgrep",                             # a Rust crate
    "npm:typescript",                            # an npm global
    "pipx:ruff",                                 # an isolated Python CLI
    "ubi:cli/cli",                               # a GitHub release (owner/repo)
    "aqua:pressly/goose",                        # via the aqua registry
])
```

`@version` still applies (`"cargo:ripgrep@14.1.1"`). Backends that hit the GitHub API (`ubi`/`aqua`/github releases)
need `GITHUB_TOKEN`/`MISE_GITHUB_TOKEN` set to avoid unauthenticated rate limits. tau also caps how many tools mise
installs in parallel — default 16, override with `mise.jobs(n)` — which passes `--jobs n` to `mise install` and helps
avoid bursts of unauthenticated API calls.

#### Pinning the pip/npm/uv runtime

**mise is a hard dependency** for tools/packages: `pip`/`uv`/`npm` run on the `python`/`node` that mise provisions
(declaring `pip.install`/`uv.install`/ `npm.install` implies an implicit `mise.tool("python")`/`mise.tool("node")`, and
`uv.install` also an implicit `uv`). Those implicit runtimes default to latest; to **pin the version they run on**, just
declare the runtime yourself and it replaces the implicit one:

```python
pip.install("ruff@0.6.9")
mise.tool("python@3.12.7")   # pip/uv run on this Python
npm.install("typescript")
mise.tool("node@22.11.0")    # npm runs on this Node
```

`tau status` lists the effective runtimes (implicit or pinned).

### 4.7 Reusable helpers

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

### 4.8 Platform

`platform.os` (`"linux"` / `"darwin"`) and `platform.arch` (`"amd64"` / `"arm64"` / …) let a config branch per host:

```python
if platform.os == "linux":
    shell.env("TAUGRES_PLATFORM", "linux")
```

### 4.9 API summary

```python
project(name)

shell.env(name, value)          # value is a string (expands $VAR/${VAR}) or a shell.exec(...) handle
shell.dotenv(path)              # load KEY=VALUE from a .env file (//-anchored/absolute; values literal)
shell.exec(command, dynamic=False, shell="")  # deferred command output; pass to shell.env (shell="" = local $SHELL)
shell.unset(name)
shell.alias(name, value)
shell.path.prepend(entry)       # //-anchored or absolute
shell.path.append(entry)
shell.fn(name, shells=[...], content=... | file=...)   # exactly one of content/file
shell.hook(shells=[...], content=... | file=...)       # raw activation snippet

# Tools/packages: a "name@version" spec (bare name = latest) or a list of them.
# "@" is the uniform pin separator (translated to pip's "==" internally); the
# last "@" wins so npm scoped names stay intact.
mise.tool("go@1.26.2") | mise.tool(["go@1.26.2", "python"])
mise.jobs(n)               # cap mise install parallelism (default 16)
mise.where("node")         # deferred: a declared mise tool's bin dir; pass to shell.env
pip.install("ruff@0.6.9") | pip.install(["ruff@0.6.9", "rich"])   # Python via pip
uv.install("ruff@0.6.9")  | uv.install(["ruff@0.6.9", "rich"])    # Python via uv (faster)
npm.install("typescript") | npm.install(["typescript@5.6.2", "@scope/x@1"])

platform.os                     # "linux" | "darwin"
platform.arch                   # "amd64" | "arm64" | ...

exists("//go.mod")              # bool: root-anchored/absolute path on disk?
which("docker")                 # abs path of a PATH binary, or None
env("CI", "")                   # value of a process env var, or the default when unset
read("//VERSION", default="")   # file contents as a string (default when missing)

load("//taugres/lib/x.tg", "sym")   # root-anchored
load("./lib/x.tg", "sym")           # relative to the importing file
```

## 5. Managing tools and packages

### 5.1 How tools are exposed on PATH

Activation prepends real bin dirs directly, the way `mise activate` works — no symlink/wrapper farm:

- **mise** installs into its user-global store; tau prepends each tool's real store bin dir (e.g.
  `~/.local/share/mise/installs/node/22.11.0/bin`).
- **pip** installs into a project-local venv at `.taugres/tools/pip`; tau prepends `.taugres/tools/pip/bin` (which also
  gives the project its own `python`). Never touches system Python.
- **uv** is the faster modern alternative to pip: `uv.install(...)` creates and installs into a venv at
  `.taugres/tools/uv` (implies an implicit mise `uv` + `python`); tau prepends `.taugres/tools/uv/bin`. (Mixing `pip`
  and `uv` makes two separate venvs — prefer one.)
- **npm** installs into a project-local prefix at `.taugres/tools/npm` via `npm install -g --prefix`; tau prepends
  `.taugres/tools/npm/bin`.

Tool output is shown only with `tau sync --verbose`; otherwise a single progress line is shown. Installs are
best-effort — if one fails, the shell env is still generated and the failure is reported.

Note the distinction from committed scripts: your own `bin/` scripts are exposed explicitly with
`shell.path.prepend("//bin")`, while everything under `.taugres/` is generated, git-ignored, and auto-prepended by tau.

### 5.2 The lockfile and versions

Versions are pinned by default in a committed `.taugres.lock` (in a repo root). On first sync tau records each tool's
resolved concrete version; subsequent syncs install exactly that, so an unpinned `mise.tool("node")` stays put instead
of drifting to "latest". Each entry stores both the requested spec and the resolved version, so editing a version in the
config automatically re-resolves that entry.

- `tau sync` — reproducible: install locked versions. Skips a manager whose declared set is unchanged and whose bin dirs
  are present.
- `tau sync --force [mgr...]` — reinstall even when unchanged, at the **locked** versions (no re-resolve). With no names
  it forces every manager; otherwise just the named ones (`mise`/`pip`/`npm`/`uv`) — e.g. `tau sync --force pip` when
  a venv got corrupted. tau-owned prefixes (pip/uv/npm) are wiped and rebuilt; mise runs `mise install --force`.
- `tau sync --update` — re-resolve **all unpinned** tools/packages to their latest and rewrite the lock. (Pinned entries
  are controlled by editing the config.)
- `tau update <name>...` — re-resolve just the named unpinned entries to their latest, leaving everything else at its
  locked version, and report `old -> new`.  The manager is inferred from where you declared the tool; qualify as
  `<manager>:name` (`pip:ruff`, `uv:ruff`, `mise:node`, `npm:typescript`) to disambiguate a package declared under two
  managers. Updating a pinned entry is refused (edit its version in the config instead); `tau update` with no names is
  equivalent to `tau sync --update`.

### 5.3 Removing tools (GC)

Dropping a `mise.tool`/`pip.install`/`uv.install`/`npm.install` from the config
and running `tau sync` cleans up: its PATH entry is gone (scripts are
regenerated), its lock entry is pruned, and pip/uv/npm packages are uninstalled
from their project-local prefix (a fully-removed manager's dir is deleted). mise
tools live in mise's shared store, so only their lock entry is dropped.

## 6. Reference

### 6.1 Commands

| Command | Description |
| --- | --- |
| `tau init [--nested]` | create `workspace.tg` (or `project.tg`) |
| `tau setup [shell] [--yes]` | install the shell hook into your startup file (default: current shell); offers to install mise if missing (`--yes`: no prompt) |
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

### 6.2 Built-in environment variables

Activation (and `tau exec`) set:

```sh
TAUGRES_ACTIVE=1
TAUGRES_ROOT=/repo                    # repository root; anchor for //
TAUGRES_REPO_ROOT=/repo               # alias for TAUGRES_ROOT
TAUGRES_PROJECT_ROOT=/repo/service-a  # active project root
TAUGRES_CONFIG=/repo/service-a/project.tg
TAUGRES_LOCK=/repo/service-a/.taugres.lock
TAUGRES_STATE=/repo/service-a/.taugres
```

## 7. Performance

Prompts outside any project are pure shell — a config-dir walk, no subprocess (<1ms). Inside a project, each prompt runs
one `tau hook-env`, which does the staleness check in-process and prints nothing when the state is unchanged (~1–3ms;
see `internal/cli/hook_perf_test.go`).

Real work happens only on real events:

- **On a change**, `hook-env` runs a sync (which *does* evaluate Starlark and may run mise/pip/npm). The staleness check
  trips when any recorded **config input** — the active config file, a `load(...)` module, or a `shell.fn`/`shell.hook`
  file — is newer than the last completed sync, or a generated script / tool directory is missing, or an
  `exists()`/`which()` probe flipped.
- **On (re)activation** (entering/switching projects, or after a change), `hook-env` emits the activation script for the
  shell to `eval` — after enforcing trust. This is the security boundary: the shell never sources an in-repo file that
  tau did not vouch for.

So you **always get the latest on the next prompt after an edit**, even without leaving the project. A persistently
failing sync is not retried until the inputs change again (tracked in the session token, once per shell), so there is no
re-sync storm.

## 8. Limitations

Not yet implemented (deferred per plan):

- frozen/lockfile-based dependency installs (npm ci, uv sync --frozen, …) and hashing the ecosystem lockfiles
- remote (`https://`) Starlark imports
