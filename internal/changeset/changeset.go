// SPDX-License-Identifier: MIT

// Package changeset models a cross-cutting change: several PRs, possibly in
// different repositories and on different forges, reviewed as one (SPEC
// §21, ADR 0014). It provides the set identity, the staleness key over all
// member heads, and a deterministic merge-order and compatibility analysis
// from cross-PR symbol edges. Nothing here is an LLM judgement.
package changeset

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

// MaxMembers caps a set: each member is a full review.
const MaxMembers = 8

// Member is one PR of the set.
type Member struct {
	RunID      string     `json:"run_id"`
	URL        string     `json:"url"`
	Ref        string     `json:"ref" jsonschema:"forge-native reference, e.g. o/r#12"`
	Repo       forge.Repo `json:"repo"`
	Number     int        `json:"number"`
	Title      string     `json:"title" jsonschema:"untrusted"`
	Author     string     `json:"author"`
	State      string     `json:"state" jsonschema:"open, merged or closed"`
	BaseBranch string     `json:"base_branch"`
	HeadBranch string     `json:"head_branch"`
	HeadSHA    string     `json:"head_sha"`
	MergeBase  string     `json:"merge_base"`
}

// Set is a change set.
type Set struct {
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
	Members []Member  `json:"members"`
	Key     string    `json:"key" jsonschema:"staleness key over every member's head; set-level analysis is valid only for this key"`
	Created time.Time `json:"created"`
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ID is the stable set id for a group of member runs (order-independent).
func ID(runIDs []string) string {
	ids := append([]string{}, runIDs...)
	sort.Strings(ids)
	return "changeset-" + hash(strings.Join(ids, "\x00"))[:12]
}

// Key is the staleness key: it changes when any member's head moves.
func Key(ms []Member) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.RunID+":"+m.HeadSHA)
	}
	sort.Strings(parts)
	return hash(strings.Join(parts, "\x00"))[:40]
}

// Qualify turns a member's unit id into a repo-qualified one:
// unit:<host>/<project>:<file>#<name>.
func Qualify(repo forge.Repo, unitID string) string {
	return "unit:" + repo.Host + "/" + repo.Project + ":" + strings.TrimPrefix(unitID, "unit:")
}

// Unqualify splits a repo-qualified unit id into host/project and the
// member-local unit id.
func Unqualify(q string) (repo, unitID string, ok bool) {
	rest, found := strings.CutPrefix(q, "unit:")
	if !found {
		return "", "", false
	}
	// host/project may contain ':'-free path segments; the unit part starts
	// after the first ':' that follows a '/'.
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", false
	}
	colon := strings.Index(rest[slash:], ":")
	if colon < 0 {
		return "", "", false
	}
	return rest[:slash+colon], "unit:" + rest[slash+colon+1:], true
}

// Use is one place a member's code references a symbol.
type Use struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text,omitempty" jsonschema:"untrusted source line"`
}

// Edge links a symbol changed in one member to its uses in another.
type Edge struct {
	From       string `json:"from" jsonschema:"provider member run id"`
	FromUnit   string `json:"from_unit" jsonschema:"repo-qualified unit id"`
	Symbol     string `json:"symbol"`
	Change     string `json:"change" jsonschema:"added, modified or deleted in the provider"`
	Interface  bool   `json:"interface,omitempty" jsonschema:"defined in an API/schema/proto/migration file"`
	To         string `json:"to" jsonschema:"consumer member run id"`
	UsedAtHead bool   `json:"used_at_head"`
	UsedAtBase bool   `json:"used_at_base" jsonschema:"the consumer's merge base already used the symbol"`
	Uses       []Use  `json:"uses" jsonschema:"consumer head uses (at most 5)"`
}

// Constraint is a required merge order.
type Constraint struct {
	Before string `json:"before"`
	After  string `json:"after"`
	Kind   string `json:"kind" jsonschema:"stacked, introduces or removal"`
	Reason string `json:"reason"`
}

// Contract is a modified symbol used across members: compatibility must be
// reviewed, but it does not by itself force an order.
type Contract struct {
	Provider string `json:"provider"`
	Consumer string `json:"consumer"`
	Symbol   string `json:"symbol"`
	Unit     string `json:"unit"`
}

// Problem is something the order cannot fix.
type Problem struct {
	Kind    string   `json:"kind" jsonschema:"incompatible, cycle or merged_out_of_order"`
	Members []string `json:"members"`
	Detail  string   `json:"detail"`
}

// Order is the merge-order and compatibility analysis.
type Order struct {
	Sequence    []string     `json:"sequence" jsonschema:"a merge order satisfying every constraint (merged members first); empty if there is a cycle"`
	Constraints []Constraint `json:"constraints"`
	Contracts   []Contract   `json:"contracts"`
	Problems    []Problem    `json:"problems"`
	Merged      []string     `json:"merged"`
}

func ref(ms map[string]Member, id string) string {
	if m, ok := ms[id]; ok && m.Ref != "" {
		return m.Ref
	}
	return id
}

// Analyze derives constraints from stacking and symbol edges, orders the
// members, and reports what no order can fix.
func Analyze(members []Member, edges []Edge) Order {
	o := Order{Sequence: []string{}, Constraints: []Constraint{}, Contracts: []Contract{}, Problems: []Problem{}, Merged: []string{}}
	byID := map[string]Member{}
	for _, m := range members {
		byID[m.RunID] = m
		if m.State == "merged" {
			o.Merged = append(o.Merged, m.RunID)
		}
	}
	seen := map[string]int{} // constraint key -> index
	syms := map[string][]string{}
	add := func(c Constraint, symbol string) {
		k := c.Before + ">" + c.After + ">" + c.Kind
		if c.Before == c.After {
			return
		}
		if i, ok := seen[k]; ok {
			// Same pair and kind: one constraint naming every symbol.
			if symbol != "" {
				syms[k] = append(syms[k], symbol)
				o.Constraints[i].Reason = strings.Replace(c.Reason, symbol, strings.Join(syms[k], ", "), 1)
			}
			return
		}
		seen[k] = len(o.Constraints)
		if symbol != "" {
			syms[k] = []string{symbol}
		}
		o.Constraints = append(o.Constraints, c)
	}
	for _, a := range members {
		for _, b := range members {
			if a.RunID != b.RunID && a.Repo == b.Repo && b.BaseBranch != "" && b.BaseBranch == a.HeadBranch {
				add(Constraint{Before: a.RunID, After: b.RunID, Kind: "stacked",
					Reason: fmt.Sprintf("%s targets %s's branch %q", b.Ref, a.Ref, a.HeadBranch)}, "")
			}
		}
	}
	for _, e := range edges {
		switch e.Change {
		case "added":
			if e.UsedAtHead && !e.UsedAtBase {
				add(Constraint{Before: e.From, After: e.To, Kind: "introduces",
					Reason: fmt.Sprintf("%s uses %s, which %s introduces", ref(byID, e.To), e.Symbol, ref(byID, e.From))}, e.Symbol)
			}
		case "deleted":
			if e.UsedAtHead {
				o.Problems = append(o.Problems, Problem{Kind: "incompatible", Members: []string{e.From, e.To},
					Detail: fmt.Sprintf("%s still uses %s, which %s removes: it breaks once %s merges", ref(byID, e.To), e.Symbol, ref(byID, e.From), ref(byID, e.From))})
			} else if e.UsedAtBase {
				add(Constraint{Before: e.To, After: e.From, Kind: "removal",
					Reason: fmt.Sprintf("%s stops using %s, which %s removes", ref(byID, e.To), e.Symbol, ref(byID, e.From))}, e.Symbol)
			}
		case "modified":
			if e.UsedAtHead || e.UsedAtBase {
				o.Contracts = append(o.Contracts, Contract{Provider: e.From, Consumer: e.To, Symbol: e.Symbol, Unit: e.FromUnit})
			}
		}
	}
	for _, c := range o.Constraints {
		if byID[c.After].State == "merged" && byID[c.Before].State != "merged" {
			o.Problems = append(o.Problems, Problem{Kind: "merged_out_of_order", Members: []string{c.Before, c.After},
				Detail: fmt.Sprintf("%s is merged but needs %s first (%s)", ref(byID, c.After), ref(byID, c.Before), c.Reason)})
		}
	}
	// Kahn's algorithm, stable in member order; merged members first.
	indeg := map[string]int{}
	next := map[string][]string{}
	for _, c := range o.Constraints {
		indeg[c.After]++
		next[c.Before] = append(next[c.Before], c.After)
	}
	var queue, seq []string
	ordered := append([]Member{}, members...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].State == "merged" && ordered[j].State != "merged" })
	for _, m := range ordered {
		if indeg[m.RunID] == 0 {
			queue = append(queue, m.RunID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seq = append(seq, id)
		for _, n := range next[id] {
			indeg[n]--
			if indeg[n] == 0 {
				queue = append(queue, n)
			}
		}
	}
	if len(seq) < len(members) {
		left := map[string]bool{}
		for _, m := range members {
			if indeg[m.RunID] > 0 {
				left[m.RunID] = true
			}
		}
		// Members on a cycle reach themselves; the rest are only blocked
		// behind one.
		onCycle := func(start string) bool {
			stack, visited := append([]string{}, next[start]...), map[string]bool{}
			for len(stack) > 0 {
				n := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if n == start {
					return true
				}
				if visited[n] || !left[n] {
					continue
				}
				visited[n] = true
				stack = append(stack, next[n]...)
			}
			return false
		}
		var cyc, blocked []string
		for _, m := range members {
			if !left[m.RunID] {
				continue
			}
			if onCycle(m.RunID) {
				cyc = append(cyc, m.RunID)
			} else {
				blocked = append(blocked, ref(byID, m.RunID))
			}
		}
		refs := make([]string, len(cyc))
		for i, c := range cyc {
			refs[i] = ref(byID, c)
		}
		detail := "no merge order works for " + strings.Join(refs, ", ") + ": merge them together, or add a compatibility shim so one can land first"
		if len(blocked) > 0 {
			detail += "; blocked behind them: " + strings.Join(blocked, ", ")
		}
		o.Problems = append(o.Problems, Problem{Kind: "cycle", Members: cyc, Detail: detail})
		return o
	}
	o.Sequence = seq
	return o
}
