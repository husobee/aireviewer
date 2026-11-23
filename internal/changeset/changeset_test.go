// SPDX-License-Identifier: MIT

package changeset

import (
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

var (
	api    = forge.Repo{Forge: forge.GitHub, Host: "github.com", Project: "o/api"}
	client = forge.Repo{Forge: forge.GitLab, Host: "gitlab.example.com", Project: "g/client"}
)

func TestIDAndKey(t *testing.T) {
	if ID([]string{"b", "a"}) != ID([]string{"a", "b"}) || !strings.HasPrefix(ID([]string{"a"}), "changeset-") {
		t.Error("id must be order-independent")
	}
	ms := []Member{{RunID: "a", HeadSHA: "1"}, {RunID: "b", HeadSHA: "2"}}
	k := Key(ms)
	ms[1].HeadSHA = "3"
	if Key(ms) == k {
		t.Error("key must change when a head moves")
	}
	q := Qualify(client, "unit:pkg/x.go#Do")
	if q != "unit:gitlab.example.com/g/client:pkg/x.go#Do" {
		t.Errorf("qualify = %s", q)
	}
	repo, u, ok := Unqualify(q)
	if !ok || repo != "gitlab.example.com/g/client" || u != "unit:pkg/x.go#Do" {
		t.Errorf("unqualify = %s %s %v", repo, u, ok)
	}
	if _, _, ok := Unqualify("unit:a.go#F"); ok {
		t.Error("unqualified id accepted")
	}
}

func TestAnalyzeOrderAndProblems(t *testing.T) {
	ms := []Member{
		{RunID: "client", Ref: "g/client!4", Repo: client, State: "open"},
		{RunID: "api", Ref: "o/api#1", Repo: api, State: "open", HeadBranch: "v2"},
		{RunID: "api-docs", Ref: "o/api#2", Repo: api, State: "open", BaseBranch: "v2"},
	}
	o := Analyze(ms, []Edge{
		{From: "api", To: "client", Symbol: "GetV2", Change: "added", UsedAtHead: true},
		{From: "api", To: "client", Symbol: "Limits", Change: "modified", UsedAtHead: true},
	})
	if len(o.Problems) != 0 || len(o.Sequence) != 3 || o.Sequence[0] != "api" || len(o.Contracts) != 1 {
		t.Fatalf("order = %+v", o)
	}
	kinds := map[string]bool{}
	for _, c := range o.Constraints {
		kinds[c.Kind] = true
	}
	if !kinds["stacked"] || !kinds["introduces"] {
		t.Errorf("constraints = %+v", o.Constraints)
	}

	// Several symbols between the same pair make one constraint naming all.
	o = Analyze(ms[:2], []Edge{
		{From: "api", To: "client", Symbol: "Lookup", Change: "added", UsedAtHead: true},
		{From: "api", To: "client", Symbol: "Copy", Change: "added", UsedAtHead: true},
	})
	if len(o.Constraints) != 1 || !strings.Contains(o.Constraints[0].Reason, "uses Lookup, Copy, which o/api#1 introduces") {
		t.Errorf("merged constraint = %+v", o.Constraints)
	}

	// Removal both ways makes a cycle; a consumer still using a removed
	// symbol is incompatible regardless of order.
	o = Analyze(ms[:2], []Edge{
		{From: "api", To: "client", Symbol: "GetV2", Change: "added", UsedAtHead: true},
		{From: "api", To: "client", Symbol: "GetV1", Change: "deleted", UsedAtBase: true},
		{From: "api", To: "client", Symbol: "Ping", Change: "deleted", UsedAtHead: true},
	})
	var kindsP []string
	for _, p := range o.Problems {
		kindsP = append(kindsP, p.Kind)
	}
	got := strings.Join(kindsP, ",")
	if !strings.Contains(got, "incompatible") || !strings.Contains(got, "cycle") || len(o.Sequence) != 0 {
		t.Errorf("problems = %+v sequence = %v", o.Problems, o.Sequence)
	}
	if !strings.Contains(o.Problems[0].Detail, "g/client!4 still uses Ping") {
		t.Errorf("detail = %s", o.Problems[0].Detail)
	}

	// A member stacked behind a cycle is reported as blocked, not on it.
	o = Analyze(ms, []Edge{
		{From: "api", To: "client", Symbol: "GetV2", Change: "added", UsedAtHead: true},
		{From: "api", To: "client", Symbol: "GetV1", Change: "deleted", UsedAtBase: true},
	})
	if len(o.Problems) != 1 || len(o.Problems[0].Members) != 2 || !strings.Contains(o.Problems[0].Detail, "blocked behind them: o/api#2") {
		t.Errorf("cycle = %+v", o.Problems)
	}

	// Merged members come first; merging the consumer first is flagged.
	ms2 := []Member{
		{RunID: "client", Ref: "g/client!4", Repo: client, State: "merged"},
		{RunID: "api", Ref: "o/api#1", Repo: api, State: "open"},
	}
	o = Analyze(ms2, []Edge{{From: "api", To: "client", Symbol: "GetV2", Change: "added", UsedAtHead: true}})
	if len(o.Merged) != 1 || len(o.Problems) != 1 || o.Problems[0].Kind != "merged_out_of_order" {
		t.Errorf("merged = %+v", o)
	}
}
