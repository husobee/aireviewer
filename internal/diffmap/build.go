// SPDX-License-Identifier: MIT

// Package diffmap builds the semantic diff-map (SPEC.md §7): the PR diff
// reorganised around logical units (functions, methods, classes, config
// files) with stable ids that every agent report anchors to.
package diffmap

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// Unit kinds.
const (
	KindFunction = "function"
	KindMethod   = "method"
	KindClass    = "class"
	KindModule   = "module" // top-level code outside any definition
	KindConfig   = "config"
	KindDoc      = "doc"
	KindHunk     = "hunk" // unsupported language: one unit per hunk
	KindBinary   = "binary"
	KindFile     = "file" // rename/mode change with no content hunks
)

// File / unit change statuses.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
	StatusRenamed  = "renamed"
)

// TopLevel is the pseudo-definition name for code outside any definition.
const TopLevel = "(top-level)"

// MaxParseBytes skips tree-sitter for very large files (they fall back to hunk units).
const MaxParseBytes = 1 << 20

// Unit is one logical unit of change.
type Unit struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	File      string `json:"file"`
	OldFile   string `json:"old_file,omitempty"`
	Language  string `json:"language,omitempty"`
	Change    string `json:"change"`
	OldRange  []int  `json:"old_range,omitempty" jsonschema:"[start,end] 1-based lines of the unit at the merge base"`
	NewRange  []int  `json:"new_range,omitempty" jsonschema:"[start,end] 1-based lines of the unit at head"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsTest    bool   `json:"is_test,omitempty"`
	Lines     []Line `json:"lines,omitempty"`
}

// FileSummary lists a changed file and the units it contains.
type FileSummary struct {
	Path      string   `json:"path"`
	OldPath   string   `json:"old_path,omitempty"`
	Status    string   `json:"status"`
	Language  string   `json:"language,omitempty"`
	Additions int      `json:"additions"`
	Deletions int      `json:"deletions"`
	Units     []string `json:"units"`
}

// Map is the semantic diff-map of a PR.
type Map struct {
	Version   int           `json:"version"`
	HeadSHA   string        `json:"head_sha"`
	MergeBase string        `json:"merge_base"`
	Files     []FileSummary `json:"files"`
	Units     []Unit        `json:"units"`
	Order     []string      `json:"order" jsonschema:"suggested review order (unit ids)"`
}

// Unit returns the unit with the given id.
func (m *Map) Unit(id string) (*Unit, bool) {
	for i := range m.Units {
		if m.Units[i].ID == id {
			return &m.Units[i], true
		}
	}
	return nil, false
}

// HasFile reports whether path is a changed file (either side).
func (m *Map) HasFile(path string) bool {
	for _, f := range m.Files {
		if f.Path == path || f.OldPath == path {
			return true
		}
	}
	return false
}

// Source supplies file contents on each side of the diff.
type Source interface {
	// Head returns the file content at the PR head.
	Head(ctx context.Context, path string) ([]byte, error)
	// Base returns the file content at the merge base.
	Base(ctx context.Context, path string) ([]byte, error)
}

// Build constructs the diff-map from parsed git diff output.
func Build(ctx context.Context, files []*FileDiff, src Source, headSHA, mergeBase string) (*Map, error) {
	m := &Map{Version: 1, HeadSHA: headSHA, MergeBase: mergeBase, Files: []FileSummary{}, Units: []Unit{}}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		units, lang := buildFile(ctx, f, src)
		fs := FileSummary{Path: f.Path(), Status: f.Status, Language: lang, Units: []string{}}
		if f.Status == StatusRenamed {
			fs.OldPath = f.OldPath
		}
		for _, u := range units {
			fs.Additions += u.Additions
			fs.Deletions += u.Deletions
			fs.Units = append(fs.Units, u.ID)
		}
		m.Files = append(m.Files, fs)
		m.Units = append(m.Units, units...)
	}
	m.Order = order(m.Units)
	return m, nil
}

func countOps(lines []Line) (add, del int) {
	for _, l := range lines {
		switch l.Op {
		case "+":
			add++
		case "-":
			del++
		}
	}
	return add, del
}

func allLines(f *FileDiff) []Line {
	var out []Line
	for _, h := range f.Hunks {
		out = append(out, h.Lines...)
	}
	return out
}

func newUnit(f *FileDiff, kind, id string, lines []Line) Unit {
	u := Unit{ID: id, Kind: kind, File: f.Path(), Change: f.Status, IsTest: IsTestPath(f.Path()), Lines: lines}
	if f.Status == StatusRenamed {
		u.OldFile = f.OldPath
	}
	u.Additions, u.Deletions = countOps(lines)
	return u
}

func buildFile(ctx context.Context, f *FileDiff, src Source) ([]Unit, string) {
	p := f.Path()
	lang, fileKind := classify(p)
	switch {
	case f.Binary:
		return []Unit{newUnit(f, KindBinary, "unit:"+p, nil)}, lang
	case len(f.Hunks) == 0:
		return []Unit{newUnit(f, KindFile, "unit:"+p, nil)}, lang
	case fileKind != "":
		return []Unit{newUnit(f, fileKind, "unit:"+p, allLines(f))}, lang
	case lang != "":
		if units, ok := semanticUnits(ctx, f, src, lang); ok {
			return units, lang
		}
	}
	return hunkUnits(f), lang
}

func hunkUnits(f *FileDiff) []Unit {
	units := make([]Unit, 0, len(f.Hunks))
	for _, h := range f.Hunks {
		start := h.NewStart
		if f.Status == StatusDeleted {
			start = h.OldStart
		}
		u := newUnit(f, KindHunk, "unit:"+f.Path()+"@L"+strconv.Itoa(start), h.Lines)
		u.Name = h.Header
		units = append(units, u)
	}
	return units
}

func semanticUnits(ctx context.Context, f *FileDiff, src Source, lang string) ([]Unit, bool) {
	var newDefs, oldDefs []def
	if f.Status != StatusDeleted {
		b, err := src.Head(ctx, f.NewPath)
		if err != nil || len(b) > MaxParseBytes {
			return nil, false
		}
		var ok bool
		if newDefs, ok = extractDefs(lang, b); !ok {
			return nil, false
		}
	}
	if f.Status != StatusAdded {
		b, err := src.Base(ctx, f.OldPath)
		if err != nil || len(b) > MaxParseBytes {
			return nil, false
		}
		var ok bool
		if oldDefs, ok = extractDefs(lang, b); !ok {
			return nil, false
		}
	}
	byName := func(defs []def) map[string]*def {
		m := map[string]*def{}
		for i := range defs {
			if _, dup := m[defs[i].name]; !dup {
				m[defs[i].name] = &defs[i]
			}
		}
		return m
	}
	newBy, oldBy := byName(newDefs), byName(oldDefs)

	keyFor := func(l Line) string {
		var d *def
		switch {
		case l.Op == "-":
			d = innermost(oldDefs, l.Old)
		case l.New > 0 && f.Status != StatusDeleted:
			d = innermost(newDefs, l.New)
		default:
			d = innermost(oldDefs, l.Old)
		}
		if d == nil {
			return TopLevel
		}
		return d.name
	}

	type acc struct{ lines []Line }
	groups := map[string]*acc{}
	var keys []string
	changed := map[string]bool{}
	for _, l := range allLines(f) {
		k := keyFor(l)
		if groups[k] == nil {
			groups[k] = &acc{}
			keys = append(keys, k)
		}
		groups[k].lines = append(groups[k].lines, l)
		if l.Op != " " {
			changed[k] = true
		}
	}
	var units []Unit
	for _, k := range keys {
		if !changed[k] { // context-only group
			continue
		}
		kind := KindModule
		nd, od := newBy[k], oldBy[k]
		if nd != nil {
			kind = nd.kind
		} else if od != nil {
			kind = od.kind
		}
		u := newUnit(f, kind, "unit:"+f.Path()+"#"+k, groups[k].lines)
		u.Name, u.Language = k, lang
		if nd != nil {
			u.NewRange = []int{nd.start, nd.end}
		}
		if od != nil {
			u.OldRange = []int{od.start, od.end}
		}
		switch {
		case f.Status == StatusAdded || f.Status == StatusDeleted:
			// keep file status
		case k == TopLevel:
			u.Change = StatusModified
		case nd != nil && od == nil:
			u.Change = StatusAdded
		case nd == nil && od != nil:
			u.Change = StatusDeleted
		default:
			u.Change = StatusModified
		}
		units = append(units, u)
	}
	if len(units) == 0 {
		return nil, false
	}
	return units, true
}

func rank(u Unit) int {
	switch {
	case u.Kind == KindBinary || u.Kind == KindFile:
		return 4
	case u.Kind == KindDoc:
		return 3
	case u.Kind == KindConfig:
		return 2
	case u.IsTest:
		return 1
	}
	return 0
}

// order returns unit ids in suggested review order: production code first,
// then tests, config, docs and binaries; larger changes first within a group.
// (M2 re-orders by risk once augmentation reports exist.)
func order(units []Unit) []string {
	idx := make([]int, len(units))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ua, ub := units[idx[a]], units[idx[b]]
		if ra, rb := rank(ua), rank(ub); ra != rb {
			return ra < rb
		}
		if ca, cb := ua.Additions+ua.Deletions, ub.Additions+ub.Deletions; ca != cb {
			return ca > cb
		}
		return ua.ID < ub.ID
	})
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = units[j].ID
	}
	return out
}

// Summary returns a copy of the map without per-line hunk data (for compact
// tool responses).
func (m *Map) Summary() *Map {
	c := *m
	c.Units = make([]Unit, len(m.Units))
	for i, u := range m.Units {
		u.Lines = nil
		c.Units[i] = u
	}
	return &c
}

// Render formats a unit's lines as numbered diff text for humans and agents.
func (u *Unit) Render() string {
	out := fmt.Sprintf("%s  [%s, %s, +%d −%d]\n", u.ID, u.Kind, u.Change, u.Additions, u.Deletions)
	for _, l := range u.Lines {
		num := ""
		switch l.Op {
		case "-":
			num = fmt.Sprintf("%5d      ", l.Old)
		case "+":
			num = fmt.Sprintf("      %5d", l.New)
		default:
			num = fmt.Sprintf("%5d %5d", l.Old, l.New)
		}
		out += num + " " + l.Op + " " + l.Text + "\n"
	}
	return out
}
