package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotRestore(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	g := Git{Dir: dir}
	ctx := context.Background()
	gitDir := filepath.Join(dir, ".git")
	hooks := filepath.Join(gitDir, "hooks")
	_ = os.RemoveAll(hooks)
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := os.ReadFile(filepath.Join(gitDir, "config"))

	snap, err := g.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// What a prompt-injected command might do.
	for _, args := range [][]string{{"config", "core.fsmonitor", "/tmp/evil"}, {"config", "filter.x.clean", "/tmp/evil"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nevil\n"), 0o755)
	_ = os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nevil\n"), 0o755)
	_ = os.MkdirAll(filepath.Join(gitDir, "info"), 0o755)
	_ = os.WriteFile(filepath.Join(gitDir, "info", "attributes"), []byte("* filter=x\n"), 0o644)

	restored, err := snap.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(restored, ","); got != "config,hooks/post-checkout,hooks/pre-push,info/attributes" {
		t.Errorf("restored = %s", got)
	}
	if cfg, _ := os.ReadFile(filepath.Join(gitDir, "config")); string(cfg) != string(cfgBefore) {
		t.Errorf("config not restored:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(hooks, "post-checkout")); err == nil {
		t.Error("planted hook not removed")
	}
	if h, _ := os.ReadFile(filepath.Join(hooks, "pre-push")); string(h) != "#!/bin/sh\necho mine\n" {
		t.Errorf("user's hook not restored: %q", h)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "info", "attributes")); err == nil {
		t.Error("planted attributes not removed")
	}
	if again, _ := snap.Restore(); len(again) != 0 {
		t.Errorf("second restore changed %v", again)
	}
}
