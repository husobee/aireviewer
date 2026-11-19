// SPDX-License-Identifier: MIT

package diffmap

import (
	"path"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// langSpec describes how to find definitions in one language.
type langSpec struct {
	name string
	lang func() *ts.Language
	// defs maps a tree-sitter node kind to the unit kind it produces.
	defs map[string]string
	// classLike node kinds make nested functions "method"s.
	classLike map[string]bool
}

func set(kinds ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

var jsDefs = map[string]string{
	"function_declaration":           KindFunction,
	"generator_function_declaration": KindFunction,
	"class_declaration":              KindClass,
	"abstract_class_declaration":     KindClass,
	"interface_declaration":          KindClass,
	"type_alias_declaration":         KindClass,
	"enum_declaration":               KindClass,
	"method_definition":              KindMethod,
	"method_signature":               KindMethod,
	"abstract_method_signature":      KindMethod,
	"variable_declarator":            KindFunction, // only when bound to a function; see defName
}

var langs = map[string]*langSpec{
	"go": {name: "go", lang: lazy(tsgo.Language),
		defs:      map[string]string{"function_declaration": KindFunction, "method_declaration": KindMethod, "type_spec": KindClass},
		classLike: set("type_spec")},
	"python": {name: "python", lang: lazy(tspy.Language),
		defs:      map[string]string{"function_definition": KindFunction, "class_definition": KindClass},
		classLike: set("class_definition")},
	"javascript": {name: "javascript", lang: lazy(tsjs.Language), defs: jsDefs,
		classLike: set("class_declaration")},
	"typescript": {name: "typescript", lang: lazy(tsts.LanguageTypescript), defs: jsDefs,
		classLike: set("class_declaration", "abstract_class_declaration", "interface_declaration")},
	"tsx": {name: "tsx", lang: lazy(tsts.LanguageTSX), defs: jsDefs,
		classLike: set("class_declaration", "abstract_class_declaration", "interface_declaration")},
	"java": {name: "java", lang: lazy(tsjava.Language),
		defs: map[string]string{"class_declaration": KindClass, "interface_declaration": KindClass, "enum_declaration": KindClass,
			"record_declaration": KindClass, "method_declaration": KindMethod, "constructor_declaration": KindMethod},
		classLike: set("class_declaration", "interface_declaration", "enum_declaration", "record_declaration")},
	"rust": {name: "rust", lang: lazy(tsrust.Language),
		defs: map[string]string{"function_item": KindFunction, "impl_item": KindClass, "struct_item": KindClass,
			"enum_item": KindClass, "trait_item": KindClass, "mod_item": KindClass},
		classLike: set("impl_item", "trait_item")},
	"ruby": {name: "ruby", lang: lazy(tsruby.Language),
		defs:      map[string]string{"method": KindFunction, "singleton_method": KindFunction, "class": KindClass, "module": KindClass},
		classLike: set("class", "module")},
}

type unsafePointer = unsafe.Pointer

func lazy(f func() unsafePointer) func() *ts.Language {
	var once sync.Once
	var l *ts.Language
	return func() *ts.Language {
		once.Do(func() { l = ts.NewLanguage(f()) })
		return l
	}
}

var extLang = map[string]string{
	".go": "go", ".py": "python", ".pyi": "python",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".mts": "typescript", ".cts": "typescript", ".tsx": "tsx",
	".java": "java", ".rs": "rust", ".rb": "ruby",
}

var configExt = set(".yaml", ".yml", ".json", ".toml", ".ini", ".cfg", ".conf", ".env", ".properties",
	".xml", ".tf", ".tfvars", ".hcl", ".proto", ".graphql", ".sql", ".lock", ".mod", ".sum")
var configNames = set("dockerfile", "makefile", "containerfile", "jenkinsfile", "go.mod", "go.sum",
	"package.json", "cargo.toml", "gemfile", ".gitignore", ".dockerignore", ".editorconfig")
var docExt = set(".md", ".markdown", ".rst", ".txt", ".adoc")

// classify returns (language, unit kind for whole-file units).
func classify(p string) (lang string, fileKind string) {
	base := strings.ToLower(path.Base(p))
	ext := strings.ToLower(path.Ext(p))
	if l, ok := extLang[ext]; ok {
		return l, ""
	}
	switch {
	case configNames[base] || configExt[ext] || strings.HasPrefix(base, "dockerfile"):
		return "", KindConfig
	case docExt[ext]:
		return "", KindDoc
	}
	return "", ""
}

var testPath = regexp.MustCompile(`(^|/)(tests?|__tests__|spec|testdata)/|_test\.go$|(^|/)test_[^/]*\.py$|_test\.py$|\.(test|spec)\.[jt]sx?$|Tests?\.java$|_spec\.rb$|_test\.rb$`)

// IsTestPath reports whether a path looks like test code.
func IsTestPath(p string) bool { return testPath.MatchString(p) }

// def is one definition found in a source file. Rows are 1-based, inclusive.
type def struct {
	kind  string
	name  string // qualified, e.g. Client.Do
	start int
	end   int
}

var genericSuffix = regexp.MustCompile(`[\[<].*$`)

func cleanName(s string) string {
	s = strings.TrimSpace(genericSuffix.ReplaceAllString(s, ""))
	return strings.Join(strings.Fields(s), " ")
}

// defName returns the declared name for a definition node, or "" when the
// node should not be treated as a definition.
func defName(spec *langSpec, n *ts.Node, src []byte) string {
	switch n.Kind() {
	case "variable_declarator":
		v := n.ChildByFieldName("value")
		if v == nil {
			return ""
		}
		switch v.Kind() {
		case "arrow_function", "function_expression", "function", "generator_function":
		default:
			return ""
		}
	case "method_declaration":
		if spec.name == "go" {
			name := text(n.ChildByFieldName("name"), src)
			if recv := firstOfKind(n.ChildByFieldName("receiver"), "type_identifier"); recv != nil {
				return cleanName(recv.Utf8Text(src)) + "." + name
			}
			return name
		}
	case "impl_item":
		t := text(n.ChildByFieldName("type"), src)
		if tr := text(n.ChildByFieldName("trait"), src); tr != "" {
			return cleanName(t) + " (" + cleanName(tr) + ")"
		}
		return cleanName(t)
	}
	return cleanName(text(n.ChildByFieldName("name"), src))
}

func text(n *ts.Node, src []byte) string {
	if n == nil {
		return ""
	}
	return n.Utf8Text(src)
}

func firstOfKind(n *ts.Node, kind string) *ts.Node {
	if n == nil {
		return nil
	}
	if n.Kind() == kind {
		return n
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if f := firstOfKind(n.NamedChild(i), kind); f != nil {
			return f
		}
	}
	return nil
}

// extractDefs parses src and returns every definition with its qualified name.
func extractDefs(lang string, src []byte) ([]def, bool) {
	spec, ok := langs[lang]
	if !ok {
		return nil, false
	}
	p := ts.NewParser()
	defer p.Close()
	if err := p.SetLanguage(spec.lang()); err != nil {
		return nil, false
	}
	tree := p.Parse(src, nil)
	if tree == nil {
		return nil, false
	}
	defer tree.Close()
	var out []def
	var walk func(n *ts.Node, scope []string, inClass bool)
	walk = func(n *ts.Node, scope []string, inClass bool) {
		kind, isDef := spec.defs[n.Kind()]
		if isDef {
			if name := defName(spec, n, src); name != "" {
				// Go methods carry their receiver in the name already.
				q := name
				if len(scope) > 0 && !(spec.name == "go" && n.Kind() == "method_declaration") {
					q = strings.Join(scope, ".") + "." + name
				}
				if kind == KindFunction && inClass {
					kind = KindMethod
				}
				out = append(out, def{kind: kind, name: q,
					start: int(n.StartPosition().Row) + 1, end: int(n.EndPosition().Row) + 1})
				child := append(append([]string{}, scope...), name)
				cls := spec.classLike[n.Kind()]
				for i := uint(0); i < n.NamedChildCount(); i++ {
					walk(n.NamedChild(i), child, cls)
				}
				return
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i), scope, inClass)
		}
	}
	walk(tree.RootNode(), nil, false)
	return out, true
}

// innermost returns the smallest def containing row, or nil.
func innermost(defs []def, row int) *def {
	var best *def
	for i := range defs {
		d := &defs[i]
		if row >= d.start && row <= d.end && (best == nil || d.end-d.start < best.end-best.start) {
			best = d
		}
	}
	return best
}
