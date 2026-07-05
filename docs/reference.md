# Taugres Reference

Lookup reference for the `tau` CLI: config files, the Starlark configuration
API, path/anchoring rules, built-in variables, and commands. For *why* things
work this way, see [design.md](design.md).

## Config files

Config file names are fixed; Taugres does not discover arbitrary `.tg` files as
entrypoints:

- `workspace.tg` — repository-root marker and shared root config;
- `project.tg` — nested/secondary project config;
- `*.tg` elsewhere (e.g. `taugres/lib/`) — helper modules, only reachable via
  `load(...)`.

A directory must not contain both `workspace.tg` and `project.tg`.

## Discovery

1. walk upward from cwd to the first `project.tg`/`workspace.tg` — the active
   config;
2. from there, walk upward to the first `workspace.tg` — the repository root;
3. if none, the active project root is also the repo root.

For nested projects, `//` is the repository root while `TAUGRES_PROJECT_ROOT` is
the active project directory.

## Path anchoring and imports

Config must not depend on the process cwd. Path arguments (`shell.path.*`,
`shell.fn`/`shell.hook`/`shell.dotenv` `file=`/`path=`) are anchored:

- `//foo/bar` → `<repo-root>/foo/bar`;
- absolute paths are allowed;
- bare/relative paths (`foo`, `./foo`, `../foo`) are rejected.

`load(...)` is more flexible: besides root-anchored `//…` it also accepts
**relative** imports (`./x.tg`, `../x.tg`) resolved against the importing file's
directory. Remote (`https://`) imports are **not supported yet** and produce a
clear error.

## Configuration API

The API is side-effect style: calls mutate an in-memory plan. All shell-facing
configuration is grouped under the `shell` namespace; external tool managers keep
their own namespaces.

```python
project("my-app")

# Environment. Values expand $VAR / ${VAR} against earlier shell.env entries
# and the process environment.
shell.env("DATABASE_URL", "postgres://localhost/app")
shell.env("BIN", "$HOME/.local/bin")
shell.unset("PYTHONPATH")

# Load KEY=VALUE pairs from a .env file (values taken literally, no expansion).
shell.dotenv("//.env")

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

# Tools/packages. Single or an array of specs.
mise.tool(["go@1.26.2", "python"])
pip.install(["ruff@0.6.9", "rich"])
npm.install("typescript")

# Platform conditionals.
if platform.os == "linux":
    shell.env("TAUGRES_PLATFORM", "linux")

# Environment conditionals (read-only probe, like exists()/which()).
if env("CI"):
    mise.jobs(4)
```

### API summary

```python
project(name)

shell.env(name, value)          # value expands $VAR/${VAR} (earlier env, then process env)
shell.dotenv(path)              # load KEY=VALUE from a .env file (//-anchored/absolute; values literal)
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
# cap mise install parallelism (default 16)
mise.jobs(n)
pip.install("ruff@0.6.9") | pip.install(["ruff@0.6.9", "rich"])   # Python via pip
uv.install("ruff@0.6.9")  | uv.install(["ruff@0.6.9", "rich"])    # Python via uv (faster)
npm.install("typescript") | npm.install(["typescript@5.6.2", "@scope/x@1"])

platform.os                     # "linux" | "darwin"
platform.arch                   # "amd64" | "arm64" | ...

exists("//go.mod")              # bool: root-anchored/absolute path on disk?
which("docker")                 # abs path of a PATH binary, or None
env("CI", "")                   # value of a process env var, or the default when unset

load("//taugres/lib/x.tg", "sym")   # root-anchored
load("./lib/x.tg", "sym")           # relative to the importing file
```

### Pinning the pip/npm/uv runtime

`pip`/`uv` run on the mise-provided `python`, `npm` on the mise-provided `node`,
and `uv` also needs the `uv` binary — so declaring packages implies an implicit
`mise.tool("python")` / `mise.tool("node")` / `mise.tool("uv")` at **latest**
(pinned in the lock on first sync). To choose the version, declare the runtime
explicitly; any `mise.tool` with that name replaces the implicit one:

```python
pip.install("ruff@0.6.9")
uv.install("rich")
npm.install("typescript")

mise.tool("python@3.12.7")   # runtime for pip + uv
mise.tool("node@22.11.0")    # runtime for npm
mise.tool("uv@0.4.20")       # uv itself
```

`tau status` lists the effective runtimes (implicit or pinned).

`shell.hook` bodies run at activation *after* env/PATH/aliases/functions, in
declaration order, inside the trust gate; they are not undone on deactivation
(fire-and-forget, like a function body).

`exists()`, `which()`, and `env()` are read-only probes: they read the
filesystem/PATH/environment but never run commands or write anything, and their
results are recorded so a later change re-syncs (see
[design.md](design.md#staleness-checks)).

### `.env` files (`shell.dotenv`)

`shell.dotenv(path)` reads a `.env` file (root-anchored `//…` or absolute) and
sets each pair as if via `shell.env(...)`. The file must exist (a missing file is
an evaluation error) and is tracked as a config input, so editing it triggers a
resync. Format:

- `KEY=VALUE` per line, with an optional `export ` prefix;
- blank lines and `#` comment lines are ignored;
- a single-quoted value is literal; a double-quoted value honors `\n \t \r \\ \"`
  escapes; an unquoted value is taken verbatim after trimming surrounding spaces;
- values are **not** `$VAR`-expanded (a later `shell.env` can expand a var loaded
  from the file);
- `KEY` must be a valid environment variable name.

```sh
# .env
export TOKEN=abc123
DATABASE_URL=postgres://localhost/app
QUOTED="a b c"
LITERAL='keep $HOME literal'
```

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

## Built-in environment variables

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

## Commands

```sh
tau init [--nested]        # create workspace.tg (or project.tg)
tau check                  # evaluate + validate config
tau sync [--update]        # install tools/packages and generate scripts (needs trust)
tau sync --verbose         # print every step and tool output
tau update [name...]       # re-resolve unpinned tools/packages to latest (all, or just those named)
tau exec [--] <cmd>...     # run a command with the project env/PATH applied (no shell hook); needs trust
tau status                 # active project, sync state, tools, trust
tau allow                  # trust the active project (once)
tau deny                   # revoke trust
tau clean [--lock]         # remove .taugres/; --lock also drops .taugres.lock
tau prune                  # remove orphaned trust records
tau hook <shell>           # print the shell hook (bash|zsh|fish)
tau hook-env <shell>       # used by the hook: env/activation commands for this prompt
tau activate [shell]       # print the activation script for a trusted project (default: $SHELL)
tau deactivate [shell]     # print the deactivation script for a trusted project (default: $SHELL)
tau version
```

### Installing the shell hook

Install once per shell:

```sh
eval "$(tau hook zsh)"     # ~/.zshrc
eval "$(tau hook bash)"    # ~/.bashrc  (double quotes required)
tau hook fish | source     # ~/.config/fish/config.fish
```
