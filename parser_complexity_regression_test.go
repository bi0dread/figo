package figo

import (
	"strings"
	"testing"
	"time"
)

// A run of one connector collapses into a single n-ary node instead of a
// left-nested binary chain. Both shapes mean the same thing (and/or are
// associative), but the nested one was N levels deep for N conjuncts, which
// made every adapter's render quadratic.
func TestConnectorRunsCollapseFlat(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 and b=2 and c=3 and d=4`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	clauses := f.GetClauses()
	if len(clauses) != 1 {
		t.Fatalf("want 1 clause, got %d", len(clauses))
	}
	and, ok := clauses[0].(AndExpr)
	if !ok {
		t.Fatalf("want AndExpr, got %#v", clauses[0])
	}
	if len(and.Operands) != 4 {
		t.Fatalf("want 4 flat operands, got %d: %#v", len(and.Operands), and.Operands)
	}
	for i, op := range and.Operands {
		if _, ok := op.(EqExpr); !ok {
			t.Fatalf("operand %d should be a leaf EqExpr, got %#v", i, op)
		}
	}
}

// Precedence is unchanged by the flattening: and binds tighter than or, and a
// run only absorbs its OWN connector.
func TestFlatteningPreservesPrecedence(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 and b=2 or c=3 and d=4`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	or, ok := f.GetClauses()[0].(OrExpr)
	if !ok {
		t.Fatalf("want a top-level OrExpr, got %#v", f.GetClauses()[0])
	}
	if len(or.Operands) != 2 {
		t.Fatalf("want 2 or-operands, got %d", len(or.Operands))
	}
	for i, op := range or.Operands {
		and, ok := op.(AndExpr)
		if !ok {
			t.Fatalf("or-operand %d should be an AndExpr, got %#v", i, op)
		}
		if len(and.Operands) != 2 {
			t.Fatalf("or-operand %d should hold 2 conjuncts, got %d", i, len(and.Operands))
		}
	}
}

// Mixed runs: "a and b and c or d" is (a AND b AND c) OR d.
func TestFlatteningMixedRun(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`a=1 and b=2 and c=3 or d=4`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(nil)

	or, ok := f.GetClauses()[0].(OrExpr)
	if !ok {
		t.Fatalf("want OrExpr, got %#v", f.GetClauses()[0])
	}
	and, ok := or.Operands[0].(AndExpr)
	if !ok || len(and.Operands) != 3 {
		t.Fatalf("want a 3-operand AndExpr as the first disjunct, got %#v", or.Operands[0])
	}
}

// Group nesting must parse in linear time. The quadratic form took ~1s at
// depth 20 000 and did not finish at 100 000; the bound here is deliberately
// loose so it fails only on a real complexity regression, not on a slow box.
func TestDeepGroupNestingParsesQuickly(t *testing.T) {
	const depth = 50000
	dsl := strings.Repeat("(", depth) + "a=1" + strings.Repeat(")", depth)

	f := New()
	if err := f.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}

	start := time.Now()
	f.Build(nil)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("parsing %d nested groups took %v; the quadratic parse is back", depth, elapsed)
	}
	if eq, ok := f.GetClauses()[0].(EqExpr); !ok || eq.Field != "a" {
		t.Fatalf("want the inner a=1 to survive, got %#v", f.GetClauses()[0])
	}
}

// A long single-connector chain must build in linear time too.
func TestLongConnectorChainBuildsQuickly(t *testing.T) {
	const n = 20000
	dsl := strings.Repeat("a=1 and ", n) + "b=2"

	f := New()
	if err := f.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}

	start := time.Now()
	f.Build(nil)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("building a %d-term chain took %v", n, elapsed)
	}
	and, ok := f.GetClauses()[0].(AndExpr)
	if !ok {
		t.Fatalf("want AndExpr, got %#v", f.GetClauses()[0])
	}
	if len(and.Operands) != n+1 {
		t.Fatalf("want %d flat operands, got %d", n+1, len(and.Operands))
	}
}
