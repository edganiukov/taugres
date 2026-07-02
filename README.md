# tau (Taugres)

A fast tool for managing reproducible, deterministic development environments.
Starlark is used as a configuration language; `tau` generates shell activation
scripts that a lightweight hook sources on `cd`. It also installs tools and
packages (via mise, pip, uv, and npm), pinned in a committed lockfile.

## Install

```sh
go install github.com/edganiukov/taugres/cmd/tau@latest
```

**Requires [mise](https://mise.jdx.dev)** for tools/packages: install it with
`curl https://mise.run | sh`. Only the `mise` binary on `PATH` is needed — tau
reads mise's store directly, so mise's own shell activation is not required and
won't conflict with tau's hook. (A config with no tools/packages works without
mise.)

## Quick start

Install the shell hook once:

```sh
eval "$(tau hook zsh)"      # ~/.zshrc  (or: bash, or `tau hook fish | source`)
```

Then, in a project:

```sh
tau init            # writes workspace.tg
$EDITOR workspace.tg
tau allow           # trust this project once
tau sync            # install tools + generate scripts under .taugres/gen/
```

Now `cd` into the directory (or open a new shell): the hook activates the
environment and restores it when you leave. Example config:

```python
project("my-app")

shell.env("DATABASE_URL", "postgres://localhost/app")
shell.alias("ll", "ls -lah")

mise.tool(["node@22.11.0", "ripgrep"])
uv.install(["ruff", "rich"])
```

## Docs

- **[Getting started](docs/getting-started.md)** — the shell hook & auto-sync,
  trust, the full config API, tools/packages (mise/pip/uv/npm + mise backends),
  the lockfile, commands, performance, and root-anchored paths.
- **[Reference](docs/reference.md)** — config files, the Starlark API, path
  rules, built-in variables, and commands.
- **[Design](docs/design.md)** — architecture and rationale.
