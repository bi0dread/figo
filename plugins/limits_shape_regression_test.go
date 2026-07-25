package plugins

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
)

// MaxExpressionCount must bound the QUERY, not the parser's internal tree
// shape. The parser collapses a run of one connector into a single n-ary node
// instead of a chain of binary ones; counting each logical node as 1 made the
// same query measure roughly half as much, so a configured limit silently
// admitted about twice the input it used to.
func TestExpressionCountIsShapeIndependent(t *testing.T) {
	// Param-free terms so MaxExpressionCount is the binding limit.
	chain := func(n int) string {
		terms := make([]string, n)
		for i := range terms {
			terms[i] = "a<null>"
		}
		return strings.Join(terms, " and ")
	}

	accepts := func(n int) bool {
		f := figo.New()
		if err := f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxExpressionCount: 200})); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		return f.AddFiltersFromString(chain(n)) == nil
	}

	// 100 terms => 100 leaves + 99 connectors = 199 <= 200.
	if !accepts(100) {
		t.Fatal("100 param-free conjuncts should fit within MaxExpressionCount=200")
	}
	// 101 terms => 101 + 100 = 201 > 200.
	if accepts(101) {
		t.Fatal("101 param-free conjuncts should exceed MaxExpressionCount=200")
	}
}

// The same count must come out whether the tree was built n-ary (as the parser
// now does) or as nested binary nodes (as AddFilter callers may still supply).
func TestExpressionCountMatchesAcrossTreeShapes(t *testing.T) {
	leaf := func() figo.Expr { return figo.IsNullExpr{Field: "f"} }

	flat := figo.AndExpr{Operands: []figo.Expr{leaf(), leaf(), leaf(), leaf()}}
	nested := figo.AndExpr{Operands: []figo.Expr{
		figo.AndExpr{Operands: []figo.Expr{
			figo.AndExpr{Operands: []figo.Expr{leaf(), leaf()}},
			leaf(),
		}},
		leaf(),
	}}

	measure := func(e figo.Expr) int {
		m := &queryMeasure{fields: make(map[string]bool)}
		measureExpr(e, m)
		return m.expressions
	}

	if got, want := measure(flat), measure(nested); got != want {
		t.Fatalf("flat tree measured %d, equivalent nested tree measured %d", got, want)
	}
	// 4 leaves joined by 3 connectors.
	if got := measure(flat); got != 7 {
		t.Fatalf("want 7 nodes for a 4-term conjunction, got %d", got)
	}
}

// Depth is unchanged: a flat chain is still one logical level.
func TestFlatChainStaysDepthOne(t *testing.T) {
	flat := figo.AndExpr{Operands: []figo.Expr{
		figo.IsNullExpr{Field: "a"},
		figo.IsNullExpr{Field: "b"},
		figo.IsNullExpr{Field: "c"},
	}}
	if got := logicalDepth(flat); got != 1 {
		t.Fatalf("want depth 1 for a flat chain, got %d", got)
	}
}
