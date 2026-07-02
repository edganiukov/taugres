// Package cli implements command routing and the tau subcommands using only
// the Go standard library flag package.
package cli

import (
	"fmt"
	"io"
	"os"
)

// Version is the tau version string.
const Version = "0.0.1-alpha"

// Env carries process context so commands are testable.
type Env struct {
	Args   []string // args after the program name
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Wd     string // working directory
}

// DefaultEnv builds an Env from the real process.
func DefaultEnv() *Env {
	wd, _ := os.Getwd()
	return &Env{
		Args:   os.Args[1:],
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Wd:     wd,
	}
}

// Main is the entry point; it returns a process exit code.
func Main(e *Env) int {
	if len(e.Args) == 0 {
		printUsage(e.Stderr)
		return 2
	}

	cmd := e.Args[0]
	rest := e.Args[1:]
	return dispatch(e, cmd, rest)
}

func dispatch(e *Env, cmd string, rest []string) int {
	switch cmd {
	case "init":
		return runInit(e, rest)
	case "check":
		return runCheck(e, rest)
	case "sync":
		return runSync(e, rest)
	case "update":
		return runUpdate(e, rest)
	case "exec":
		return runExec(e, rest)
	case "status":
		return runStatus(e, rest)
	case "hook":
		return runHook(e, rest)
	case "hook-env":
		return runHookEnv(e, rest)
	case "activate":
		return runActivate(e, rest)
	case "deactivate":
		return runDeactivate(e, rest)
	case "allow":
		return runAllow(e, rest)
	case "deny":
		return runDeny(e, rest)
	case "clean":
		return runClean(e, rest)
	case "prune":
		return runPrune(e, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(e.Stdout, Version)
		return 0
	case "help", "--help", "-h":
		printUsage(e.Stdout)
		return 0
	default:
		fmt.Fprintf(e.Stderr, "tau: unknown command %q\n\n", cmd)
		printUsage(e.Stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `tau — a fast tool for reproducible, deterministic development environments

Usage:
  tau init [--nested]        create workspace.tg (or project.tg)
  tau check                  evaluate and validate the active config
  tau sync [--update]        install tools and generate shell scripts
  tau update [name...]       re-resolve unpinned tools/packages to latest (all, or just those named)
  tau exec [--] <cmd>...     run a command with the project's env/PATH applied (no shell hook)
  tau status                 show active project, sync state, and tools
  tau allow                  trust the active project (once)
  tau deny                   revoke trust for the active project
  tau clean [--lock]         remove generated state (.taugres/); --lock also drops .taugres.lock
  tau prune                  remove trust records for projects that no longer exist
  tau hook <shell>           print the shell hook (bash|zsh|fish)
  tau hook-env <shell>       used by the hook: print env/activation commands for this prompt
  tau activate [shell]       print the activation script for a trusted project (default: current shell)
  tau deactivate [shell]     print the deactivation script for the active project (default: current shell)
  tau version                print version

Flags:
  tau sync --update          re-resolve unpinned tools to latest, update .taugres.lock
  tau sync --verbose         print every step and tool output

Install the hook:
  eval "$(tau hook zsh)"      # ~/.zshrc
  eval "$(tau hook bash)"     # ~/.bashrc
  tau hook fish | source      # ~/.config/fish/config.fish
`)
}

// fail prints an error to stderr and returns exit code 1.
func fail(e *Env, format string, args ...any) int {
	fmt.Fprintf(e.Stderr, "tau: "+format+"\n", args...)
	return 1
}
