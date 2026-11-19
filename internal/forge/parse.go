// SPDX-License-Identifier: MIT

package forge

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// HostConfig tells the parser which self-hosted hostnames belong to which
// forge. github.com and gitlab.com are always known.
type HostConfig struct {
	GitHubHosts []string
	GitLabHosts []string
}

// HostConfigFromEnv reads AIREVIEWER_GITHUB_HOSTS and AIREVIEWER_GITLAB_HOSTS
// (comma-separated hostnames).
func HostConfigFromEnv() HostConfig {
	split := func(v string) []string {
		var out []string
		for _, h := range strings.Split(v, ",") {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				out = append(out, h)
			}
		}
		return out
	}
	return HostConfig{
		GitHubHosts: split(os.Getenv("AIREVIEWER_GITHUB_HOSTS")),
		GitLabHosts: split(os.Getenv("AIREVIEWER_GITLAB_HOSTS")),
	}
}

// KindForHost returns the forge for a hostname. Unknown hosts containing
// "gitlab" or "github" are guessed; anything else is an error so the user can
// configure it explicitly.
func (c HostConfig) KindForHost(host string) (Kind, error) {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "github.com":
		return GitHub, nil
	case "gitlab.com":
		return GitLab, nil
	}
	for _, g := range c.GitHubHosts {
		if g == h {
			return GitHub, nil
		}
	}
	for _, g := range c.GitLabHosts {
		if g == h {
			return GitLab, nil
		}
	}
	switch {
	case strings.Contains(h, "gitlab"):
		return GitLab, nil
	case strings.Contains(h, "github"):
		return GitHub, nil
	}
	return "", fmt.Errorf("unknown forge host %q: set AIREVIEWER_GITHUB_HOSTS or AIREVIEWER_GITLAB_HOSTS", host)
}

var (
	ghPRPath = regexp.MustCompile(`^/([^/]+)/([^/]+)/pulls?/(\d+)(?:/.*)?$`)
	glMRPath = regexp.MustCompile(`^/(.+?)/-/merge_requests/(\d+)(?:/.*)?$`)
)

// ParsePRURL turns a PR/MR web URL into a PRRef. The URL shape decides the
// forge when it is unambiguous (GitLab's "/-/merge_requests/" segment);
// otherwise the host is consulted.
func (c HostConfig) ParsePRURL(raw string) (PRRef, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return PRRef{}, fmt.Errorf("not a URL: %q", raw)
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimSuffix(u.Path, "/")

	if m := glMRPath.FindStringSubmatch(path); m != nil {
		n, _ := strconv.Atoi(m[2])
		return PRRef{Repo: Repo{Forge: GitLab, Host: host, Project: m[1]}, Number: n}, nil
	}
	if m := ghPRPath.FindStringSubmatch(path); m != nil {
		kind, err := c.KindForHost(host)
		if err != nil {
			kind = GitHub // the /owner/repo/pull/N shape is GitHub's
		}
		if kind != GitHub {
			return PRRef{}, fmt.Errorf("URL %q looks like a GitHub PR but host is configured as %s", raw, kind)
		}
		n, _ := strconv.Atoi(m[3])
		return PRRef{Repo: Repo{Forge: GitHub, Host: host, Project: m[1] + "/" + m[2]}, Number: n}, nil
	}
	return PRRef{}, fmt.Errorf("unrecognised PR/MR URL %q (expected https://<host>/<owner>/<repo>/pull/<n> or https://<host>/<group>/<project>/-/merge_requests/<n>)", raw)
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// ParseRemote turns a git remote URL (https, ssh://, or scp-like
// git@host:path) into a Repo.
func (c HostConfig) ParseRemote(remote string) (Repo, error) {
	remote = strings.TrimSpace(remote)
	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return Repo{}, fmt.Errorf("parse remote %q: %w", remote, err)
		}
		host, path = u.Hostname(), u.Path
	} else if m := scpLike.FindStringSubmatch(remote); m != nil {
		host, path = m[1], m[2]
	} else {
		return Repo{}, fmt.Errorf("unrecognised git remote %q", remote)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" || !strings.Contains(path, "/") {
		return Repo{}, fmt.Errorf("remote %q has no owner/project path", remote)
	}
	kind, err := c.KindForHost(host)
	if err != nil {
		return Repo{}, err
	}
	return Repo{Forge: kind, Host: strings.ToLower(host), Project: path}, nil
}
