// Package persona holds Zenith's pre-built personas: specialist roles
// (security, performance, UX, maintainability, tests, docs) that come with
// their own instructions and a default task, so `zenith run --persona
// sentinel` needs no task text.
//
// A persona is a Markdown file with a small front-matter header:
//
//	---
//	name: sentinel
//	emoji: 🛡️
//	title: Security
//	description: One line for listings.
//	task: The default task when --task is not given.
//	---
//	Instructions for the model (Markdown).
//
// Operators can write their own and pass them with --persona-file.
package persona

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/divmora/zenith/pkg/protocol"
)

//go:embed builtin/*.md
var builtinFS embed.FS

// Persona is a specialist role.
type Persona struct {
	Name         string
	Emoji        string
	Title        string
	Description  string
	Task         string // default task
	Instructions string
}

// Info is the part saved in checkpoints, so a resumed run keeps the same
// role even if the persona file is gone.
func (p Persona) Info() *protocol.Persona {
	return &protocol.Persona{Name: p.Name, Emoji: p.Emoji, Title: p.Title, Instructions: p.Instructions}
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Parse reads a persona file's content.
func Parse(data []byte) (Persona, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Persona{}, errors.New("persona file must start with a --- front-matter header")
	}
	header, body, ok := strings.Cut(text[4:], "\n---\n")
	if !ok {
		return Persona{}, errors.New("persona front matter is not closed with ---")
	}
	var p Persona
	for i, line := range strings.Split(header, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Persona{}, fmt.Errorf("front matter line %d: want key: value", i+1)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			p.Name = value
		case "emoji":
			p.Emoji = value
		case "title":
			p.Title = value
		case "description":
			p.Description = value
		case "task":
			p.Task = value
		default:
			return Persona{}, fmt.Errorf("front matter line %d: unknown key %q", i+1, strings.TrimSpace(key))
		}
	}
	p.Instructions = strings.TrimSpace(body)
	switch {
	case !validName.MatchString(p.Name):
		return Persona{}, fmt.Errorf("persona name %q: use lowercase letters, digits and dashes", p.Name)
	case p.Instructions == "":
		return Persona{}, fmt.Errorf("persona %s has no instructions", p.Name)
	}
	if p.Title == "" {
		p.Title = p.Name
	}
	return p, nil
}

// Load reads a persona from a file.
func Load(path string) (Persona, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Persona{}, err
	}
	p, err := Parse(data)
	if err != nil {
		return Persona{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Builtin returns the pre-built personas, sorted by name.
func Builtin() []Persona {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		panic(err)
	}
	out := make([]Persona, 0, len(entries))
	for _, e := range entries {
		data, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			panic(err)
		}
		p, err := Parse(data)
		if err != nil {
			panic(fmt.Sprintf("built-in persona %s: %v", e.Name(), err))
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a built-in persona by name.
func Get(name string) (Persona, error) {
	var names []string
	for _, p := range Builtin() {
		if p.Name == strings.ToLower(strings.TrimSpace(name)) {
			return p, nil
		}
		names = append(names, p.Name)
	}
	return Persona{}, fmt.Errorf("unknown persona %q (built-in: %s; or use --persona-file)", name, strings.Join(names, ", "))
}
