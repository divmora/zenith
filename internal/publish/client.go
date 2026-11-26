package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PR is an open pull request (merge request on GitLab).
type PR struct {
	Number int
	URL    string
	Draft  bool
}

// NewPR is what Create opens.
type NewPR struct {
	Head, Base, Title, Body string
	Draft                   bool
}

// Host is the part of a code host's API that publishing needs.
type Host interface {
	DefaultBranch(ctx context.Context) (string, error)
	BranchExists(ctx context.Context, name string) (bool, error)
	FindOpen(ctx context.Context, head string) (*PR, error)
	Create(ctx context.Context, pr NewPR) (*PR, error)
	// PushHeader is the HTTP header git sends with the push.
	PushHeader() string
	// MaxBody is the longest description the host accepts.
	MaxBody() int
}

// NewHost returns the API client for r.
func NewHost(r *Remote, token string, hc *http.Client) (Host, error) {
	if token == "" {
		return nil, fmt.Errorf("no %s token (set %s)", r.Platform, strings.Join(TokenEnv(r.Platform), " or "))
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	c := *hc
	base, _ := url.Parse(r.APIURL)
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Host != base.Host {
			return http.ErrUseLastResponse
		}
		return nil
	}
	api := &rest{base: r.APIURL, hc: &c, token: token, name: r.Platform}
	switch r.Platform {
	case GitHub:
		return &github{api: api, repo: r.Path}, nil
	case GitLab:
		return &gitlab{api: api, project: url.PathEscape(r.Path)}, nil
	}
	return nil, fmt.Errorf("unknown platform %q", r.Platform)
}

// TokenEnv lists the environment variables a platform's token is read
// from, in order.
func TokenEnv(platform string) []string {
	if platform == GitLab {
		return []string{"ZENITH_GITLAB_TOKEN", "GITLAB_TOKEN"}
	}
	return []string{"ZENITH_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}
}

func basicHeader(user, token string) string {
	return "Authorization: Basic " + b64(user+":"+token)
}

// --- GitHub ---

type github struct {
	api  *rest
	repo string // owner/name
}

func (g *github) path(format string, a ...any) string {
	owner, name, _ := strings.Cut(g.repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + fmt.Sprintf(format, a...)
}

func (g *github) PushHeader() string { return basicHeader("x-access-token", g.api.token) }
func (g *github) MaxBody() int       { return 65000 }

func (g *github) DefaultBranch(ctx context.Context) (string, error) {
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := g.api.do(ctx, "GET", g.path(""), nil, &v)
	return v.DefaultBranch, err
}

func (g *github) BranchExists(ctx context.Context, name string) (bool, error) {
	return exists(g.api.do(ctx, "GET", g.path("/branches/%s", url.PathEscape(name)), nil, nil))
}

func (g *github) FindOpen(ctx context.Context, head string) (*PR, error) {
	owner, _, _ := strings.Cut(g.repo, "/")
	var v []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
	}
	q := url.Values{"head": {owner + ":" + head}, "state": {"open"}}
	if err := g.api.do(ctx, "GET", g.path("/pulls?%s", q.Encode()), nil, &v); err != nil || len(v) == 0 {
		return nil, err
	}
	return &PR{Number: v[0].Number, URL: v[0].HTMLURL, Draft: v[0].Draft}, nil
}

func (g *github) Create(ctx context.Context, pr NewPR) (*PR, error) {
	in := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body, "draft": pr.Draft}
	var v struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
	}
	err := g.api.do(ctx, "POST", g.path("/pulls"), in, &v)
	var he *HTTPError
	if err != nil && pr.Draft && errors.As(err, &he) && he.Status == 422 && strings.Contains(strings.ToLower(he.Body), "draft") {
		// Draft pull requests need a paid plan on private repositories.
		in["draft"] = false
		err = g.api.do(ctx, "POST", g.path("/pulls"), in, &v)
	}
	if err != nil {
		return nil, err
	}
	return &PR{Number: v.Number, URL: v.HTMLURL, Draft: v.Draft}, nil
}

// --- GitLab ---

type gitlab struct {
	api     *rest
	project string // URL-escaped path
}

func (g *gitlab) PushHeader() string { return basicHeader("oauth2", g.api.token) }
func (g *gitlab) MaxBody() int       { return 1_000_000 }

func (g *gitlab) DefaultBranch(ctx context.Context) (string, error) {
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := g.api.do(ctx, "GET", "/projects/"+g.project, nil, &v)
	return v.DefaultBranch, err
}

func (g *gitlab) BranchExists(ctx context.Context, name string) (bool, error) {
	return exists(g.api.do(ctx, "GET", "/projects/"+g.project+"/repository/branches/"+url.PathEscape(name), nil, nil))
}

type gitlabMR struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
	Draft  bool   `json:"draft"`
}

func (m gitlabMR) pr() *PR { return &PR{Number: m.IID, URL: m.WebURL, Draft: m.Draft} }

func (g *gitlab) FindOpen(ctx context.Context, head string) (*PR, error) {
	var v []gitlabMR
	q := url.Values{"source_branch": {head}, "state": {"opened"}}
	if err := g.api.do(ctx, "GET", "/projects/"+g.project+"/merge_requests?"+q.Encode(), nil, &v); err != nil || len(v) == 0 {
		return nil, err
	}
	return v[0].pr(), nil
}

func (g *gitlab) Create(ctx context.Context, pr NewPR) (*PR, error) {
	title := pr.Title
	if pr.Draft {
		title = "Draft: " + title
	}
	in := map[string]any{
		"source_branch": pr.Head, "target_branch": pr.Base, "title": title,
		"description": pr.Body, "remove_source_branch": true,
	}
	var v gitlabMR
	if err := g.api.do(ctx, "POST", "/projects/"+g.project+"/merge_requests", in, &v); err != nil {
		return nil, err
	}
	return v.pr(), nil
}

// --- REST ---

type rest struct {
	base  string
	hc    *http.Client
	token string
	name  string
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Platform, Method, Path string
	Status                 int
	Body                   string
}

func (e *HTTPError) Error() string {
	b := e.Body
	if len(b) > 400 {
		b = b[:400] + "..."
	}
	return fmt.Sprintf("%s %s %s: HTTP %d: %s", e.Platform, e.Method, e.Path, e.Status, b)
}

// exists turns a GET's result into found / not found.
func exists(err error) (bool, error) {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

var retryWait = 2 * time.Second

func (r *rest) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = b
	}
	shown, _, _ := strings.Cut(path, "?")
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, r.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if r.name == GitHub {
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		}
		req.Header.Set("Authorization", "Bearer "+r.token)
		req.Header.Set("User-Agent", "zenith")
		resp, err := r.hc.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s %s: %w", r.name, method, shown, err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		// Only reads are retried: a create that timed out may have worked.
		if method == "GET" && (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < 3 {
			wait := retryWait << attempt
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(min(s, 60)) * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return &HTTPError{r.name, method, shown, resp.StatusCode, strings.TrimSpace(string(data))}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s %s %s: decode: %w", r.name, method, shown, err)
			}
		}
		return nil
	}
}
