package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps a non-streaming response body.
const maxResponseBytes = 32 << 20

// maxSSELine caps one server-sent-events line.
const maxSSELine = 16 << 20

// newHTTPClient returns a client that refuses cross-host redirects. The
// per-call deadline comes from the context (see withTimeout), so streams
// are bounded as a whole.
func newHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: sameHostRedirects}
}

// withTimeout bounds one call by the model config's timeout.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// sameHostRedirects refuses a redirect to another host, or from https to
// http. API keys go in custom headers (x-api-key, x-goog-api-key), which
// Go carries along on redirects, so a redirect elsewhere could collect
// the key.
func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	from := via[0].URL
	switch {
	case !strings.EqualFold(req.URL.Host, from.Host):
		return fmt.Errorf("refusing a redirect from %s to another host (%s)", from.Host, req.URL.Host)
	case from.Scheme == "https" && req.URL.Scheme != "https":
		return fmt.Errorf("refusing a redirect from https to %s on %s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// post sends a JSON body and returns the response for 2xx statuses; other
// statuses become *APIError. The caller closes the body.
func post(ctx context.Context, hc *http.Client, provider, url string, headers map[string]string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", provider, err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ae := &APIError{Provider: provider, StatusCode: resp.StatusCode, Body: string(data)}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, ae
	}
	return resp, nil
}

// postJSON sends a JSON body and decodes a JSON response.
func postJSON(ctx context.Context, hc *http.Client, provider, url string, headers map[string]string, body, out any) error {
	resp, err := post(ctx, hc, provider, url, headers, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", provider, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", provider, err)
	}
	return nil
}

// readSSE reads a server-sent-events stream and calls fn for every event
// with its event name ("" when the server sends none) and data. Returning
// errStopSSE from fn ends the stream without an error.
func readSSE(r io.Reader, fn func(event string, data []byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data bytes.Buffer
	dispatch := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		d := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		err := fn(event, d)
		event = ""
		data.Reset()
		return err
	}
	for {
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if derr := dispatch(); derr != nil && !errors.Is(derr, errStopSSE) {
					return derr
				}
				return nil
			}
			return err
		}
		if len(line) == 0 {
			if derr := dispatch(); derr != nil {
				if errors.Is(derr, errStopSSE) {
					return nil
				}
				return derr
			}
			continue
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			data.Write(value)
			data.WriteByte('\n')
		}
	}
}

var errStopSSE = errors.New("stop reading stream")

// readLine reads one line without its line ending, up to maxSSELine bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				return line, nil
			}
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxSSELine {
			return nil, fmt.Errorf("stream line longer than %d bytes", maxSSELine)
		}
		if !isPrefix {
			return line, nil
		}
	}
}

func mergeHeaders(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func firstTemp(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func maxTokens(req *Request, cfg ModelConfig) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return cfg.MaxTokens
}

// emit calls h when it is set.
func emit(h EventHandler, e Event) {
	if h != nil {
		h(e)
	}
}
