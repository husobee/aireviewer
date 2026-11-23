// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/changeset"
	"github.com/husobee/aireviewer/internal/diffmap"
	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/schema"
)

// SPEC §21 (slice 1): reviewing a cross-cutting change made of several PRs,
// possibly across repositories and forges (ADR 0014). Each member keeps its
// own PR run (review, gates and posting are unchanged and per PR); the
// change-set run adds the cross-PR map, merge order and a set-level report.

func registerChangesetTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "changeset_open", Annotations: local,
		Description: fmt.Sprintf("Open a change set: 2-%d PR/MR URLs (any mix of repositories and forges) reviewed as one change. "+
			"Checks out every member as a normal PR run (diff-map and linked issues included) and returns the set run id and the "+
			"member run ids; review each member with the usual tools, then use changeset_map and changeset_order.", changeset.MaxMembers)},
		h.changesetOpen)
	mcp.AddTool(s, &mcp.Tool{Name: "changeset_get", Annotations: ro,
		Description: "Return a change set's members with their current state, and whether the set is stale (a member head moved since the set was opened)."},
		h.changesetGet)
	mcp.AddTool(s, &mcp.Tool{Name: "changeset_discover", Annotations: ro,
		Description: "Suggest PRs that may belong in the same change as a checked-out PR: stacked PRs (targeting its branch, or the PR " +
			"its base branch belongs to) and PRs referenced from its description. Suggestions only: the human decides what goes in the set."},
		h.changesetDiscover)
	mcp.AddTool(s, &mcp.Tool{Name: "changeset_map", Annotations: local,
		Description: "Build the cross-PR map: for symbols each member adds, changes or removes, find their uses in every other member " +
			"(at its head and its merge base). Edges carry repo-qualified unit ids and use locations; counts are recorded as metrics " +
			"(source changeset). Textual matching: verify important edges with code_read on the member run."},
		h.changesetMap)
	mcp.AddTool(s, &mcp.Tool{Name: "changeset_order", Annotations: local,
		Description: "Deterministic merge-order and compatibility analysis from stacking and the cross-PR map: required order " +
			"constraints, a merge sequence, contracts to review (modified symbols used across PRs), and problems no order can fix " +
			"(a member still using a removed symbol, cycles, members merged out of order)."},
		h.changesetOrder)
}

func (h *handlers) loadSet(runID string) (*changeset.Set, error) {
	var s changeset.Set
	if err := h.loadJSON(runID, "changeset.json", &s); err != nil {
		return nil, err
	}
	if s.ID == "" {
		return nil, fmt.Errorf("run %q is not a change set; open one with changeset_open", runID)
	}
	return &s, nil
}

func (h *handlers) memberOf(st *checkoutState, pr *forge.PR, runID, url string) changeset.Member {
	return changeset.Member{RunID: runID, URL: url, Ref: pr.Ref.String(), Repo: st.Repo, Number: st.Number, Title: pr.Title,
		Author: pr.Author, State: string(pr.State), BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: st.HeadSHA, MergeBase: st.MergeBase}
}

// ---- changeset_open ----

type ChangesetOpenIn struct {
	URLs []string `json:"urls" jsonschema:"2-8 PR/MR URLs that together make one change"`
	Name string   `json:"name,omitempty"`
}

type ChangesetOut struct {
	RunID   string             `json:"run_id" jsonschema:"the change-set run id (for changeset_* tools and the set-level report)"`
	Set     changeset.Set      `json:"set"`
	Stale   bool               `json:"stale"`
	Notes   []string           `json:"notes"`
	Issues  []forge.Issue      `json:"issues,omitempty" jsonschema:"union of the members' linked issues (untrusted)"`
	Changes map[string]float64 `json:"changed_lines,omitempty" jsonschema:"changed lines per member run"`
}

func (h *handlers) changesetOpen(ctx context.Context, _ *mcp.CallToolRequest, in ChangesetOpenIn) (*mcp.CallToolResult, ChangesetOut, error) {
	urls := map[string]bool{}
	var list []string
	for _, u := range in.URLs {
		u = strings.TrimSpace(u)
		if u != "" && !urls[u] {
			urls[u] = true
			list = append(list, u)
		}
	}
	if len(list) < 2 || len(list) > changeset.MaxMembers {
		return nil, ChangesetOut{}, fmt.Errorf("a change set needs 2-%d distinct PR URLs (got %d)", changeset.MaxMembers, len(list))
	}
	out := ChangesetOut{Notes: []string{}, Changes: map[string]float64{}}
	var members []changeset.Member
	seenRun := map[string]bool{}
	for _, u := range list {
		r, err := h.resolve(u)
		if err != nil {
			return nil, ChangesetOut{}, fmt.Errorf("%s: %w", u, err)
		}
		if seenRun[r.RunID] {
			continue
		}
		seenRun[r.RunID] = true
		if _, _, err := h.prCheckout(ctx, nil, URLIn{URL: u}); err != nil {
			return nil, ChangesetOut{}, fmt.Errorf("checkout %s: %w", r.Short, err)
		}
		if _, dm, err := h.diffmapBuild(ctx, nil, RunIn{RunID: r.RunID}); err != nil {
			return nil, ChangesetOut{}, fmt.Errorf("diff-map %s: %w", r.Short, err)
		} else if dm.Map != nil {
			n := 0
			for _, un := range dm.Map.Units {
				n += un.Additions + un.Deletions
			}
			out.Changes[r.RunID] = float64(n)
		}
		if _, is, err := h.prIssues(ctx, nil, RunIn{RunID: r.RunID}); err == nil {
			out.Issues = append(out.Issues, is.Issues...)
		}
		st, err := h.loadCheckout(r.RunID)
		if err != nil {
			return nil, ChangesetOut{}, err
		}
		pr, err := h.loadPR(r.RunID)
		if err != nil {
			return nil, ChangesetOut{}, err
		}
		members = append(members, h.memberOf(st, pr, r.RunID, u))
	}
	if len(members) < 2 {
		return nil, ChangesetOut{}, errors.New("the URLs name fewer than 2 distinct PRs")
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.RunID
	}
	set := changeset.Set{ID: changeset.ID(ids), Name: strings.TrimSpace(in.Name), Members: members, Key: changeset.Key(members), Created: h.now()}
	// The set run reuses report slots and validation: its checkout records
	// the staleness key as its "head".
	if err := h.saveJSON(set.ID, "changeset.json", set); err != nil {
		return nil, ChangesetOut{}, err
	}
	if err := h.saveJSON(set.ID, "checkout.json", checkoutState{Repo: members[0].Repo, Mode: "changeset", Checkout: gitrepo.Checkout{HeadSHA: set.Key}}); err != nil {
		return nil, ChangesetOut{}, err
	}
	if err := h.saveJSON(set.ID, "issues.json", dedupeIssues(out.Issues)); err != nil {
		return nil, ChangesetOut{}, err
	}
	_, _ = h.logEvent(set.ID, "changeset_opened", "orchestrator", map[string]any{"members": ids, "key": set.Key})
	repos := map[forge.Repo]bool{}
	for _, m := range members {
		repos[m.Repo] = true
		if m.State != "open" {
			out.Notes = append(out.Notes, fmt.Sprintf("%s is %s: kept as read-only context and marked done in the merge order", m.Ref, m.State))
		}
	}
	out.Notes = append(out.Notes, fmt.Sprintf("%d PRs across %d repositories", len(members), len(repos)))
	out.RunID, out.Set, out.Issues = set.ID, set, dedupeIssues(out.Issues)
	return nil, out, nil
}

func dedupeIssues(is []forge.Issue) []forge.Issue {
	seen := map[string]bool{}
	out := []forge.Issue{}
	for _, i := range is {
		k := strings.ToLower(i.Ref)
		if !seen[k] {
			seen[k] = true
			out = append(out, i)
		}
	}
	return out
}

// ---- changeset_get ----

func (h *handlers) changesetGet(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, ChangesetOut, error) {
	set, err := h.loadSet(in.RunID)
	if err != nil {
		return nil, ChangesetOut{}, err
	}
	out := ChangesetOut{RunID: set.ID, Set: *set, Notes: []string{}}
	for i, m := range set.Members {
		p, err := h.cfg.Providers.Get(ctx, m.Repo)
		if err != nil {
			continue
		}
		pr, err := p.GetPR(ctx, forge.PRRef{Repo: m.Repo, Number: m.Number})
		if err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("%s: could not refresh (%v)", m.Ref, err))
			continue
		}
		out.Set.Members[i].State = string(pr.State)
		if pr.HeadSHA != "" && pr.HeadSHA != m.HeadSHA {
			out.Stale = true
			out.Notes = append(out.Notes, fmt.Sprintf("%s moved from %s to %s: re-open the set to analyse the new head", m.Ref, short(m.HeadSHA), short(pr.HeadSHA)))
		}
	}
	if is, ok := h.loadIssues(set.ID); ok {
		out.Issues = is
	}
	return nil, out, nil
}

// ---- changeset_discover ----

type DiscoverIn struct {
	RunID string `json:"run_id" jsonschema:"a checked-out PR run"`
}

type Suggestion struct {
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Title  string `json:"title,omitempty" jsonschema:"untrusted"`
	Reason string `json:"reason"`
}

type DiscoverOut struct {
	Suggestions []Suggestion `json:"suggestions"`
	Notes       []string     `json:"notes"`
}

var (
	prURLRe   = regexp.MustCompile(`https?://[^\s)>\]]+/(?:pull|-/merge_requests)/\d+`)
	dependsRe = regexp.MustCompile(`(?i)\b(?:depends on|requires|blocked by|after|stacked on)\s+[!#](\d+)`)
)

func (h *handlers) changesetDiscover(ctx context.Context, _ *mcp.CallToolRequest, in DiscoverIn) (*mcp.CallToolResult, DiscoverOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, DiscoverOut{}, err
	}
	pr, err := h.loadPR(in.RunID)
	if err != nil {
		return nil, DiscoverOut{}, err
	}
	out := DiscoverOut{Suggestions: []Suggestion{}, Notes: []string{"suggestions only: confirm with the human before adding any to a change set"}}
	seen := map[string]bool{pr.WebURL: true}
	add := func(s Suggestion) {
		if s.URL != "" && !seen[s.URL] {
			seen[s.URL] = true
			out.Suggestions = append(out.Suggestions, s)
		}
	}
	p, err := h.cfg.Providers.Get(ctx, st.Repo)
	if err == nil {
		if l, ok := p.(forge.BaseBranchLister); ok && pr.HeadBranch != "" {
			if prs, err := l.ListOpenPRsByBase(ctx, st.Repo, pr.HeadBranch); err == nil {
				for _, c := range prs {
					add(Suggestion{URL: c.WebURL, Ref: c.Ref.String(), Title: c.Title, Reason: fmt.Sprintf("stacked: targets this PR's branch %q", pr.HeadBranch)})
				}
			}
		}
		if f, ok := p.(forge.BranchPRFinder); ok && pr.BaseBranch != "" {
			if c, err := f.FindOpenPR(ctx, st.Repo, pr.BaseBranch); err == nil && c != nil {
				add(Suggestion{URL: c.WebURL, Ref: c.Ref.String(), Title: c.Title, Reason: fmt.Sprintf("stacked: this PR targets its branch %q", pr.BaseBranch)})
			}
		}
	}
	for _, u := range prURLRe.FindAllString(pr.Body, 10) {
		if r, err := h.resolve(u); err == nil {
			add(Suggestion{URL: u, Ref: r.Short, Reason: "linked from this PR's description"})
		}
	}
	for _, m := range dependsRe.FindAllStringSubmatch(pr.Body, 10) {
		var n int
		_, _ = fmt.Sscanf(m[1], "%d", &n)
		if n > 0 && n != st.Number {
			if c, err := p.GetPR(ctx, forge.PRRef{Repo: st.Repo, Number: n}); err == nil && c.WebURL != "" {
				add(Suggestion{URL: c.WebURL, Ref: c.Ref.String(), Title: c.Title, Reason: fmt.Sprintf("description says %q", strings.TrimSpace(m[0]))})
			}
		}
	}
	return nil, out, nil
}

// ---- changeset_map ----

type MapOut struct {
	RunID string           `json:"run_id"`
	Key   string           `json:"key"`
	Edges []changeset.Edge `json:"edges"`
	Notes []string         `json:"notes"`
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func changeOf(s string) string {
	switch s {
	case diffmap.StatusAdded:
		return "added"
	case diffmap.StatusDeleted:
		return "deleted"
	default:
		return "modified"
	}
}

func (h *handlers) changesetMap(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, MapOut, error) {
	set, err := h.loadSet(in.RunID)
	if err != nil {
		return nil, MapOut{}, err
	}
	out := MapOut{RunID: set.ID, Key: set.Key, Edges: []changeset.Edge{}, Notes: []string{}}
	maps := map[string]*diffmap.Map{}
	for _, m := range set.Members {
		dm, err := h.loadMap(m.RunID)
		if err != nil {
			return nil, MapOut{}, fmt.Errorf("%s: %w", m.Ref, err)
		}
		maps[m.RunID] = dm
	}
	stacked := func(a, b changeset.Member) bool {
		return a.Repo == b.Repo && (b.BaseBranch == a.HeadBranch || a.BaseBranch == b.HeadBranch)
	}
	kv := map[string]any{}
	for _, a := range set.Members {
		am := maps[a.RunID]
		aFiles := map[string]bool{}
		for _, f := range am.Files {
			aFiles[f.Path] = true
		}
		n := 0
		for _, id := range am.Order {
			u, _ := am.Unit(id)
			if u.IsTest || (u.Kind != diffmap.KindFunction && u.Kind != diffmap.KindMethod && u.Kind != diffmap.KindClass) {
				continue
			}
			sym := symbolName(u)
			if len(sym) < 4 || !identRe.MatchString(sym) {
				continue
			}
			if n++; n > 40 {
				out.Notes = append(out.Notes, a.Ref+": only the first 40 changed symbols were mapped")
				break
			}
			for _, b := range set.Members {
				if b.RunID == a.RunID {
					continue
				}
				g := h.git(b.Repo)
				pat := `(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(sym) + `([^A-Za-z0-9_]|$)`
				head, err := g.Grep(ctx, b.HeadSHA, pat, nil, false)
				if err != nil {
					return nil, MapOut{}, err
				}
				// In the same repository, a stacked member's head contains the
				// provider's own code: uses in files the provider changed
				// belong to the provider, unless the consumer changes them too.
				bFiles := map[string]bool{}
				for _, f := range maps[b.RunID].Files {
					bFiles[f.Path] = true
				}
				own := func(file string) bool {
					return a.Repo == b.Repo && aFiles[file] && (stacked(a, b) && !bFiles[file] || file == u.File)
				}
				e := changeset.Edge{From: a.RunID, FromUnit: changeset.Qualify(a.Repo, u.ID), Symbol: sym, Change: changeOf(u.Change),
					Interface: interfaceFile.MatchString(u.File), To: b.RunID, Uses: []changeset.Use{}}
				for _, ln := range head {
					parts := strings.SplitN(ln, ":", 3)
					if len(parts) < 3 || own(parts[0]) {
						continue
					}
					e.UsedAtHead = true
					if len(e.Uses) < 5 {
						var line int
						_, _ = fmt.Sscanf(parts[1], "%d", &line)
						text := parts[2]
						if len(text) > 160 {
							text = text[:160] + "…"
						}
						e.Uses = append(e.Uses, changeset.Use{File: parts[0], Line: line, Text: text})
					}
				}
				if b.MergeBase != "" {
					if base, err := g.Grep(ctx, b.MergeBase, pat, nil, false); err == nil {
						for _, ln := range base {
							if f, _, _ := strings.Cut(ln, ":"); !own(f) {
								e.UsedAtBase = true
								break
							}
						}
					}
				}
				if e.UsedAtHead || e.UsedAtBase {
					out.Edges = append(out.Edges, e)
				}
			}
		}
	}
	sort.SliceStable(out.Edges, func(i, j int) bool {
		rank := map[string]int{"deleted": 0, "added": 1, "modified": 2}
		return rank[out.Edges[i].Change] < rank[out.Edges[j].Change]
	})
	kv["edges"] = len(out.Edges)
	pair := map[string]int{}
	for _, e := range out.Edges {
		pair[e.From+">"+e.To]++
	}
	for k, v := range pair {
		kv["edges."+k] = v
	}
	if err := h.saveJSON(set.ID, "changeset-data/"+set.Key+"/map.json", out); err != nil {
		return nil, MapOut{}, err
	}
	if err := h.recordMetrics(set.ID, "changeset", kv); err != nil {
		return nil, MapOut{}, err
	}
	return nil, out, nil
}

// ---- changeset_order ----

type OrderOut struct {
	changeset.Order
	SequenceRefs []string `json:"sequence_refs" jsonschema:"the merge sequence as forge references"`
}

func (h *handlers) changesetOrder(_ context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, OrderOut, error) {
	set, err := h.loadSet(in.RunID)
	if err != nil {
		return nil, OrderOut{}, err
	}
	var mo MapOut
	if err := h.loadJSON(set.ID, "changeset-data/"+set.Key+"/map.json", &mo); err != nil || mo.Key != set.Key {
		return nil, OrderOut{}, errors.New("no cross-PR map for this set's heads: call changeset_map first")
	}
	o := changeset.Analyze(set.Members, mo.Edges)
	out := OrderOut{Order: o, SequenceRefs: []string{}}
	ref := map[string]string{}
	for _, m := range set.Members {
		ref[m.RunID] = m.Ref
	}
	for _, id := range o.Sequence {
		out.SequenceRefs = append(out.SequenceRefs, ref[id])
	}
	if err := h.saveJSON(set.ID, "changeset-data/"+set.Key+"/order.json", out); err != nil {
		return nil, OrderOut{}, err
	}
	kinds := map[string]int{}
	for _, p := range o.Problems {
		kinds[p.Kind]++
	}
	kv := map[string]any{"constraints": len(o.Constraints), "contracts": len(o.Contracts), "problems": len(o.Problems),
		"problems.incompatible": kinds["incompatible"], "problems.cycle": kinds["cycle"], "problems.merged_out_of_order": kinds["merged_out_of_order"],
		"sequence": strings.Join(out.SequenceRefs, " -> ")}
	if err := h.recordMetrics(set.ID, "changeset_order", kv); err != nil {
		return nil, OrderOut{}, err
	}
	return nil, out, nil
}

// ---- set-level report validation ----

// changesetEnv validates a set-level report: code evidence names its member
// run, anchors are repo-qualified unit ids of member diff-maps, issue
// evidence may quote any member's linked issue.
func (h *handlers) changesetEnv(ctx context.Context, setID string, st *checkoutState, stage, selfAgent string) (schema.Env, error) {
	set, err := h.loadSet(setID)
	if err != nil {
		return schema.Env{}, err
	}
	members := map[string]changeset.Member{}
	byRepo := map[string]string{}
	for _, m := range set.Members {
		members[m.RunID] = m
		byRepo[m.Repo.Host+"/"+m.Repo.Project] = m.RunID
	}
	env := h.env(ctx, setID, st, nil, stage, selfAgent)
	env.LineCount = nil
	cache := map[string]int{}
	env.MemberLineCount = func(member, side, p string) (int, error) {
		m, ok := members[member]
		if !ok {
			return 0, fmt.Errorf("not a member of this change set")
		}
		clean := path.Clean(p)
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
			return 0, errors.New("invalid path")
		}
		key := member + "\x00" + side + "\x00" + clean
		if n, ok := cache[key]; ok {
			return n, nil
		}
		sha := m.HeadSHA
		if side == "old" {
			sha = m.MergeBase
		}
		b, err := h.git(m.Repo).Show(ctx, sha, clean)
		if err != nil {
			return 0, err
		}
		cache[key] = countLines(b)
		return cache[key], nil
	}
	env.AnchorOK = func(a string) bool {
		repo, unit, ok := changeset.Unqualify(a)
		if !ok {
			return false
		}
		run, ok := byRepo[repo]
		if !ok {
			return false
		}
		m, err := h.loadMap(run)
		if err != nil {
			return false
		}
		_, found := m.Unit(unit)
		return found
	}
	return env, nil
}
