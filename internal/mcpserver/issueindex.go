// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/issueindex"
)

// M6d: local BM25 issue index (SPEC §8.2 v2, ADR 0013).

func registerIndexTools(s *mcp.Server, h *handlers) {
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "issues_index_sync", Annotations: local,
		Description: "Build or refresh the local issue index for the run's repository (SQLite FTS5, BM25 ranking). The first sync " +
			"fetches every issue; later syncs fetch only issues updated since the last one. issues_search uses the index once it exists. " +
			"Local cache only; reads the forge, writes nothing to it."},
		h.issuesIndexSync)
}

func (h *handlers) openIndex(repo forge.Repo) (*issueindex.Index, error) {
	return issueindex.Open(issueindex.Path(h.cfg.CacheDir, repo))
}

type IndexSyncOut struct {
	issueindex.SyncResult
	Path string `json:"path"`
}

func (h *handlers) issuesIndexSync(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, IndexSyncOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, IndexSyncOut{}, err
	}
	p, err := h.cfg.Providers.Get(ctx, st.Repo)
	if err != nil {
		return nil, IndexSyncOut{}, err
	}
	l, ok := p.(issueindex.Lister)
	if !ok {
		return nil, IndexSyncOut{}, errors.New("this forge adapter cannot list issues for indexing")
	}
	h.indexMu.Lock()
	defer h.indexMu.Unlock()
	ix, err := h.openIndex(st.Repo)
	if err != nil {
		return nil, IndexSyncOut{}, err
	}
	defer func() { _ = ix.Close() }()
	res, err := ix.Sync(ctx, l, st.Repo, h.now())
	if err != nil {
		return nil, IndexSyncOut{}, fmt.Errorf("sync issue index (kept what was synced so far): %w", err)
	}
	return nil, IndexSyncOut{SyncResult: res, Path: issueindex.Path(h.cfg.CacheDir, st.Repo)}, nil
}

// indexSearch answers issues_search from the local index when one exists.
// ok is false when there is no index, so the caller uses the forge search.
func (h *handlers) indexSearch(ctx context.Context, st *checkoutState, queries []string, closedWithin time.Duration, limit int) (hits []forge.Issue, scores map[string]float64, note string, ok bool) {
	path := issueindex.Path(h.cfg.CacheDir, st.Repo)
	if _, err := os.Stat(path); err != nil {
		return nil, nil, "", false
	}
	h.indexMu.Lock()
	defer h.indexMu.Unlock()
	ix, err := issueindex.Open(path)
	if err != nil {
		return nil, nil, "", false
	}
	defer func() { _ = ix.Close() }()
	stats := ix.Stats()
	if stats.Issues == 0 {
		return nil, nil, "", false
	}
	best := map[int]issueindex.Hit{}
	for _, q := range queries {
		hs, err := ix.Search(ctx, q, closedWithin, h.now(), limit)
		if err != nil {
			continue
		}
		for _, hit := range hs {
			if cur, seen := best[hit.Issue.Number]; !seen || hit.Score > cur.Score {
				best[hit.Issue.Number] = hit
			}
		}
	}
	ranked := make([]issueindex.Hit, 0, len(best))
	for _, hit := range best {
		ranked = append(ranked, hit)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	scores = map[string]float64{}
	for _, hit := range ranked {
		hits = append(hits, hit.Issue)
		scores[fmt.Sprintf("#%d", hit.Issue.Number)] = hit.Score
	}
	note = fmt.Sprintf("ranked by the local BM25 index (%d issues, synced %s); scores compare within one query only",
		stats.Issues, stats.SyncedAt.Format(time.RFC3339))
	return hits, scores, note, true
}
