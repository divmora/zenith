package publish

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Platforms zenith can open a pull request on.
const (
	GitHub = "github"
	GitLab = "gitlab"
)

// Remote is a repository on a code host.
type Remote struct {
	Platform string // GitHub or GitLab
	Host     string // e.g. github.com, gitlab.example.com:8443
	Path     string // owner/repo, or group/subgroup/project on GitLab
	APIURL   string // REST base, e.g. https://api.github.com
	PushURL  string // https://host/path.git; zenith always pushes over HTTPS with a token
}

// ParseRemote reads a git remote URL: https://host/owner/repo(.git),
// ssh://git@host[:port]/owner/repo.git, or git@host:owner/repo.git.
// platform ("github", "gitlab" or "") and apiURL ("" for the default)
// override what the host suggests.
func ParseRemote(raw, platform, apiURL string) (*Remote, error) {
	host, path, err := splitRemote(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if strings.Count(path, "/") < 1 || strings.Contains(path, "..") {
		return nil, fmt.Errorf("remote %q: expected owner/repo in the path", raw)
	}
	r := &Remote{Host: host, Path: path, Platform: strings.ToLower(platform)}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	if r.Platform == "" {
		switch {
		case hostname == "github.com":
			r.Platform = GitHub
		case strings.Contains(hostname, "gitlab"):
			r.Platform = GitLab
		default:
			return nil, fmt.Errorf("cannot tell whether %s is GitHub or GitLab; pass --platform", hostname)
		}
	}
	if r.Platform == GitHub && strings.Count(path, "/") != 1 {
		return nil, fmt.Errorf("remote %q: a GitHub path is owner/repo", raw)
	}
	switch r.Platform {
	case GitHub:
		r.APIURL = "https://" + host + "/api/v3"
		if hostname == "github.com" {
			r.APIURL = "https://api.github.com"
		}
	case GitLab:
		r.APIURL = "https://" + host + "/api/v4"
	default:
		return nil, fmt.Errorf("unknown platform %q (use github or gitlab)", platform)
	}
	if apiURL != "" {
		if err := checkAPIURL(apiURL); err != nil {
			return nil, err
		}
		r.APIURL = strings.TrimRight(apiURL, "/")
	}
	r.PushURL = "https://" + host + "/" + path + ".git"
	return r, nil
}

func splitRemote(raw string) (host, path string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("no remote URL")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("remote %q: %w", raw, err)
		}
		switch u.Scheme {
		case "https", "http":
		case "ssh", "git+ssh":
			// The SSH port says nothing about the HTTPS one.
			return u.Hostname(), u.Path, nil
		default:
			return "", "", fmt.Errorf("remote %q: unsupported scheme %s", raw, u.Scheme)
		}
		if u.Host == "" {
			return "", "", fmt.Errorf("remote %q has no host", raw)
		}
		return u.Host, u.Path, nil
	}
	// scp-like: [user@]host:path
	at := strings.LastIndex(raw, "@")
	rest := raw[at+1:]
	host, path, ok := strings.Cut(rest, ":")
	if !ok || host == "" || strings.Contains(host, "/") {
		return "", "", fmt.Errorf("remote %q is not a URL zenith understands", raw)
	}
	return host, path, nil
}

// checkAPIURL accepts https, or http to a loopback host (tests, proxies).
func checkAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid API URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("the API URL must use https: %q", raw)
}
