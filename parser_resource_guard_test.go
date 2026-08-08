package figo

import (
	"strings"
	"testing"
	"time"
)

// An input over the byte cap is refused UNSCANNED and fails closed: the clause
// list holds the never-true pill (renders 1=0 on every adapter) and BuildE
// reports why. Refusing by size must not depend on surviving the scan.
func TestOversizedDSLFailsClosedUnparsed(t *testing.T) {
	f := New()
	if err := f.AddFiltersFromString("a=1 and " + strings.Repeat("b", maxDSLBytes)); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	err := f.BuildE(nil)
	if err == nil || !strings.Contains(err.Error(), "parser cap") {
		t.Fatalf("want the byte-cap diagnostic, got %v", err)
	}
	assertResourceGuardPill(t, f)
}

// Group nesting past the depth cap is refused the same way. The cap sits at
// double the depth the complexity suite pins as supported (50k in
// TestDeepGroupNestingParsesQuickly), so it can only trip on a bomb.
func TestOverdeepGroupNestingFailsClosedUnparsed(t *testing.T) {
	depth := maxDSLGroupDepth + 1
	f := New()
	if err := f.AddFiltersFromString(strings.Repeat("(", depth) + "a=1" + strings.Repeat(")", depth)); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	err := f.BuildE(nil)
	if err == nil || !strings.Contains(err.Error(), "parser cap") {
		t.Fatalf("want the depth-cap diagnostic, got %v", err)
	}
	assertResourceGuardPill(t, f)
}

// The depth scan is quote-aware under the tokenizer's rule: parens inside a
// string literal are data, not nesting, and must not trip the cap.
func TestQuotedParensDoNotCountTowardDepthCap(t *testing.T) {
	val := strings.Repeat("(", maxDSLGroupDepth+1)
	f := New()
	if err := f.AddFiltersFromString(`a="` + val + `"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	if err := f.BuildE(nil); err != nil {
		t.Fatalf("a quoted paren run must parse, got %v", err)
	}
	eq, ok := f.GetClauses()[0].(EqExpr)
	if !ok || eq.Value != val {
		t.Fatalf("want the quoted value to survive verbatim, got %#v", f.GetClauses()[0])
	}
}

// pruneEverythingFilter is the adversarial ExprFilter for the pill-survival
// test below: it prunes every field, so any expression it sees becomes nil.
type pruneEverythingFilter struct{}

func (pruneEverythingFilter) Name() string                                   { return "prune-everything" }
func (pruneEverythingFilter) Version() string                                { return "0" }
func (pruneEverythingFilter) Initialize(Figo) error                          { return nil }
func (pruneEverythingFilter) BeforeQuery(Figo, any) error                    { return nil }
func (pruneEverythingFilter) AfterQuery(Figo, any, any) error                { return nil }
func (pruneEverythingFilter) BeforeParse(_ Figo, dsl string) (string, error) { return dsl, nil }
func (pruneEverythingFilter) AfterParse(Figo, string) error                  { return nil }
func (pruneEverythingFilter) FilterExpr(_ Figo, e Expr) Expr {
	return PruneExprFields(e, func(string) bool { return false })
}

// A registered ExprFilter must not launder the refusal away: pruning rebuilds
// logical nodes and drops an empty OrExpr entirely (pruneExprFields), which
// would turn the guard's pill into a filter-less match-everything query. Build
// therefore skips the filter pass when the guard tripped — this pins that.
func TestResourceGuardPillSurvivesExprFilters(t *testing.T) {
	depth := maxDSLGroupDepth + 1
	f := New()
	if err := f.RegisterPlugin(pruneEverythingFilter{}); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(strings.Repeat("(", depth) + "a=1" + strings.Repeat(")", depth)); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	if err := f.BuildE(nil); err == nil {
		t.Fatal("want the depth-cap diagnostic, got nil")
	}
	assertResourceGuardPill(t, f)
}

func assertResourceGuardPill(t *testing.T, f Figo) {
	t.Helper()
	clauses := f.GetClauses()
	if len(clauses) != 1 {
		t.Fatalf("want exactly the never-true pill, got %d clauses: %#v", len(clauses), clauses)
	}
	or, ok := clauses[0].(OrExpr)
	if !ok || len(or.Operands) != 0 {
		t.Fatalf("want the empty-OrExpr pill, got %#v", clauses[0])
	}
}

// The shape gate in front of the date probe must be invisible: every string
// that typed as a date before still does, and non-dates still fall through to
// strings exactly as they always have.
func TestDateShapeGateIsBehaviorNeutral(t *testing.T) {
	for _, s := range []string{
		"2024-05-06",
		"2024/05/06",
		"01/02/2026",
		"2024-05-06 10:11:12",
		"2024-05-06T10:11:12Z",
		"Jan 2, 2026",
		"January 2, 2026",
	} {
		if _, ok := ParseValue(s).(time.Time); !ok {
			t.Errorf("%q no longer types as a date", s)
		}
	}
	for _, s := range []string{"hello", "12abc", "September", "a-b"} {
		if _, ok := ParseValue(s).(string); !ok {
			t.Errorf("%q should stay a string, got %#v", s, ParseValue(s))
		}
	}
}
