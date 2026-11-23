// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestActivityGitLab(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case mrPath:
			fmt.Fprint(w, `{"iid":7,"author":{"username":"carol"},"created_at":"2026-11-01T09:00:00Z","merged_at":"2026-11-03T09:00:00Z",
				"diff_refs":{"head_sha":"h"}}`)
		case mrPath + "/notes":
			fmt.Fprint(w, `[{"id":1,"author":{"username":"dave"},"body":"approved this merge request","system":true,"created_at":"2026-11-02T12:00:00Z"},
				{"id":2,"author":{"username":"dave"},"body":"added 1 commit","system":true,"created_at":"2026-11-02T11:00:00Z"},
				{"id":3,"author":{"username":"dave"},"body":"off by one?\n\n<sub>Drafted with aireviewer (AI-assisted).</sub>","system":false,
				 "created_at":"2026-11-01T15:00:00Z","resolved":true,"position":{"new_path":"x.go","new_line":12}},
				{"id":4,"author":{"username":"carol"},"body":"fixed","system":false,"created_at":"2026-11-01T18:00:00Z"}]`)
		case mrPath + "/commits":
			fmt.Fprint(w, `[{"id":"c1","title":"retry","author_name":"Carol","committed_date":"2026-11-01T08:00:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	})
	a, err := newTestProvider(t, h).Activity(context.Background(), &forge.PR{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if a.Author != "carol" || a.MergedAt == nil || len(a.Events) != 4 {
		t.Fatalf("activity = %+v", a)
	}
	kinds := ""
	for _, e := range a.Events {
		kinds += e.Kind + " "
	}
	if kinds != "commit inline comment approval " {
		t.Errorf("kinds = %q (system notes other than approvals must be skipped)", kinds)
	}
	in := a.Events[1]
	if in.Path != "x.go" || in.Line != 12 || !in.AIAssisted || !in.Resolved || in.Author != "dave" {
		t.Errorf("inline = %+v", in)
	}
	if ap := a.Events[3]; ap.State != "approved" || ap.Author != "dave" {
		t.Errorf("approval = %+v", ap)
	}
	if c := a.Events[0]; c.SHA != "c1" || c.Body != "retry" {
		t.Errorf("commit = %+v", c)
	}
}
