package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/divmora/zenith/internal/prompt"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/protocol"
)

const (
	// keepRecent is how many of the latest messages compaction never touches.
	keepRecent = 8
	// elideOver is the size above which an old tool result is cut.
	elideOver  = 600
	elidedNote = "\n[... rest of this output was removed to save context; run the tool again if you need it ...]"
	// charsPerToken is a conservative estimate (real tokenizers average
	// 3.5 to 4 characters per token for code and English).
	charsPerToken = 3
)

// estimate guesses the prompt size of the next call: the provider's own
// count for the last call plus what was added since.
func (r *runner) estimate(req *llm.Request) int {
	if r.lastInput > 0 && r.lastMsgCount <= len(req.Messages) {
		return r.lastInput + chars(req.Messages[r.lastMsgCount:])/charsPerToken
	}
	n := len(req.System) + chars(req.Messages)
	for _, t := range req.Tools {
		b, _ := json.Marshal(t)
		n += len(b)
	}
	return n / charsPerToken
}

func chars(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text)
		for _, r := range m.Reasoning {
			n += len(r.Text) + len(r.Signature) + len(r.Redacted)
		}
		for _, c := range m.ToolCalls {
			n += len(c.Name) + len(c.Input)
		}
		for _, t := range m.ToolResults {
			n += len(t.Content)
		}
	}
	return n
}

// maybeCompact shortens the conversation when the next call would use more
// than 75% of the context window. First it cuts old tool outputs (cheap, no
// model call); if that is not enough, it replaces the middle of the
// conversation with a model-written summary. The first message (the task)
// and the latest messages are always kept, and roles keep alternating.
func (r *runner) maybeCompact(ctx context.Context, req *llm.Request) {
	limit := r.cfg.ContextTokens * 3 / 4
	if r.estimate(req) < limit {
		return
	}
	if n := elideOldOutputs(req.Messages, len(req.Messages)-keepRecent); n > 0 {
		r.lastInput = 0 // the provider's count no longer applies
		r.compacted(fmt.Sprintf("removed the bulk of %d old tool outputs", n))
		if r.estimate(req) < limit {
			return
		}
	}
	if err := r.summarize(ctx, req); err != nil {
		r.cfg.Logger.Warn("compaction summary failed; continuing with the long history", "error", err)
		r.emit(protocol.Event{Type: protocol.EventWarning, Text: "could not summarize the conversation: " + err.Error()})
	}
}

func (r *runner) compacted(how string) {
	r.result.Compactions++
	r.emit(protocol.Event{Type: protocol.EventCompacted, Text: how})
}

// elideOldOutputs cuts long tool results in messages before index end.
func elideOldOutputs(msgs []llm.Message, end int) int {
	n := 0
	for i := 0; i < end && i < len(msgs); i++ {
		for j := range msgs[i].ToolResults {
			tr := &msgs[i].ToolResults[j]
			if len(tr.Content) > elideOver && !strings.HasSuffix(tr.Content, elidedNote) {
				cut := 300
				for cut > 0 && tr.Content[cut]&0xC0 == 0x80 {
					cut--
				}
				tr.Content = tr.Content[:cut] + elidedNote
				n++
			}
		}
	}
	return n
}

// summarize replaces messages[1:k] with a summary folded into the first
// message, where k is the first assistant message among the latest ones, so
// the kept tail starts with the assistant and every tool result still
// follows its call.
func (r *runner) summarize(ctx context.Context, req *llm.Request) error {
	msgs := req.Messages
	k := -1
	for i := max(1, len(msgs)-keepRecent); i < len(msgs); i++ {
		if msgs[i].Role == llm.Assistant {
			k = i
			break
		}
	}
	if k <= 1 {
		return fmt.Errorf("nothing old enough to summarize")
	}
	resp, err := r.cfg.Model.Chat(ctx, &llm.Request{
		System:    prompt.SummarySystem,
		Messages:  []llm.Message{{Role: llm.User, Text: prompt.SummaryRequest(transcript(msgs[1:k]))}},
		MaxTokens: 4000,
	}, nil)
	if err != nil {
		return err
	}
	r.addUsage(resp.Usage)
	summary := cleanText(resp.Message.Text)
	if summary == "" {
		return fmt.Errorf("the model returned an empty summary")
	}
	head := llm.Message{Role: llm.User, Text: msgs[0].Text}
	if r.result.Plan != "" && r.phase == protocol.PhaseExecute {
		head.Text += "\n\n<approved_plan>\n" + r.result.Plan + "\n</approved_plan>"
	}
	head.Text += "\n\n<progress_so_far>\nEarlier steps of this run were summarised to save context:\n" + summary + "\n</progress_so_far>"
	req.Messages = append([]llm.Message{head}, msgs[k:]...)
	r.lastInput = 0
	r.compacted(fmt.Sprintf("summarized %d earlier messages", k-1))
	return nil
}

// transcript renders messages as plain text for the summary call.
func transcript(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Text != "" {
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, clip(m.Text, 4000))
		}
		for _, c := range m.ToolCalls {
			fmt.Fprintf(&b, "[tool call] %s %s\n", c.Name, clip(string(c.Input), 1500))
		}
		for _, t := range m.ToolResults {
			status := "ok"
			if t.IsError {
				status = "error"
			}
			fmt.Fprintf(&b, "[tool result: %s, %s] %s\n", t.Name, status, clip(t.Content, 1500))
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + " [...]"
}
