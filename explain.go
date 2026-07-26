package figo

import (
	"fmt"
	"sort"
	"strings"
)

// explainMaxBytes bounds the size of Explain's output.
//
// An ASCII tree repeats the whole indentation on every line, so the rendering
// is O(nodes x depth) in OUTPUT SIZE: a ~27KB DSL of 2000 nested groups
// rendered 16MB before this cap, and nothing bounded it (LimitsPlugin's
// MaxNestingDepth caps what the parser accepts, not what Explain prints — and
// it is opt-in). Explain is a debugging aid that may be called on untrusted
// input, so the output is capped and the truncation is marked. The cap is
// checked per node, so the real bound is this plus one node's line.
const explainMaxBytes = 1 << 20

// Explain renders the parsed expression AST as a human-readable ASCII tree.
// It is a debugging aid for seeing exactly how a DSL string was interpreted —
// operator precedence, grouping, and negation are all made visible. Call it
// after Build() (or after adding filters); an unparsed instance yields
// "(no filters)".
//
// Preloads (load=[...]) are printed under a trailing PRELOADS section, sorted
// by relation name: their predicates reach the rendered statement, so leaving
// them out reported "(no filters)" for a query that was in fact filtered.
//
// Example:
//
//	f.AddFiltersFromString(`id=1 and (age>20 or active=true)`)
//	f.Build(nil)
//	fmt.Println(f.Explain())
//
//	AND
//	 ├── id = 1
//	 └── OR
//	     ├── age > 20
//	     └── active = true
func (f *figo) Explain() string {
	f.mu.RLock()
	clauses := f.clauses
	// The map itself must be copied under the lock (a concurrent write would
	// race a range); the slices behind it are only read.
	preloads := make(map[string][]Expr, len(f.preloads))
	for k, v := range f.preloads {
		preloads[k] = v
	}
	f.mu.RUnlock()

	if len(clauses) == 0 && len(preloads) == 0 {
		return "(no filters)"
	}

	w := &explainWriter{prefix: []byte(" ")}
	switch len(clauses) {
	case 0:
		w.writeString("(no filters)\n")
	case 1:
		w.writeExprTree(clauses[0])
	default:
		// Multiple top-level clauses are implicitly AND-ed by the query
		// builders; show them under a synthetic AND root to match that.
		w.writeExprTree(AndExpr{Operands: clauses})
	}
	w.writePreloads(preloads)
	return w.b.String()
}

// explainWriter accumulates the tree. prefix is the indentation for the CURRENT
// level, grown and truncated in place as the walk descends and returns — the
// previous version derived a fresh prefix string per child (childPrefix+cont),
// which allocated and copied O(depth) bytes for every node.
type explainWriter struct {
	b        strings.Builder
	prefix   []byte
	overflow bool
}

// room reports whether the output budget still has space, appending the
// truncation marker the first time it does not.
func (w *explainWriter) room() bool {
	if w.overflow {
		return false
	}
	if w.b.Len() >= explainMaxBytes {
		w.overflow = true
		w.b.WriteString("... (output truncated: expression tree too large)\n")
		return false
	}
	return true
}

func (w *explainWriter) writeString(s string) {
	if !w.overflow {
		w.b.WriteString(s)
	}
}

func (w *explainWriter) writeIndent() {
	if !w.overflow {
		w.b.Write(w.prefix)
	}
}

// branchOf returns the connector printed before a child and the continuation
// added to the indentation for that child's own children.
func branchOf(last bool) (string, string) {
	if last {
		return "└── ", "    "
	}
	return "├── ", "│   "
}

// writeExprTree prints e's label, then recurses into its children.
func (w *explainWriter) writeExprTree(e Expr) {
	if !w.room() {
		return
	}
	label, children := explainNode(e)
	w.writeString(label)
	w.writeString("\n")

	base := len(w.prefix)
	for i, child := range children {
		if !w.room() {
			break
		}
		branch, cont := branchOf(i == len(children)-1)
		w.writeIndent()
		w.writeString(branch)
		w.prefix = append(w.prefix, cont...)
		w.writeExprTree(child)
		w.prefix = w.prefix[:base]
	}
	w.prefix = w.prefix[:base]
}

// writePreloads appends one subtree per preloaded relation, sorted by relation
// name to match the deterministic preload ordering the adapters render.
func (w *explainWriter) writePreloads(preloads map[string][]Expr) {
	if len(preloads) == 0 {
		return
	}
	names := make([]string, 0, len(preloads))
	for k := range preloads {
		names = append(names, k)
	}
	sort.Strings(names)

	w.writeString("PRELOADS\n")
	base := len(w.prefix)
	for i, name := range names {
		if !w.room() {
			break
		}
		branch, cont := branchOf(i == len(names)-1)
		w.writeIndent()
		w.writeString(branch)
		w.writeString(name)
		w.writeString("\n")

		w.prefix = append(w.prefix, cont...)
		exprs := preloads[name]
		inner := len(w.prefix)
		for j, e := range exprs {
			if !w.room() {
				break
			}
			eb, ec := branchOf(j == len(exprs)-1)
			w.writeIndent()
			w.writeString(eb)
			w.prefix = append(w.prefix, ec...)
			w.writeExprTree(e)
			w.prefix = w.prefix[:inner]
		}
		w.prefix = w.prefix[:base]
	}
	w.prefix = w.prefix[:base]
}

// explainNode maps an Expr to its display label and, for logical operators, its
// child operands. Leaf (comparison) nodes return a nil child slice.
func explainNode(e Expr) (string, []Expr) {
	switch v := e.(type) {
	// Logical
	case AndExpr:
		return "AND", v.Operands
	case OrExpr:
		return "OR", v.Operands
	case NotExpr:
		return "NOT", v.Operands

	// Comparison
	case EqExpr:
		return fmt.Sprintf("%s = %s", v.Field, explainVal(v.Value)), nil
	case NeqExpr:
		return fmt.Sprintf("%s != %s", v.Field, explainVal(v.Value)), nil
	case GtExpr:
		return fmt.Sprintf("%s > %s", v.Field, explainVal(v.Value)), nil
	case GteExpr:
		return fmt.Sprintf("%s >= %s", v.Field, explainVal(v.Value)), nil
	case LtExpr:
		return fmt.Sprintf("%s < %s", v.Field, explainVal(v.Value)), nil
	case LteExpr:
		return fmt.Sprintf("%s <= %s", v.Field, explainVal(v.Value)), nil

	// Pattern / regex
	case LikeExpr:
		return fmt.Sprintf("%s LIKE %s", v.Field, explainVal(v.Value)), nil
	case ILikeExpr:
		return fmt.Sprintf("%s ILIKE %s", v.Field, explainVal(v.Value)), nil
	case RegexExpr:
		return fmt.Sprintf("%s =~ %s", v.Field, explainVal(v.Value)), nil

	// Set / range / null
	case InExpr:
		return fmt.Sprintf("%s IN %s", v.Field, explainList(v.Values)), nil
	case NotInExpr:
		return fmt.Sprintf("%s NOT IN %s", v.Field, explainList(v.Values)), nil
	case BetweenExpr:
		return fmt.Sprintf("%s BETWEEN %s AND %s", v.Field, explainVal(v.Low), explainVal(v.High)), nil
	case IsNullExpr:
		return fmt.Sprintf("%s IS NULL", v.Field), nil
	case NotNullExpr:
		return fmt.Sprintf("%s IS NOT NULL", v.Field), nil

	// Sort. OrderBy is an Expr too (clone and walk both handle it); without a
	// case here a supported shape printed as the bare Go type "figo.OrderBy".
	case OrderBy:
		return explainOrderBy(v), nil

	// Advanced. JsonPathExpr and CustomExpr carry a type marker: Field+Path
	// concatenated with no delimiter made {Field:"a",Path:"$.b"} and
	// {Field:"a$",Path:".b"} print identically although they render different
	// Mongo paths, and an unmarked CustomExpr printed as "a = 1" — the same
	// string as a working EqExpr, though no SQL adapter can render it and Mongo
	// refuses it. Explain's whole job is to say what a node actually IS.
	case JsonPathExpr:
		return fmt.Sprintf("%s JSON(%s) %s %s", v.Field, v.Path, v.Op, explainVal(v.Value)), nil
	case ArrayContainsExpr:
		return fmt.Sprintf("%s CONTAINS %s", v.Field, explainList(v.Values)), nil
	case ArrayOverlapsExpr:
		return fmt.Sprintf("%s OVERLAPS %s", v.Field, explainList(v.Values)), nil
	case FullTextSearchExpr:
		if v.Language != "" {
			return fmt.Sprintf("%s FULLTEXT %q (lang=%s)", v.Field, v.Query, v.Language), nil
		}
		return fmt.Sprintf("%s FULLTEXT %q", v.Field, v.Query), nil
	case GeoDistanceExpr:
		unit := v.Unit
		if unit == "" {
			unit = "km"
		}
		return fmt.Sprintf("%s GEO_DISTANCE(<= %g%s of %g,%g)", v.Field, v.Distance, unit, v.Latitude, v.Longitude), nil
	case CustomExpr:
		return fmt.Sprintf("%s CUSTOM(%s) %s", v.Field, v.Operator, explainVal(v.Value)), nil

	default:
		return fmt.Sprintf("%T", e), nil
	}
}

// explainOrderBy renders a sort spec as "ORDER BY id DESC, name ASC".
func explainOrderBy(o OrderBy) string {
	if len(o.Columns) == 0 {
		return "ORDER BY (none)"
	}
	parts := make([]string, len(o.Columns))
	for i, c := range o.Columns {
		dir := "ASC"
		if c.Desc {
			dir = "DESC"
		}
		parts[i] = c.Name + " " + dir
	}
	return "ORDER BY " + strings.Join(parts, ", ")
}

// explainVal renders a scalar value. Strings are quoted so they are visually
// distinct from numbers and booleans (e.g. name = "john" vs. active = true).
func explainVal(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// explainList renders a slice of values as [a, b, c].
func explainList(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = explainVal(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
