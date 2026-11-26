package publish

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/divmora/zenith/internal/gitx"
	"github.com/divmora/zenith/pkg/protocol"
)

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in, platform, api       string
		wantPlatform, wantPath  string
		wantAPI, wantPush, fail string
	}{
		{in: "https://github.com/divmora/zenith.git", wantPlatform: GitHub, wantPath: "divmora/zenith",
			wantAPI: "https://api.github.com", wantPush: "https://github.com/divmora/zenith.git"},
		{in: "git@github.com:divmora/zenith.git", wantPlatform: GitHub, wantPath: "divmora/zenith",
			wantAPI: "https://api.github.com", wantPush: "https://github.com/divmora/zenith.git"},
		{in: "ssh://git@gitlab.com:2222/group/sub/proj.git", wantPlatform: GitLab, wantPath: "group/sub/proj",
			wantAPI: "https://gitlab.com/api/v4", wantPush: "https://gitlab.com/group/sub/proj.git"},
		{in: "https://code.example.com/a/b", platform: "github", wantPlatform: GitHub, wantPath: "a/b",
			wantAPI: "https://code.example.com/api/v3", wantPush: "https://code.example.com/a/b.git"},
		{in: "https://git.example.com/a/b", platform: "gitlab", api: "https://api.example.com/v4/",
			wantPlatform: GitLab, wantPath: "a/b", wantAPI: "https://api.example.com/v4", wantPush: "https://git.example.com/a/b.git"},
		{in: "https://code.example.com/a/b", fail: "pass --platform"},
		{in: "https://github.com/a/b/c", fail: "owner/repo"},
		{in: "https://github.com/onlyone", fail: "owner/repo"},
		{in: "https://github.com/a/../b", fail: "owner/repo"},
		{in: "file:///tmp/repo", fail: "unsupported scheme"},
		{in: "https://github.com/a/b", api: "http://evil.example.com", fail: "must use https"},
		{in: "", fail: "no remote"},
	}
	for _, c := range cases {
		r, err := ParseRemote(c.in, c.platform, c.api)
		if c.fail != "" {
			if err == nil || !strings.Contains(err.Error(), c.fail) {
				t.Errorf("%q: err = %v, want %q", c.in, err, c.fail)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if r.Platform != c.wantPlatform || r.Path != c.wantPath || r.APIURL != c.wantAPI || r.PushURL != c.wantPush {
			t.Errorf("%q: got %+v", c.in, r)
		}
	}
}

// --- helpers ---

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithRun makes a repository whose zenith branch has one commit, and a
// bare repository to push to. It returns the result, the work dir and the bare dir.
func repoWithRun(t *testing.T) (*protocol.Result, string, string) {
	t.Helper()
	dir, bare := t.TempDir(), t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "switch", "-q", "-c", "zenith/fix-it-abc123")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "zenith: fix it", "-m", "Fixed it.")
	sha := git(t, dir, "rev-parse", "HEAD")
	git(t, bare, "init", "-q", "--bare")
	return &protocol.Result{
		V: 1, Status: protocol.StatusDone, Task: "Fix it. cc @someone", Summary: "Fixed it.\n/approve\nThanks @all",
		Model: "fake:model", Branch: "zenith/fix-it-abc123", BaseBranch: "main", BaseSHA: base, CommitSHA: sha,
		Workspace: dir, Files: []string{"a.go"}, Commands: []string{"go test ./...", "echo ```x```"},
		Checks: []protocol.Check{
			{Command: "make test", Stage: protocol.CheckBaseline, Passed: true},
			{Command: "make test", Stage: protocol.CheckFinal, Round: 1, ExitCode: 2},
			{Command: "make test", Stage: protocol.CheckFinal, Round: 2, Passed: true},
		},
		Steps: 5, Usage: protocol.Usage{InputTokens: 100, OutputTokens: 20},
	}, dir, bare
}

type call struct {
	Method, Path, Auth string
	Body               map[string]any
}

type fakeAPI struct {
	mu    sync.Mutex
	calls []call
	route func(c call) (int, string)
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.Body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		status, body := f.route(c)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAPI) posts() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == "POST" {
			out = append(out, c)
		}
	}
	return out
}

func remoteFor(t *testing.T, platform, path, api, bare string) *Remote {
	t.Helper()
	host := map[string]string{GitHub: "github.com", GitLab: "gitlab.com"}[platform]
	r, err := ParseRemote("https://"+host+"/"+path+".git", platform, api)
	if err != nil {
		t.Fatal(err)
	}
	r.PushURL = "file://" + bare
	return r
}

// --- tests ---

func TestPublishGitHub(t *testing.T) {
	res, dir, bare := repoWithRun(t)
	api := &fakeAPI{route: func(c call) (int, string) {
		switch {
		case c.Method == "GET" && c.Path == "/repos/o/r/branches/main":
			return 200, `{"name":"main"}`
		case c.Method == "GET" && strings.HasPrefix(c.Path, "/repos/o/r/pulls?"):
			return 200, `[]`
		case c.Method == "POST" && c.Path == "/repos/o/r/pulls":
			if c.Body["draft"] == true {
				return 422, `{"message":"Draft pull requests are not supported in this repository."}`
			}
			return 201, `{"number":7,"html_url":"https://github.com/o/r/pull/7","draft":false}`
		}
		return 404, `{}`
	}}
	srv := api.server(t)
	var logs []string
	out, err := Publish(context.Background(), Options{
		Result: res, Dir: dir, Remote: remoteFor(t, GitHub, "o/r", srv.URL, bare), Token: "tok",
		Draft: true, PushProtocol: "file", Log: func(s string) { logs = append(logs, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Number != 7 || out.Base != "main" || out.Draft || out.URL != "https://github.com/o/r/pull/7" {
		t.Errorf("outcome = %+v", out)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/zenith/fix-it-abc123"); got != res.CommitSHA {
		t.Errorf("pushed %s, want %s", got, res.CommitSHA)
	}
	posts := api.posts()
	if len(posts) != 2 || posts[0].Body["draft"] != true || posts[1].Body["draft"] != false {
		t.Fatalf("posts = %+v", posts)
	}
	p := posts[1]
	if p.Auth != "Bearer tok" || p.Body["head"] != "zenith/fix-it-abc123" || p.Body["base"] != "main" || p.Body["title"] != "zenith: fix it" {
		t.Errorf("create = %+v", p)
	}
	body := p.Body["body"].(string)
	for _, want := range []string{"Written by an AI agent", "## Summary", "`a.go`", "| `make test` | ✅ passed | ✅ passed |", "Commands the agent ran (2)", "````sh"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@all") { // @someone in the task fence is inert
		t.Errorf("body has a live mention:\n%s", body)
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "/") {
			t.Errorf("body has a quick-action line %q", l)
		}
	}
	if !strings.Contains(strings.Join(logs, "\n"), "does not allow draft") {
		t.Errorf("logs = %q", logs)
	}

	// Publishing again finds the open pull request and creates nothing.
	api.route = func(c call) (int, string) {
		switch {
		case c.Method == "GET" && c.Path == "/repos/o/r/branches/main":
			return 200, `{}`
		case c.Method == "GET" && strings.HasPrefix(c.Path, "/repos/o/r/pulls?"):
			if !strings.Contains(c.Path, "head=o%3Azenith%2Ffix-it-abc123") {
				t.Errorf("find query = %s", c.Path)
			}
			return 200, `[{"number":7,"html_url":"https://github.com/o/r/pull/7"}]`
		}
		return 500, `{}`
	}
	out, err = Publish(context.Background(), Options{
		Result: res, Dir: dir, Remote: remoteFor(t, GitHub, "o/r", srv.URL, bare), Token: "tok", PushProtocol: "file",
	})
	if err != nil || out.Created || out.Number != 7 {
		t.Errorf("second publish = %+v, %v", out, err)
	}
	if len(api.posts()) != 2 {
		t.Errorf("second publish created another pull request")
	}
}

func TestPublishGitLabDefaultBranch(t *testing.T) {
	res, dir, bare := repoWithRun(t)
	res.BaseBranch = "local-only"
	res.Status = protocol.StatusFailed
	res.Error = "verification failed"
	api := &fakeAPI{route: func(c call) (int, string) {
		p := "/projects/g%2Fsub%2Fp"
		switch {
		case c.Method == "GET" && c.Path == p+"/repository/branches/local-only":
			return 404, `{"message":"404 Branch Not Found"}`
		case c.Method == "GET" && c.Path == p:
			return 200, `{"default_branch":"develop"}`
		case c.Method == "GET" && strings.HasPrefix(c.Path, p+"/merge_requests?"):
			return 200, `[]`
		case c.Method == "POST" && c.Path == p+"/merge_requests":
			return 201, `{"iid":3,"web_url":"https://gitlab.com/g/sub/p/-/merge_requests/3","draft":true}`
		}
		return 404, `{}`
	}}
	srv := api.server(t)
	opts := Options{Result: res, Dir: dir, Remote: remoteFor(t, GitLab, "g/sub/p", srv.URL, bare), Token: "glpat", PushProtocol: "file"}
	if _, err := Publish(context.Background(), opts); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("failed run without --allow-incomplete: %v", err)
	}
	opts.AllowIncomplete = true
	out, err := Publish(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Base != "develop" || out.Number != 3 || !out.Draft || out.Platform != GitLab {
		t.Errorf("outcome = %+v", out)
	}
	p := api.posts()[0]
	if p.Body["title"] != "Draft: zenith: fix it" || p.Body["target_branch"] != "develop" || p.Body["source_branch"] != res.Branch {
		t.Errorf("create = %+v", p.Body)
	}
	if d := p.Body["description"].(string); !strings.Contains(d, "The run did not finish") || !strings.Contains(d, "verification failed") {
		t.Errorf("description = %s", d)
	}
}

func TestPublishRefuses(t *testing.T) {
	res, dir, bare := repoWithRun(t)
	api := &fakeAPI{route: func(call) (int, string) { return 200, `{"default_branch":"main"}` }}
	srv := api.server(t)
	remote := remoteFor(t, GitHub, "o/r", srv.URL, bare)
	run := func(mut func(r *protocol.Result), token string) error {
		r := *res
		mut(&r)
		_, err := Publish(context.Background(), Options{Result: &r, Dir: dir, Remote: remote, Token: token, PushProtocol: "file"})
		return err
	}
	cases := map[string]struct {
		mut   func(r *protocol.Result)
		token string
		want  string
	}{
		"awaiting":  {func(r *protocol.Result) { r.Status = protocol.StatusAwaitingApproval }, "t", "awaiting approval"},
		"no change": {func(r *protocol.Result) { r.CommitSHA = "" }, "t", "no changes"},
		"budget":    {func(r *protocol.Result) { r.Status = protocol.StatusBudgetExceeded }, "t", "--allow-incomplete"},
		"no token":  {func(*protocol.Result) {}, "", "GITHUB_TOKEN"},
		"unknown":   {func(r *protocol.Result) { r.CommitSHA = strings.Repeat("a", 40) }, "t", "not in"},
		"same base": {func(r *protocol.Result) { r.BaseBranch = ""; r.Branch = "main" }, "t", "both main"},
	}
	for name, c := range cases {
		if err := run(c.mut, c.token); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if n := len(api.posts()); n != 0 {
		t.Errorf("%d pull requests created", n)
	}
	if out := git(t, bare, "for-each-ref"); out != "" {
		t.Errorf("something was pushed: %s", out)
	}
}

// A push the remote rejects (the branch moved there) fails without
// creating a pull request, and the error does not include the token.
func TestPublishPushRejected(t *testing.T) {
	res, dir, bare := repoWithRun(t)
	other := t.TempDir()
	git(t, other, "init", "-q", "-b", "x")
	git(t, other, "commit", "-q", "--allow-empty", "-m", "unrelated")
	git(t, other, "push", "-q", bare, "x:refs/heads/"+res.Branch)
	api := &fakeAPI{route: func(call) (int, string) { return 200, `{"default_branch":"main"}` }}
	srv := api.server(t)
	_, err := Publish(context.Background(), Options{
		Result: res, Dir: dir, Remote: remoteFor(t, GitHub, "o/r", srv.URL, bare), Token: "s3cret-token", PushProtocol: "file",
	})
	if err == nil || !strings.Contains(err.Error(), "git push") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "s3cret-token") || strings.Contains(err.Error(), b64("x-access-token:s3cret-token")) {
		t.Errorf("error leaks the token: %v", err)
	}
	if len(api.posts()) != 0 {
		t.Error("a pull request was created after a failed push")
	}
}

func TestPushSendsHeaderOnlyToItsHost(t *testing.T) {
	// The header goes through env config scoped to the push URL's host.
	res, dir, _ := repoWithRun(t)
	var got []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(403)
	}))
	defer srv.Close()
	remote := &Remote{Platform: GitHub, Host: "x", Path: "o/r", APIURL: srv.URL, PushURL: srv.URL + "/o/r.git"}
	host, _ := NewHost(remote, "tok", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := gitx.Git{Dir: dir}.Push(ctx, remote.PushURL, res.CommitSHA, res.Branch, gitx.PushOptions{Header: host.PushHeader(), Protocol: "http"})
	if err == nil {
		t.Fatal("push to a 403 server succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != "Basic "+b64("x-access-token:tok") {
		t.Errorf("Authorization headers = %q", got)
	}
}

func TestBodyFencesAndLimits(t *testing.T) {
	res, _, _ := repoWithRun(t)
	res.Task = "do ```` this"
	res.Checks = nil
	res.Files = make([]string, 60)
	for i := range res.Files {
		res.Files[i] = filepath.Join("dir", strings.Repeat("f", i%5+1)+".go")
	}
	body := Body(res, "main", 65000)
	if !strings.Contains(body, "`````text\ndo ```` this\n`````") {
		t.Errorf("task fence not longer than its content:\n%s", body)
	}
	if !strings.Contains(body, "... and 10 more") || !strings.Contains(body, "nothing was verified") {
		t.Errorf("body:\n%s", body)
	}
	res.Summary = strings.Repeat("long ", 5000)
	if b := Body(res, "main", 3000); len(b) > 3000 || !strings.Contains(b, "cut to fit") {
		t.Errorf("limit not applied: %d bytes", len(b))
	}
	if got := tableCode("a | b"); got != "`a \\| b`" {
		t.Errorf("tableCode = %q", got)
	}
}
