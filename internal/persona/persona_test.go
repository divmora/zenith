package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	want := []string{"bolt", "palette", "probe", "scribe", "sentinel", "sweeper"}
	got := Builtin()
	if len(got) != len(want) {
		t.Fatalf("got %d personas", len(got))
	}
	for i, p := range got {
		if p.Name != want[i] {
			t.Errorf("persona %d = %s, want %s", i, p.Name, want[i])
		}
		if p.Task == "" || p.Description == "" || p.Emoji == "" || len(p.Instructions) < 500 {
			t.Errorf("%s is incomplete: %+v", p.Name, p)
		}
		// Zenith owns branches, commits and pushes; personas must not ask the
		// model to do them, and every persona says what to do when there is
		// nothing worth changing.
		low := strings.ToLower(p.Instructions)
		for _, bad := range []string{"git commit", "git push", "git checkout -b", "create a pr", "pull request with"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s tells the model to %q", p.Name, bad)
			}
		}
		if p.Name != "probe" && !strings.Contains(low, "change nothing") {
			t.Errorf("%s does not say what to do when nothing needs changing", p.Name)
		}
	}
}

func TestGet(t *testing.T) {
	if p, err := Get(" Sentinel "); err != nil || p.Name != "sentinel" {
		t.Errorf("Get = %+v %v", p, err)
	}
	if _, err := Get("jarvis"); err == nil || !strings.Contains(err.Error(), "bolt, palette") {
		t.Errorf("unknown persona error = %v", err)
	}
}

func TestParse(t *testing.T) {
	good := "---\nname: release-notes\n# a comment\ntitle: Release notes\ntask: Write notes.\n---\nDo the thing.\n"
	p, err := Parse([]byte(good))
	if err != nil || p.Name != "release-notes" || p.Title != "Release notes" || p.Instructions != "Do the thing." || p.Task != "Write notes." {
		t.Fatalf("Parse = %+v %v", p, err)
	}
	if info := p.Info(); info.Name != p.Name || info.Instructions != p.Instructions {
		t.Errorf("Info = %+v", info)
	}
	bad := map[string]string{
		"no header":   "name: x\n",
		"unclosed":    "---\nname: x\n",
		"bad name":    "---\nname: Not Valid\n---\nbody",
		"unknown key": "---\nname: x\ncolour: red\n---\nbody",
		"no body":     "---\nname: x\n---\n   \n",
		"no colon":    "---\nname x\n---\nbody",
	}
	for name, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	path := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(path, []byte("---\r\nname: crlf\r\n---\r\nWindows line endings.\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := Load(path); err != nil || p.Name != "crlf" || p.Title != "crlf" {
		t.Errorf("Load = %+v %v", p, err)
	}
}
