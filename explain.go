package figo

import (
	"fmt"
	"strings"
)

// Explain renders the parsed expression AST as a human-readable ASCII tree.
// It is a debugging aid for seeing exactly how a DSL string was interpreted —
// operator precedence, grouping, and negation are all made visible. Call it
// after Build() (or after adding filters); an unparsed instance yields
// "(no filters)".
//
// Example:
//
//	f.AddFiltersFromString(`id=1 and (age>20 or active=true)`)
//	f.Build()
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
	f.mu.RUnlock()

	var b strings.Builder
	switch len(clauses) {
	case 0:
		b.WriteString("(no filters)")
	case 1:
		writeExprTree(&b, clauses[0], " ")
	default:
		// Multiple top-level clauses are implicitly AND-ed by the query
		// builders; show them under a synthetic AND root to match that.
		writeExprTree(&b, AndExpr{Operands: clauses}, " ")
	}
	return b.String()
}

// writeExprTree prints e's label, then recurses into its children. childPrefix is
// the string prepended to every child line (before the branch connector); it
// grows by one connector-width per level so branches align under their parent.
func writeExprTree(b *strings.Builder, e Expr, childPrefix string) {
	label, children := explainNode(e)
	b.WriteString(label)
	b.WriteByte('\n')

	for i, child := range children {
		last := i == len(children)-1
		branch, cont := "├── ", "│   "
		if last {
			branch, cont = "└── ", "    "
		}
		b.WriteString(childPrefix)
		b.WriteString(branch)
		writeExprTree(b, child, childPrefix+cont)
	}
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

	// Advanced
	case JsonPathExpr:
		return fmt.Sprintf("%s%s %s %s", v.Field, v.Path, v.Op, explainVal(v.Value)), nil
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
		return fmt.Sprintf("%s %s %s", v.Field, v.Operator, explainVal(v.Value)), nil

	default:
		return fmt.Sprintf("%T", e), nil
	}
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
