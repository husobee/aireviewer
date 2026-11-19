// SPDX-License-Identifier: MIT

package runctx

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestRunIDs(t *testing.T) {
	ref := forge.PRRef{Repo: forge.Repo{Forge: forge.GitLab, Host: "gitlab.com", Project: "grp/sub/proj"}, Number: 7}
	if got, want := RunIDForPR(ref), "gitlab-gitlab.com-grp-sub-proj-pr-7"; got != want {
		t.Errorf("RunIDForPR = %q, want %q", got, want)
	}
	repo := forge.Repo{Forge: forge.GitHub, Host: "github.com", Project: "o/r"}
	if got, want := RunIDForBranch(repo, "feat/x y"), "github-github.com-o-r-branch-feat-x-y"; got != want {
		t.Errorf("RunIDForBranch = %q, want %q", got, want)
	}
}

func TestPutGetList(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Put("run-1", "augmentation/abc/summary.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("run-1", "context.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Get("run-1", "context.json")
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("Get = %q, %v", b, err)
	}
	paths, err := s.List("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"augmentation/abc/summary.json", "context.json"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("List = %v, want %v", paths, want)
	}
	if _, err := s.Get("run-1", "missing.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file err = %v", err)
	}
	if paths, err := s.List("never-ran"); err != nil || len(paths) != 0 {
		t.Errorf("List(empty) = %v, %v", paths, err)
	}
}

func TestRejectsEscapes(t *testing.T) {
	s := New(t.TempDir())
	bad := []struct{ run, path string }{
		{"run", "../escape"},
		{"run", "/etc/passwd"},
		{"run", "a/../../b"},
		{"../run", "x"},
		{"run/../x", "x"},
		{"", "x"},
		{"run", ""},
	}
	for _, b := range bad {
		if _, err := s.Put(b.run, b.path, []byte("x")); !errors.Is(err, ErrBadPath) {
			t.Errorf("Put(%q, %q) err = %v, want ErrBadPath", b.run, b.path, err)
		}
	}
}
