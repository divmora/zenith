package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divmora/zenith/pkg/protocol"
)

func TestSystem(t *testing.T) {
	root := t.TempDir()
	if s := System(root, nil); strings.Contains(s, "Your role") || strings.Contains(s, "repository_guidance") {
		t.Errorf("plain prompt has extra sections")
	}
	_ = os.MkdirAll(filepath.Join(root, ".zenith"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Run make test."), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".zenith", "sentinel.md"), []byte("2026-11-01: auth lives in mw/auth.go"), 0o644)
	p := &protocol.Persona{Name: "sentinel", Emoji: "🛡️", Title: "Security", Instructions: "Find one security issue."}
	s := System(root, p)
	for _, want := range []string{
		"# Your role: 🛡️ Security (sentinel)\nFind one security issue.",
		".zenith/sentinel.md",
		"Run make test.",
		"auth lives in mw/auth.go",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if strings.Index(s, "Your role") > strings.Index(s, "Repository guidance") {
		t.Errorf("role should come before repository guidance")
	}
	if strings.Contains(System(root, &protocol.Persona{Name: "bolt", Instructions: "x"}), "auth lives") {
		t.Errorf("another persona's journal leaked in")
	}
}
