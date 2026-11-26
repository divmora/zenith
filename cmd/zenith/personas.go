package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/divmora/zenith/internal/persona"
	"github.com/divmora/zenith/pkg/protocol"
)

// cmdPersonas lists the built-in personas, or shows one in full.
func cmdPersonas(args []string, stdout, stderr io.Writer) int {
	switch len(args) {
	case 0:
		_, _ = fmt.Fprintln(stdout, "Built-in personas (zenith run --persona NAME):")
		_, _ = fmt.Fprintln(stdout)
		for _, p := range persona.Builtin() {
			_, _ = fmt.Fprintf(stdout, "  %-9s %s %-20s %s\n", p.Name, p.Emoji, p.Title, p.Description)
		}
		_, _ = fmt.Fprintln(stdout, "\nShow one in full: zenith personas NAME. Write your own and pass it with --persona-file.")
		return protocol.ExitDone
	case 1:
		p, err := persona.Get(args[0])
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "zenith: %v\n", err)
			return protocol.ExitUsage
		}
		_, _ = fmt.Fprintf(stdout, "---\nname: %s\nemoji: %s\ntitle: %s\ndescription: %s\ntask: %s\n---\n%s\n",
			p.Name, p.Emoji, p.Title, p.Description, p.Task, strings.TrimSpace(p.Instructions))
		return protocol.ExitDone
	default:
		_, _ = fmt.Fprintln(stderr, "usage: zenith personas [NAME]")
		return protocol.ExitUsage
	}
}
