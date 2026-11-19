// SPDX-License-Identifier: MIT

package history

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitIn(dir string) Runner {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.Output()
		return string(out), err
	}
}

func commit(t *testing.T, dir, file, msg, who string, when time.Time) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, msg)
	_ = f.Close()
	date := when.Format(time.RFC3339)
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME="+who, "GIT_AUTHOR_EMAIL="+strings.ToLower(who)+"@example.com",
			"GIT_COMMITTER_NAME="+who, "GIT_COMMITTER_EMAIL="+strings.ToLower(who)+"@example.com",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestHistoryRiskExpertise(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	commit(t, dir, "too_old.go", "ancient", "Old", base.AddDate(-3, 0, 0)) // outside the window
	// hot.go: 6 commits, 3 of them bug fixes, 3 authors. cold.go: 1 commit, long ago.
	commit(t, dir, "cold.go", "add cold", "Ann", base.AddDate(0, 0, -300))
	for i, who := range []string{"Ann", "Bob", "Cy", "Bob", "Bob", "Ann"} {
		msg := fmt.Sprintf("feature %d", i)
		if i%2 == 1 {
			msg = fmt.Sprintf("fix crash %d", i)
		}
		commit(t, dir, "hot.go", msg, who, base.AddDate(0, 0, -100+i*15))
	}
	commit(t, dir, "other.go", "add other", "Dee", base.AddDate(0, 0, -1))

	now, commits, total, err := Load(context.Background(), gitIn(dir), "HEAD", 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 || len(commits) != 8 {
		t.Fatalf("total=%d window=%d", total, len(commits))
	}
	if !now.Equal(base.AddDate(0, 0, -1)) {
		t.Fatalf("now = %v, want the head commit time", now)
	}
	// Anchor at a fixed time for deterministic assertions.
	now = base
	st := Stats(now, commits, []string{"hot.go", "cold.go", "new.go"}, BugfixRE([]string{"fix", "bug"}))
	hot := st["hot.go"]
	if hot.Commits365 != 6 || hot.Bugfix365 != 3 || hot.Authors365 != 3 || hot.Commits90 != 5 || hot.Commits30 != 1 || hot.LastChangeDays != 25 {
		t.Errorf("hot = %+v", hot)
	}
	if st["new.go"].Commits365 != 0 || st["new.go"].LastChangeDays != -1 {
		t.Errorf("new = %+v", st["new.go"])
	}
	if hot.HotspotPercentile <= st["cold.go"].HotspotPercentile {
		t.Errorf("hotspot ordering: hot %v cold %v", hot.HotspotPercentile, st["cold.go"].HotspotPercentile)
	}

	r := Score(st, map[string]int{"hot.go": 40, "cold.go": 400}, len(commits), total, 5)
	if r.Confidence != "medium" || len(r.Files) != 2 || r.Files[0].Path != "hot.go" {
		// cold.go has <3 commits → half the files low-history → medium
		t.Errorf("risk = %+v", r)
	}
	if r.Score <= 0 || r.Score > 100 || r.Files[0].Components["bug_ratio"] != 0.5 {
		t.Errorf("risk score = %+v", r)
	}
	sparse := Score(st, map[string]int{"hot.go": 1}, 3, 3, 20)
	if sparse.Confidence != "low" || !strings.Contains(sparse.ConfidenceReason, "sparse history") {
		t.Errorf("sparse = %+v", sparse)
	}

	ex := Expertise(now, commits, []string{"hot.go"})
	if len(ex) != 3 || ex[0].Email != "bob@example.com" || ex[0].Commits != 3 {
		t.Errorf("expertise = %+v", ex)
	}
}
