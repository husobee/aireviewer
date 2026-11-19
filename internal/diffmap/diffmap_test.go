// SPDX-License-Identifier: MIT

package diffmap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type gitSource struct {
	t    *testing.T
	dir  string
	base string
}

func (s gitSource) Head(_ context.Context, p string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, p))
}
func (s gitSource) Base(_ context.Context, p string) ([]byte, error) {
	cmd := exec.Command("git", "show", s.base+":"+p)
	cmd.Dir = s.dir
	return cmd.Output()
}

const goBase = `package client

import "time"

const DefaultTimeout = 30 * time.Second

type Client struct{ n int }

func (c *Client) Do() error {
	return nil
}

func helper() int {
	return 1
}

func removed() {}
`

const goHead = `package client

import "time"

const DefaultTimeout = 5 * time.Second

type Client struct{ n int }

func (c *Client) Do() error {
	if c.n > 0 {
		return nil
	}
	return nil
}

func helper() int {
	return 1
}

func Added[T any](v T) T { return v }
`

const pyBase = `class Store:
    def get(self, k):
        return k

def top():
    pass
`

const pyHead = `class Store:
    def get(self, k):
        return k.strip()

def top():
    pass
`

func buildFixture(t *testing.T) *Map {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "client/client.go", goBase)
	write(t, dir, "store.py", pyBase)
	write(t, dir, "config.yaml", "a: 1\n")
	write(t, dir, "old_name.txt.go", "package x\n\nfunc Keep() {}\n")
	write(t, dir, "notes.xyz", "line1\nline2\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	write(t, dir, "client/client.go", goHead)
	write(t, dir, "store.py", pyHead)
	write(t, dir, "config.yaml", "a: 2\n")
	write(t, dir, "client/client_test.go", "package client\n\nimport \"testing\"\n\nfunc TestDo(t *testing.T) {}\n")
	write(t, dir, "notes.xyz", "line1\nline2 changed\n")
	git(t, dir, "mv", "old_name.txt.go", "new_name.go")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "head")
	head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	out := git(t, dir, "diff", "--no-color", "-M", "-U3", base, head)
	m, err := Build(context.Background(), ParseGitDiff(out), gitSource{t: t, dir: dir, base: base}, head, base)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ids(m *Map) []string {
	var out []string
	for _, u := range m.Units {
		out = append(out, u.ID)
	}
	return out
}

func TestBuildUnits(t *testing.T) {
	m := buildFixture(t)
	want := map[string]struct{ kind, change string }{
		"unit:client/client.go#(top-level)":      {KindModule, StatusModified},
		"unit:client/client.go#Client.Do":        {KindMethod, StatusModified},
		"unit:client/client.go#Added":            {KindFunction, StatusAdded},
		"unit:client/client.go#removed":          {KindFunction, StatusDeleted},
		"unit:store.py#Store.get":                {KindMethod, StatusModified},
		"unit:config.yaml":                       {KindConfig, StatusModified},
		"unit:client/client_test.go#TestDo":      {KindFunction, StatusAdded},
		"unit:client/client_test.go#(top-level)": {KindModule, StatusAdded},
		"unit:notes.xyz@L1":                      {KindHunk, StatusModified},
		"unit:new_name.go":                       {KindFile, StatusRenamed},
	}
	got := ids(m)
	for id, w := range want {
		u, ok := m.Unit(id)
		if !ok {
			t.Errorf("missing unit %s (have %v)", id, got)
			continue
		}
		if u.Kind != w.kind || u.Change != w.change {
			t.Errorf("%s: kind=%s change=%s, want %s/%s", id, u.Kind, u.Change, w.kind, w.change)
		}
	}
	if _, ok := m.Unit("unit:client/client.go#helper"); ok {
		t.Error("unchanged helper() should not be a unit")
	}
	if len(got) != len(want) {
		t.Errorf("got %d units %v, want %d", len(got), got, len(want))
	}

	do, _ := m.Unit("unit:client/client.go#Client.Do")
	if do.Additions != 3 || do.Deletions != 0 || len(do.NewRange) != 2 || do.NewRange[0] != 9 {
		t.Errorf("Client.Do = +%d -%d range %v", do.Additions, do.Deletions, do.NewRange)
	}
	tst, _ := m.Unit("unit:client/client_test.go#TestDo")
	if !tst.IsTest {
		t.Error("test file not flagged")
	}
	if fs := m.Files; !slices.ContainsFunc(fs, func(f FileSummary) bool { return f.Path == "new_name.go" && f.OldPath == "old_name.txt.go" }) {
		t.Errorf("rename not recorded: %+v", fs)
	}
}

func TestOrder(t *testing.T) {
	m := buildFixture(t)
	pos := func(id string) int { return slices.Index(m.Order, id) }
	if len(m.Order) != len(m.Units) {
		t.Fatalf("order has %d ids for %d units", len(m.Order), len(m.Units))
	}
	if !(pos("unit:client/client.go#Client.Do") < pos("unit:client/client_test.go#TestDo") &&
		pos("unit:client/client_test.go#TestDo") < pos("unit:config.yaml") &&
		pos("unit:config.yaml") < pos("unit:new_name.go")) {
		t.Errorf("unexpected order: %v", m.Order)
	}
}

func TestSummaryAndRender(t *testing.T) {
	m := buildFixture(t)
	for _, u := range m.Summary().Units {
		if u.Lines != nil {
			t.Fatalf("summary kept lines for %s", u.ID)
		}
	}
	u, _ := m.Unit("unit:client/client.go#(top-level)")
	r := u.Render()
	if !strings.Contains(r, "-") || !strings.Contains(r, "DefaultTimeout = 5") {
		t.Errorf("render:\n%s", r)
	}
}

func TestLanguages(t *testing.T) {
	cases := []struct {
		lang, src string
		want      []string
	}{
		{"typescript", "export class A {\n  m(): void {}\n}\nconst f = () => 1;\ninterface I { x(): void }\n", []string{"A", "A.m", "f", "I", "I.x"}},
		{"javascript", "function a() {}\nclass B { c() {} }\nconst d = function() {};\nconst e = 3;\n", []string{"a", "B", "B.c", "d"}},
		{"java", "class A {\n  A() {}\n  void m() {}\n}\n", []string{"A", "A.A", "A.m"}},
		{"rust", "struct S;\nimpl S {\n  fn new() -> S { S }\n}\nfn free() {}\n", []string{"S", "S", "S.new", "free"}},
		{"ruby", "module M\n  class C\n    def go; end\n  end\nend\n", []string{"M", "M.C", "M.C.go"}},
		{"go", "package p\ntype G[T any] struct{}\nfunc (g *G[T]) Run() {}\n", []string{"G", "G.Run"}},
	}
	for _, c := range cases {
		defs, ok := extractDefs(c.lang, []byte(c.src))
		if !ok {
			t.Errorf("%s: parse failed", c.lang)
			continue
		}
		var names []string
		for _, d := range defs {
			names = append(names, d.name)
		}
		if !slices.Equal(names, c.want) {
			t.Errorf("%s: defs = %v, want %v", c.lang, names, c.want)
		}
	}
}

func TestParseQuotedAndDeleted(t *testing.T) {
	out := "diff --git \"a/sp ace.go\" \"b/sp ace.go\"\ndeleted file mode 100644\nindex 1..0\n--- \"a/sp ace.go\"\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package x\n--- weird\n"
	files := ParseGitDiff(out)
	if len(files) != 1 || files[0].Path() != "sp ace.go" || files[0].Status != StatusDeleted {
		t.Fatalf("parsed %+v", files[0])
	}
	if a, d := countOps(allLines(files[0])); a != 0 || d != 2 {
		t.Errorf("ops +%d -%d", a, d)
	}
}
