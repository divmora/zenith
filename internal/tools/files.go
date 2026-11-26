package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/divmora/zenith/internal/gitx"
	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/pkg/llm"
)

const (
	maxReadBytes   = 10 << 20 // largest file read_file will open
	defaultLines   = 2000
	maxListEntries = 1000
	maxGrepMatches = 200
	maxGrepFile    = 2 << 20 // grep skips larger files
	maxLineChars   = 2000    // long lines are cut in read/grep output
)

// ReadFile reads a text file with line numbers.
func ReadFile() Tool {
	return Tool{
		Def: llm.Tool{
			Name:        "read_file",
			Description: "Read a text file from the workspace. Output has line numbers (tab-separated) that are not part of the file. Reads up to 2000 lines; use offset and limit for more.",
			InputSchema: schema(map[string]any{
				"path":   str("File path relative to the workspace root."),
				"offset": num("First line to read, 1-based (default 1)."),
				"limit":  num("Number of lines to read (default 2000)."),
			}, "path"),
		},
		ReadOnly: true,
		Run: func(_ context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			abs, rel, err := env.WS.Resolve(in.Path, false)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(abs)
			if err != nil {
				return "", noSuchFile(err, rel)
			}
			if fi.IsDir() {
				return "", fmt.Errorf("%s is a directory; use list_files", rel)
			}
			if fi.Size() > maxReadBytes {
				return "", fmt.Errorf("%s is %d bytes; too large to read (max %d)", rel, fi.Size(), maxReadBytes)
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			if isBinary(data) {
				return "", fmt.Errorf("%s looks like a binary file", rel)
			}
			if len(data) == 0 {
				return "(empty file)", nil
			}
			offset := max(in.Offset, 1)
			limit := in.Limit
			if limit <= 0 {
				limit = defaultLines
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if offset > len(lines) {
				return "", fmt.Errorf("offset %d is past the end of %s (%d lines)", offset, rel, len(lines))
			}
			end := min(offset-1+limit, len(lines))
			var b strings.Builder
			for i := offset - 1; i < end; i++ {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, cutLine(lines[i]))
			}
			if end < len(lines) {
				fmt.Fprintf(&b, "\n(%d more lines; continue with offset=%d)\n", len(lines)-end, end+1)
			}
			return b.String(), nil
		},
	}
}

// WriteFile creates or overwrites a file.
func WriteFile() Tool {
	return Tool{
		Def: llm.Tool{
			Name:        "write_file",
			Description: "Create a file, or overwrite it completely, with the given content. Parent directories are created. Prefer edit_file for changes to existing files.",
			InputSchema: schema(map[string]any{
				"path":    str("File path relative to the workspace root."),
				"content": str("The complete file content."),
			}, "path", "content"),
		},
		Run: func(_ context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			abs, rel, err := env.WS.Resolve(in.Path, true)
			if err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			verb := "Created"
			if fi, err := os.Stat(abs); err == nil {
				if fi.IsDir() {
					return "", fmt.Errorf("%s is a directory", rel)
				}
				mode = fi.Mode().Perm()
				verb = "Overwrote"
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(abs, []byte(in.Content), mode); err != nil {
				return "", err
			}
			return fmt.Sprintf("%s %s (%d bytes).", verb, rel, len(in.Content)), nil
		},
	}
}

// EditFile replaces an exact string in a file.
func EditFile() Tool {
	return Tool{
		Def: llm.Tool{
			Name: "edit_file",
			Description: "Replace an exact string in a file. old_string must match the file exactly (including whitespace and indentation) " +
				"and must occur exactly once unless replace_all is true; include surrounding lines to make it unique. " +
				"Read the file first. Do not include read_file's line-number prefixes.",
			InputSchema: schema(map[string]any{
				"path":        str("File path relative to the workspace root."),
				"old_string":  str("The exact text to replace."),
				"new_string":  str("The replacement text."),
				"replace_all": flag("Replace every occurrence (default false)."),
			}, "path", "old_string", "new_string"),
		},
		Run: func(_ context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Old == "" {
				return "", errors.New("old_string is empty; use write_file to create a file")
			}
			if in.Old == in.New {
				return "", errors.New("old_string and new_string are the same")
			}
			abs, rel, err := env.WS.Resolve(in.Path, true)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(abs)
			if err != nil {
				return "", noSuchFile(err, rel)
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			content := string(data)
			n := strings.Count(content, in.Old)
			switch {
			case n == 0:
				return "", fmt.Errorf("old_string was not found in %s; read the file again and copy the text exactly", rel)
			case n > 1 && !in.ReplaceAll:
				return "", fmt.Errorf("old_string occurs %d times in %s; add surrounding lines to make it unique, or set replace_all", n, rel)
			}
			if in.ReplaceAll {
				content = strings.ReplaceAll(content, in.Old, in.New)
			} else {
				content = strings.Replace(content, in.Old, in.New, 1)
			}
			if err := os.WriteFile(abs, []byte(content), fi.Mode().Perm()); err != nil {
				return "", err
			}
			return fmt.Sprintf("Edited %s (%d replacement%s).", rel, n, plural(n)), nil
		},
	}
}

// ListFiles lists files, honouring .gitignore when the workspace is a git repo.
func ListFiles() Tool {
	return Tool{
		Def: llm.Tool{
			Name: "list_files",
			Description: "List files in the workspace (or under a directory), skipping files ignored by .gitignore. " +
				"Optional glob pattern: \"*.go\" matches by file name anywhere; \"src/**/*.ts\" matches by path. At most 1000 results.",
			InputSchema: schema(map[string]any{
				"path":    str("Directory relative to the workspace root (default: the root)."),
				"pattern": str("Glob pattern to filter by."),
			}),
		},
		ReadOnly: true,
		Run: func(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Path    string `json:"path"`
				Pattern string `json:"pattern"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			files, err := listFiles(ctx, env.WS, in.Path, in.Pattern)
			if err != nil {
				return "", err
			}
			if len(files) == 0 {
				return "(no files match)", nil
			}
			var b strings.Builder
			for i, f := range files {
				if i == maxListEntries {
					fmt.Fprintf(&b, "(%d more files; narrow the path or pattern)\n", len(files)-maxListEntries)
					break
				}
				b.WriteString(f)
				b.WriteByte('\n')
			}
			return b.String(), nil
		},
	}
}

// Grep searches file contents with a regular expression.
func Grep() Tool {
	return Tool{
		Def: llm.Tool{
			Name:        "grep",
			Description: "Search file contents with a regular expression (Go RE2 syntax). Returns path:line:text, at most 200 matches. Skips ignored, binary and very large files.",
			InputSchema: schema(map[string]any{
				"pattern":          str("Regular expression to search for."),
				"path":             str("Directory or file relative to the workspace root (default: the root)."),
				"glob":             str("Only search files matching this glob, e.g. \"*.go\"."),
				"case_insensitive": flag("Ignore case (default false)."),
			}, "pattern"),
		},
		ReadOnly: true,
		Run: func(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Pattern         string `json:"pattern"`
				Path            string `json:"path"`
				Glob            string `json:"glob"`
				CaseInsensitive bool   `json:"case_insensitive"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			expr := in.Pattern
			if in.CaseInsensitive {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %w", err)
			}
			files, err := listFiles(ctx, env.WS, in.Path, in.Glob)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			matches := 0
			for _, rel := range files {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				n, err := grepFile(env.WS, rel, re, &b, maxGrepMatches-matches)
				if err != nil {
					continue
				}
				matches += n
				if matches >= maxGrepMatches {
					fmt.Fprintf(&b, "(stopped at %d matches; narrow the pattern, path or glob)\n", maxGrepMatches)
					break
				}
			}
			if matches == 0 {
				return "(no matches)", nil
			}
			return b.String(), nil
		},
	}
}

func grepFile(ws *policy.Workspace, rel string, re *regexp.Regexp, b *strings.Builder, limit int) (int, error) {
	abs, _, err := ws.Resolve(rel, false)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() || fi.Size() > maxGrepFile {
		return 0, errors.New("skip")
	}
	data, err := os.ReadFile(abs)
	if err != nil || isBinary(data) {
		return 0, errors.New("skip")
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxGrepFile)
	for line := 1; sc.Scan(); line++ {
		if re.Match(sc.Bytes()) {
			fmt.Fprintf(b, "%s:%d:%s\n", rel, line, cutLine(sc.Text()))
			n++
			if n >= limit {
				break
			}
		}
	}
	return n, nil
}

// listFiles returns workspace-relative file paths under dir that match the
// glob, sorted. It uses git to honour .gitignore when it can, and falls back
// to walking the tree (skipping .git and common dependency directories).
func listFiles(ctx context.Context, ws *policy.Workspace, dir, pattern string) ([]string, error) {
	absDir, relDir, err := ws.Resolve(dir, false)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(absDir)
	if err != nil {
		return nil, noSuchFile(err, relDir)
	}
	var match func(string) bool
	if pattern != "" {
		if match, err = compileGlob(pattern); err != nil {
			return nil, err
		}
	}
	if !fi.IsDir() {
		return []string{filepath.ToSlash(relDir)}, nil
	}
	all, err := gitFiles(ctx, ws.Root, relDir)
	if err != nil {
		all, err = walkFiles(ws.Root, absDir)
		if err != nil {
			return nil, err
		}
	}
	out := all[:0]
	for _, f := range all {
		if match == nil || match(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

func gitFiles(ctx context.Context, root, relDir string) ([]string, error) {
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate"}
	if relDir != "." {
		args = append(args, "--", relDir)
	}
	out, err := gitx.Git{Dir: root}.RunRaw(ctx, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		// ls-files lists tracked files that were deleted in the worktree.
		if _, err := os.Lstat(filepath.Join(root, f)); err != nil {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".idea": true, ".vscode": true}

func walkFiles(root, absDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(absDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != absDir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		if len(files) > 50_000 {
			return errors.New("too many files")
		}
		return nil
	})
	return files, err
}

// compileGlob supports *, ?, ** and {a,b}. A pattern without a slash
// matches the file name in any directory.
func compileGlob(pattern string) (func(string) bool, error) {
	pattern = filepath.ToSlash(pattern)
	var b strings.Builder
	b.WriteString("^")
	inAlt := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			inAlt = true
			b.WriteString("(?:")
		case '}':
			if inAlt {
				inAlt = false
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if inAlt {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	byName := !strings.Contains(pattern, "/")
	return func(p string) bool {
		if byName {
			return re.MatchString(filepath.Base(p))
		}
		return re.MatchString(p)
	}, nil
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

func cutLine(s string) string {
	if len(s) <= maxLineChars {
		return s
	}
	return s[:maxLineChars] + " [line cut]"
}

func noSuchFile(err error, rel string) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist", rel)
	}
	return err
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
