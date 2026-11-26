package gitx

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// guarded are the files in the git directory that make git run programs or
// change how it treats files. Commands the model runs can rewrite them, and
// the user's own git (with the user's full environment) would then run what
// they say. Linked worktrees share them with the main checkout.
var guarded = []string{"config", "config.worktree", "info/attributes"}

// Snapshot is the state of a repository's guarded git files and hooks.
type Snapshot struct {
	dir   string            // the git common directory
	files map[string][]byte // relative path -> content; nil value = absent
}

// Snapshot records the guarded files and every hook in the repository's
// common git directory.
func (g Git) Snapshot(ctx context.Context) (*Snapshot, error) {
	common, err := g.Run(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(g.Dir, common)
	}
	s := &Snapshot{dir: common, files: map[string][]byte{}}
	paths, err := s.paths()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(common, p))
		switch {
		case err == nil:
			s.files[p] = data
		case errors.Is(err, fs.ErrNotExist):
			s.files[p] = nil
		default:
			return nil, err
		}
	}
	return s, nil
}

// paths lists the guarded files plus whatever is in hooks/ now.
func (s *Snapshot) paths() ([]string, error) {
	seen := map[string]bool{}
	for _, p := range guarded {
		seen[p] = true
	}
	for p := range s.files {
		seen[p] = true
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "hooks"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			seen[filepath.Join("hooks", e.Name())] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// Restore puts every guarded file and hook back as it was at the snapshot:
// changed files are rewritten, new ones removed, deleted ones recreated. It
// returns the paths it restored (relative to the git directory).
func (s *Snapshot) Restore() ([]string, error) {
	paths, err := s.paths()
	if err != nil {
		return nil, err
	}
	var restored []string
	var errs []error
	for _, p := range paths {
		abs := filepath.Join(s.dir, p)
		want, known := s.files[p]
		got, rerr := os.ReadFile(abs)
		exists := rerr == nil
		switch {
		case !known || want == nil:
			if exists || !errors.Is(rerr, fs.ErrNotExist) {
				if err := os.RemoveAll(abs); err != nil {
					errs = append(errs, err)
					continue
				}
				restored = append(restored, p)
			}
		case !exists || !bytes.Equal(got, want):
			mode := os.FileMode(0o644)
			if filepath.Dir(p) == "hooks" {
				mode = 0o755
			}
			_ = os.RemoveAll(abs) // a symlink or directory planted in its place
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.WriteFile(abs, want, mode); err != nil {
				errs = append(errs, err)
				continue
			}
			restored = append(restored, p)
		}
	}
	return restored, errors.Join(errs...)
}
