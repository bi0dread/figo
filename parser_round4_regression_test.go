package figo

import (
	"strings"
	"testing"
)

// A "not" in front of a parenthesized group that yields no conditions must be
// dropped, not re-targeted onto the next predicate. The group can be empty
// ("()"), hold only directives (sort=/page=/load=, all valid DSL), or hold only
// invalid conditions — every form used to render NOT(<next predicate>),
// inverting a filter the caller never negated.
func TestNotBeforeEmptyGroupDoesNotInvertNextPredicate(t *testing.T) {
	cases := []struct {
		name string
		dsl  string
	}{
		{"empty group", `not () and a=1`},
		{"empty group, implicit and", `not () a=1`},
		{"group with only sort=", `not (sort=id:desc) and a=1`},
		{"group with only page=", `not (page=skip:0,take:5) and a=1`},
		{"group with only load=", `not (load=[Orders:id=1]) and a=1`},
		{"group with only an invalid condition", `not (x<bet>(broken)) and a=1`},
		{"group with a valueless operator", `not (b=) and a=1`},
		{"stacked nots", `not (not ()) and a=1`},
		{"or connector", `not () or a=1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			if err := f.AddFiltersFromString(tc.dsl); err != nil {
				t.Fatalf("AddFiltersFromString: %v", err)
			}
			f.Build(nil)

			clauses := f.GetClauses()
			if len(clauses) != 1 {
				t.Fatalf("want 1 clause, got %d: %#v", len(clauses), clauses)
			}
			eq, ok := clauses[0].(EqExpr)
			if !ok {
				t.Fatalf("want a bare EqExpr on the surviving predicate, got %#v", clauses[0])
			}
			if eq.Field != "a" || eq.Value != int64(1) {
				t.Fatalf("want a=1, got %#v", eq)
			}
		})
	}
}

// The NOT must still apply when the group DOES produce a condition.
func TestNotBeforeNonEmptyGroupStillNegates(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`not (b=2) and a=1`); err != nil {
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
	if len(and.Operands) != 2 {
		t.Fatalf("want 2 operands, got %d: %#v", len(and.Operands), and.Operands)
	}
	not, ok := and.Operands[0].(NotExpr)
	if !ok {
		t.Fatalf("want the first operand to stay a NotExpr, got %#v", and.Operands[0])
	}
	if inner, ok := not.Operands[0].(EqExpr); !ok || inner.Field != "b" {
		t.Fatalf("want NOT(b=2), got %#v", not.Operands[0])
	}
	if eq, ok := and.Operands[1].(EqExpr); !ok || eq.Field != "a" {
		t.Fatalf("want a=1 as the second operand, got %#v", and.Operands[1])
	}
}

// Dropping the not is reported: BuildE must not silently change the meaning.
func TestNotBeforeEmptyGroupIsDiagnosed(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString(`not () a=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	err := f.BuildE(nil)
	if err == nil {
		t.Fatal("want a diagnostic for the dropped 'not', got nil")
	}
	if !strings.Contains(err.Error(), "not") {
		t.Fatalf("diagnostic should mention the dropped not, got %q", err)
	}
}
