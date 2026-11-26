package publish

import "strings"

// GitLab runs quick actions (/approve, /merge, /close, /label, ...) in
// notes and descriptions written through the API, as the token's user.
// Text zenith posts comes partly from the model, which a prompt injection
// in the diff or the MR can steer, so nothing it posts may start a line
// with "/" (code-review-bot issue #19).

// NeutraliseQuickActions escapes every line whose first non-space
// character is "/" by putting a backslash before the slash: Markdown
// renders "\/approve" as "/approve", and GitLab doesn't read it as a
// command. Lines inside the code blocks GitLab itself skips (a line
// starting with ``` up to the next line that is exactly ```) are left as
// they are, so code and suggestions stay intact. A block GitLab wouldn't
// skip (unclosed, or closed by anything but a bare ```) is escaped.
func NeutraliseQuickActions(body string) string {
	if !strings.Contains(body, "/") {
		return body
	}
	lines := strings.Split(body, "\n")
	// Mark the lines inside closed fences, the way GitLab's extractor
	// finds them: from a line starting with ``` to the next bare ```.
	code := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSuffix(lines[j], "\r") == "```" {
				for k := i; k <= j; k++ {
					code[k] = true
				}
				i = j
				break
			}
		}
	}
	changed := false
	for i, l := range lines {
		if code[i] {
			continue
		}
		rest := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(rest, "/") {
			lines[i] = l[:len(l)-len(rest)] + `\` + rest
			changed = true
		}
	}
	if !changed {
		return body
	}
	return strings.Join(lines, "\n")
}

// zwsp is a zero-width space: after "@" it keeps a mention from linking
// or notifying anyone, and shows nothing.
const zwsp = "\u200b"

// NeutraliseMentions keeps @mentions in text zenith posts from notifying
// anyone (@all, a user, another bot): a zero-width space goes after each
// "@" that starts a mention. Code (fenced blocks and inline code spans) is
// left as it is, and so are e-mail addresses.
func NeutraliseMentions(body string) string {
	if !strings.Contains(body, "@") {
		return body
	}
	lines := strings.Split(body, "\n")
	fence := "" // the open fence's marker (``` or ~~~), or ""
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if fence != "" {
			if strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]+" \t\r") == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		lines[i] = mentionsInLine(l)
	}
	return strings.Join(lines, "\n")
}

// mentionsInLine neutralises the mentions in one line outside `code`.
func mentionsInLine(l string) string {
	if !strings.Contains(l, "@") {
		return l
	}
	var b strings.Builder
	inCode := false
	for i := 0; i < len(l); i++ {
		c := l[i]
		if c == '`' {
			inCode = !inCode
		}
		b.WriteByte(c)
		if c != '@' || inCode || i+1 >= len(l) || !mentionChar(l[i+1]) {
			continue
		}
		if i > 0 && (mentionChar(l[i-1]) || l[i-1] == '.') { // user@example.com
			continue
		}
		b.WriteString(zwsp)
	}
	return b.String()
}

func mentionChar(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Sanitize is what every note, comment and summary zenith posts goes
// through: no quick actions (GitLab), no mentions.
func Sanitize(body string) string { return NeutraliseQuickActions(NeutraliseMentions(body)) }
