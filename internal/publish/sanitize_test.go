package publish

import (
	"strings"
	"testing"
)

func TestNeutraliseQuickActions(t *testing.T) {
	in := strings.Join([]string{
		"The token check can be bypassed.",
		"/approve",
		"  /merge",
		"see /etc/hosts in the middle of a line",
		"```suggestion:-0+0",
		"/* kept: code in a closed block */",
		"```",
		"```",
		"/close (an unclosed block is not code to GitLab)",
	}, "\n")
	out := NeutraliseQuickActions(in)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "/") && !strings.HasPrefix(l, "/* kept") {
			t.Errorf("a line still starts with /: %q", l)
		}
	}
	for _, want := range []string{`\/approve`, `  \/merge`, "/* kept: code in a closed block */", `\/close`, "see /etc/hosts"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if NeutraliseQuickActions("no slash here") != "no slash here" {
		t.Error("text without commands is unchanged")
	}
}

func TestNeutraliseMentions(t *testing.T) {
	in := "Ping @all and @alice-b, mail ops@example.com, run `@codereview review`.\n```\n@decorator\n```\n~~~\n@also_code\n~~~\n@bob"
	out := NeutraliseMentions(in)
	for _, want := range []string{"@" + zwsp + "all", "@" + zwsp + "alice-b", "ops@example.com", "`@codereview review`", "\n@decorator\n", "\n@also_code\n", "@" + zwsp + "bob"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}
