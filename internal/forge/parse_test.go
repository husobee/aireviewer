// SPDX-License-Identifier: MIT

package forge

import "testing"

func TestParsePRURL(t *testing.T) {
	c := HostConfig{GitLabHosts: []string{"code.example.com"}, GitHubHosts: []string{"ghe.example.com"}}
	tests := []struct {
		in      string
		want    PRRef
		wantErr bool
	}{
		{in: "https://github.com/octo/repo/pull/12", want: PRRef{Repo{GitHub, "github.com", "octo/repo"}, 12}},
		{in: "https://github.com/octo/repo/pull/12/files?diff=split", want: PRRef{Repo{GitHub, "github.com", "octo/repo"}, 12}},
		{in: "https://ghe.example.com/team/svc/pull/3/", want: PRRef{Repo{GitHub, "ghe.example.com", "team/svc"}, 3}},
		{in: "https://gitlab.com/grp/sub/proj/-/merge_requests/7", want: PRRef{Repo{GitLab, "gitlab.com", "grp/sub/proj"}, 7}},
		{in: "https://code.example.com/a/b/-/merge_requests/99/diffs", want: PRRef{Repo{GitLab, "code.example.com", "a/b"}, 99}},
		{in: "https://code.example.com/a/b/pull/1", wantErr: true}, // GitHub shape on a GitLab host
		{in: "https://github.com/octo/repo/issues/12", wantErr: true},
		{in: "not a url", wantErr: true},
		// Credentials are sent to the parsed host: undeclared hosts never parse.
		{in: "https://evil.example/a/b/-/merge_requests/1", wantErr: true},
		{in: "https://evil.example/o/r/pull/1", wantErr: true},
		{in: "https://gitlab.evil.example/a/b/-/merge_requests/1", wantErr: true},
		{in: "https://github.evil.example/o/r/pull/1", wantErr: true},
		{in: "https://github.com.evil.example/o/r/pull/1", wantErr: true},
		{in: "https://github.com@evil.example/o/r/pull/1", wantErr: true},
		{in: "https://github.com/a/b/-/merge_requests/1", wantErr: true}, // GitLab shape on a GitHub host
		{in: "https://ghe.example.com/a/b/-/merge_requests/1", wantErr: true},
		{in: "ftp://github.com/o/r/pull/1", wantErr: true},
		{in: "https://GitHub.com:443/o/r/pull/2", want: PRRef{Repo{GitHub, "github.com:443", "o/r"}, 2}},
	}
	for _, tt := range tests {
		got, err := c.ParsePRURL(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePRURL(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParsePRURL(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseRemote(t *testing.T) {
	c := HostConfig{GitLabHosts: []string{"code.example.com"}}
	tests := []struct {
		in      string
		want    Repo
		wantErr bool
	}{
		{in: "git@github.com:octo/repo.git", want: Repo{GitHub, "github.com", "octo/repo"}},
		{in: "https://github.com/octo/repo", want: Repo{GitHub, "github.com", "octo/repo"}},
		{in: "ssh://git@code.example.com:2222/grp/sub/proj.git", want: Repo{GitLab, "code.example.com", "grp/sub/proj"}},
		{in: "https://gitlab.com/grp/proj.git/", want: Repo{GitLab, "gitlab.com", "grp/proj"}},
		{in: "git@unknown.example.org:a/b.git", wantErr: true},
		{in: "git@gitlab.evil.example:a/b.git", wantErr: true},
		{in: "https://github.evil.example/a/b.git", wantErr: true},
		{in: "https://github.com/onlyowner", wantErr: true},
	}
	for _, tt := range tests {
		got, err := c.ParseRemote(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRemote(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseRemote(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestTrusted(t *testing.T) {
	c := HostConfig{GitLabHosts: []string{"code.example.com"}, GitHubHosts: []string{"ghe.example.com"}}
	for _, tt := range []struct {
		kind Kind
		host string
		want bool
	}{
		{GitHub, "github.com", true},
		{GitHub, "GitHub.com:443", true},
		{GitLab, "gitlab.com", true},
		{GitLab, "code.example.com:8443", true},
		{GitHub, "ghe.example.com", true},
		{GitHub, "gitlab.com", false},
		{GitLab, "ghe.example.com", false},
		{GitHub, "github.evil.example", false},
		{GitLab, "gitlab.evil.example", false},
		{GitHub, "github.com.", false},
		{GitHub, "", false},
	} {
		if got := c.Trusted(tt.kind, tt.host); got != tt.want {
			t.Errorf("Trusted(%s, %q) = %v, want %v", tt.kind, tt.host, got, tt.want)
		}
	}
}

func TestPRRefString(t *testing.T) {
	if s := (PRRef{Repo{GitHub, "github.com", "o/r"}, 1}).String(); s != "o/r#1" {
		t.Errorf("github ref = %q", s)
	}
	if s := (PRRef{Repo{GitLab, "gitlab.com", "g/p"}, 2}).String(); s != "g/p!2" {
		t.Errorf("gitlab ref = %q", s)
	}
	if GitLab.Abbrev() != "MR" || GitHub.Abbrev() != "PR" {
		t.Error("abbrev mismatch")
	}
}

func TestCountPatch(t *testing.T) {
	full := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n+more\n--- not a header, a deleted line starting with --\n"
	if a, d := CountPatch(full); a != 2 || d != 2 {
		t.Errorf("full patch: +%d -%d, want +2 -2", a, d)
	}
	hunkOnly := "@@ -0,0 +1,2 @@\n+a\n+b\n"
	if a, d := CountPatch(hunkOnly); a != 2 || d != 0 {
		t.Errorf("hunk-only: +%d -%d, want +2 -0", a, d)
	}
}

func TestParseClosingRefs(t *testing.T) {
	body := "This Fixes #12 and closes org/other#3.\nResolves: https://gitlab.com/grp/sub/proj/-/issues/9\n" +
		"Mentions #44 without a keyword. fixes #12 again. Implements grp/proj#7"
	got := ParseClosingRefs(body)
	want := []IssueRef{{"", 12}, {"org/other", 3}, {"grp/sub/proj", 9}, {"grp/proj", 7}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !(IssueRef{"", 1}).SameProject("a/b") || (IssueRef{"x/y", 1}).SameProject("a/b") {
		t.Error("SameProject")
	}
}
