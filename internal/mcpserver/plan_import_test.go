// SPDX-License-Identifier: MIT

package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuntimeImport copies results between runs at the same head only.
func TestRuntimeImport(t *testing.T) {
	src, head := runtimeRepo(t, runtimeYML)
	cs, root := runtimeServerRoot(t, src, head, newFake())
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	prep := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run})
	call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": prep.Proposal.ProposalID, "hash": prep.Proposal.Hash})
	call[RuntimeRunOut](t, cs, "runtime_run", map[string]any{"run_id": run})
	call[LintOut](t, cs, "lint_run", map[string]any{"run_id": run})

	// A second run of the same repository at the same head (as after
	// /pr-create opens the PR without committing fixes).
	co, err := os.ReadFile(filepath.Join(root, ".aireviewer", "runs", run, "checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(id string, data []byte) {
		dir := filepath.Join(root, ".aireviewer", "runs", id)
		_ = os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, "checkout.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("same-head", co)
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": "same-head"})
	imp := call[RuntimeImportOut](t, cs, "runtime_import", map[string]any{"run_id": "same-head", "from_run_id": run})
	joined := strings.Join(imp.Imported, " ")
	if !strings.Contains(joined, "/results.json") || !strings.Contains(joined, "lint/"+head+"/findings.json") || !strings.Contains(joined, "-cmd1-run1.log") {
		t.Fatalf("imported = %v", imp.Imported)
	}
	if res := call[RuntimeResults](t, cs, "runtime_results", map[string]any{"run_id": "same-head"}); res.Summary["tests_new_failure"] != 1 {
		t.Errorf("imported results = %+v", res.Summary)
	}
	ev := call[EvidenceOut](t, cs, "evidence_check", map[string]any{"run_id": "same-head", "evidence": []map[string]any{
		{"kind": "metric", "source": "runtime", "name": "tests_flaky", "value": 1},
		{"kind": "metric", "source": "lint", "name": "changed", "value": 1}}})
	if !ev.Results[0].OK || !ev.Results[1].OK {
		t.Errorf("imported metrics = %+v", ev.Results)
	}

	// Different head (fixes committed after testing): refused.
	put("moved-head", []byte(strings.ReplaceAll(string(co), head, strings.Repeat("e", 40))))
	if msg := callErr(t, cs, "runtime_import", map[string]any{"run_id": "moved-head", "from_run_id": run}); !strings.Contains(msg, "stale") {
		t.Errorf("stale import: %s", msg)
	}
}

// TestReviewPlanUsesSignals: once the deterministic augmentation tools have
// run, review batches are risk-ordered and carry per-unit signals.
func TestReviewPlanUsesSignals(t *testing.T) {
	cs, _, _ := reviewServer(t)
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	before := call[PlanOut](t, cs, "review_plan", map[string]any{"run_id": run})
	if before.RiskOrdered || len(before.Signals) != 0 {
		t.Errorf("signals before augmentation: %+v", before)
	}
	call[RepoMetricsOut](t, cs, "repo_metrics", map[string]any{"run_id": run})
	call[SymbolRefsOut](t, cs, "symbol_refs", map[string]any{"run_id": run})
	after := call[PlanOut](t, cs, "review_plan", map[string]any{"run_id": run})
	s, ok := after.Signals["unit:svc/svc.go#Retry"]
	if !after.RiskOrdered || !ok || s.FileRisk == nil || s.RefFiles == nil || *s.RefFiles != 1 {
		t.Fatalf("plan = %+v", after)
	}
	ev := call[EvidenceOut](t, cs, "evidence_check", map[string]any{"run_id": run, "evidence": []map[string]any{
		{"kind": "metric", "source": "repo_metrics", "name": "svc/svc.go.risk", "value": *s.FileRisk},
		{"kind": "metric", "source": "symbol_refs", "name": "unit:svc/svc.go#Retry.ref_files", "value": *s.RefFiles}}})
	if !ev.Results[0].OK || !ev.Results[1].OK {
		t.Errorf("signals are not citable metrics: %+v", ev.Results)
	}
}
