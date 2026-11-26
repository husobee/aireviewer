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

// KindForHost returns the forge for a hostname. Only github.com, gitlab.com
// and hosts declared in the config are known; anything else is an error.
//
// This is a security boundary: forge credentials are sent to whatever host
// this accepts, so a host is never inferred from its name or from a URL's
// shape (a PR URL naming "gitlab.attacker.example" must not receive
// GITLAB_TOKEN). A port, if present, is ignored for matching.
func (c HostConfig) KindForHost(host string) (Kind, error) {
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	switch h {
	case "github.com":
		return GitHub, nil
	case "gitlab.com":
		return GitLab, nil
	}
	if h != "" {
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
	}
	return "", fmt.Errorf("unknown forge host %q: only github.com, gitlab.com and hosts listed in AIREVIEWER_GITHUB_HOSTS or AIREVIEWER_GITLAB_HOSTS are used, so credentials are never sent to an undeclared host", host)
}

// Trusted reports whether credentials for kind may be sent to host.
func (c HostConfig) Trusted(kind Kind, host string) bool {
	k, err := c.KindForHost(host)
	return err == nil && k == kind
}

var (
	ghPRPath = regexp.MustCompile(`^/([^/]+)/([^/]+)/pulls?/(\d+)(?:/.*)?$`)
	glMRPath = regexp.MustCompile(`^/(.+?)/-/merge_requests/(\d+)(?:/.*)?$`)
)

// ParsePRURL turns a PR/MR web URL into a PRRef. The host must be known
// (see KindForHost) and the URL shape must match that host's forge.
func (c HostConfig) ParsePRURL(raw string) (PRRef, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return PRRef{}, fmt.Errorf("not a URL: %q", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return PRRef{}, fmt.Errorf("unsupported URL scheme in %q (expected https)", raw)
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimSuffix(u.Path, "/")

	var ref PRRef
	switch m, g := glMRPath.FindStringSubmatch(path), ghPRPath.FindStringSubmatch(path); {
	case m != nil:
		n, _ := strconv.Atoi(m[2])
		ref = PRRef{Repo: Repo{Forge: GitLab, Host: host, Project: m[1]}, Number: n}
	case g != nil:
		n, _ := strconv.Atoi(g[3])
		ref = PRRef{Repo: Repo{Forge: GitHub, Host: host, Project: g[1] + "/" + g[2]}, Number: n}
	default:
		return PRRef{}, fmt.Errorf("unrecognised PR/MR URL %q (expected https://<host>/<owner>/<repo>/pull/<n> or https://<host>/<group>/<project>/-/merge_requests/<n>)", raw)
	}
	kind, err := c.KindForHost(host)
	if err != nil {
		return PRRef{}, err
	}
	if kind != ref.Forge {
		return PRRef{}, fmt.Errorf("URL %q looks like a %s %s but host %s is configured as %s", raw, ref.Forge, ref.Forge.Noun(), host, kind)
	}
	return ref, nil
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
