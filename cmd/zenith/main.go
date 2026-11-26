// Command zenith is an autonomous coding agent: give it a task and a
// repository, and it plans the change, edits the code, runs the project's
// checks and hands back a branch ready for a pull request.
//
// It runs one task per process, usually inside a sandbox. Zenith Console
// (separate repo) queues tasks, starts sandboxes and hosts the approval UI.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/divmora/zenith/internal/version"
)

const usage = `zenith — autonomous coding agent

Usage:
  zenith run --task "..." [flags]   Do one task in a git repository and commit it on a new branch
  zenith run --persona NAME         Same, as a specialist with its own default task (sentinel, bolt, ...)
  zenith resume --checkpoint FILE   Continue after plan approval (--approve or --feedback "...")
  zenith publish --result FILE      Push the run's branch and open a pull request (GitHub, GitLab)
  zenith personas [NAME]            List the built-in personas, or print one
  zenith version                    Print the version
  zenith help                       Show this help

Pick a model with --model or ZENITH_MODEL:
  anthropic:<model>  openai:<model>  gemini:<model>  bedrock:<model-id>  ollama:<model> ...

Run "zenith <command> -h" for its flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a command and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "resume":
		return cmdResume(args[1:], stdout, stderr)
	case "publish":
		return cmdPublish(args[1:], stdout, stderr)
	case "personas":
		return cmdPersonas(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		_, _ = fmt.Fprintln(stdout, version.Version)
		return 0
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "zenith: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
