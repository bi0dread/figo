package figo_test

// Round-3 bug-hunt regression tests for the core parser and build pipeline.
// Each test pins a fix; the "was" comments describe the pre-fix behavior.

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

func rawWhereOf(t *testing.T, dsl string) (string, []any, error) {
	t.Helper()
	f := figo.New()
	if err := f.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("AddFiltersFromString(%q): %v", dsl, err)
	}
	buildErr := f.BuildE(adapters.RawAdapter{})
	where, args, err := adapters.BuildRawWhere(f)
	if err != nil {
		t.Fatalf("BuildRawWhere(%q): %v", dsl, err)
	}
	return where, args, buildErr
}

// Was: "not(a=1 or b=2)" parsed as a filter on the literal field "not(a",
// silently dropping the NOT; "a=1 or(b=2)" turned the OR into an AND on
// field "or(b".
func TestGluedKeywordBeforeParen(t *testing.T) {
	where, args, _ := rawWhereOf(t, "not(a=1 or b=2)")
	if where != "NOT ((`a` = ? OR `b` = ?))" {
		t.Errorf("not(...) rendered %q, want NOT ((`a` = ? OR `b` = ?))", where)
	}
	if len(args) != 2 || args[0] != int64(1) || args[1] != int64(2) {
		t.Errorf("args = %v", args)
	}

	where, args, _ = rawWhereOf(t, "a=1 or(b=2)")
	if where != "(`a` = ? OR `b` = ?)" {
		t.Errorf("or(...) rendered %q, want (`a` = ? OR `b` = ?)", where)
	}
	if len(args) != 2 || args[1] != int64(2) {
		t.Errorf("args = %v", args)
	}

	where, _, _ = rawWhereOf(t, "a=1 and(b=2)")
	if where != "(`a` = ? AND `b` = ?)" {
		t.Errorf("and(...) rendered %q", where)
	}
}

// Was: a load= token with no '[' started a bracket scan at depth 1 that ran to
// the end of the string, swallowing every subsequent filter — the query
// silently became match-everything under Build().
func TestMalformedLoadDoesNotSwallowRestOfDSL(t *testing.T) {
	where, args, buildErr := rawWhereOf(t, `load=x id=5 and status="active"`)
	if where != "(`id` = ? AND `status` = ?)" {
		t.Errorf("filters after malformed load= were lost: where=%q args=%v", where, args)
	}
	if buildErr == nil {
		t.Error("expected a diagnostic for the malformed load= directive")
	}
}

// Was: an invalid operator value appended a node with a nil expression; the
// precedence pass skipped it WITHOUT consuming the pending "not", which then
// negated the NEXT predicate: "not price<bet>(broken) and y=1" rendered
// NOT(y=1).
func TestDanglingNotDoesNotJumpToNextPredicate(t *testing.T) {
	where, args, buildErr := rawWhereOf(t, "not price<bet>(broken) and y=1")
	if where != "`y` = ?" {
		t.Errorf("where = %q, want `y` = ? (un-negated)", where)
	}
	if len(args) != 1 || args[0] != int64(1) {
		t.Errorf("args = %v", args)
	}
	if buildErr == nil {
		t.Error("expected diagnostics for the invalid <bet> value and dropped not")
	}
}

// Was: the tokenizer ended a token at any closing quote outside brackets, so
// BETWEEN values with quoted parenthesized bounds fell apart; the quoted-bound
// ".." split used strings.Index and cut inside quotes.
func TestBetweenQuotedBounds(t *testing.T) {
	where, args, buildErr := rawWhereOf(t, `name<bet>("(a".."b)") and c=3`)
	if buildErr != nil {
		t.Errorf("unexpected diagnostics: %v", buildErr)
	}
	if where != "(`name` BETWEEN ? AND ? AND `c` = ?)" {
		t.Errorf("where = %q", where)
	}
	if len(args) != 3 || args[0] != "(a" || args[1] != "b)" || args[2] != int64(3) {
		t.Errorf("args = %v", args)
	}

	// Quoted low bound containing the ".." separator.
	where, args, _ = rawWhereOf(t, `code <bet> "a..b".."c"`)
	if where != "`code` BETWEEN ? AND ?" || len(args) != 2 || args[0] != "a..b" || args[1] != "c" {
		t.Errorf("where = %q args = %v, want bounds a..b / c", where, args)
	}
}

// Was: load= segments split on '|' with a plain strings.Split, cutting inside
// a quoted value and corrupting the preload filter.
func TestLoadQuotedPipe(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString(`a=1 load=[Orders:name="x|y"]`)
	if err := f.BuildE(adapters.RawAdapter{}); err != nil {
		t.Errorf("unexpected diagnostics: %v", err)
	}
	pre, err := adapters.BuildRawPreloads(f)
	if err != nil {
		t.Fatalf("BuildRawPreloads: %v", err)
	}
	p, ok := pre["Orders"]
	if !ok {
		t.Fatal("preload Orders missing")
	}
	if len(p.Args) != 1 || p.Args[0] != "x|y" {
		t.Errorf("preload args = %v, want [x|y]", p.Args)
	}
}

// Was: parseListLiteral stripped only [...] so <in>(1,2) produced the string
// elements "(1" and "2)".
func TestParenthesizedInList(t *testing.T) {
	where, args, _ := rawWhereOf(t, "a<in>(1,2)")
	if where != "`a` IN (?,?)" {
		t.Errorf("where = %q", where)
	}
	if len(args) != 2 || args[0] != int64(1) || args[1] != int64(2) {
		t.Errorf("args = %v, want int64 1,2", args)
	}
}

// Was: "sort=:desc" rendered ORDER BY “ (invalid SQL) with no diagnostic, and
// any sort= directive with zero valid segments replaced an earlier valid sort
// with an empty one.
func TestSortEmptyFieldAndClobber(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("a=1 sort=:desc")
	buildErr := f.BuildE(adapters.RawAdapter{})
	if buildErr == nil {
		t.Error("expected a diagnostic for the empty sort field")
	}
	if s := f.GetSort(); s != nil && len(s.Columns) > 0 {
		t.Errorf("sort = %+v, want none", s)
	}

	f = figo.New()
	_ = f.AddFiltersFromString("a=1 sort=id:desc sort=")
	buildErr = f.BuildE(adapters.RawAdapter{})
	if buildErr == nil {
		t.Error("expected a diagnostic for the bare sort= directive")
	}
	s := f.GetSort()
	if s == nil || len(s.Columns) != 1 || s.Columns[0].Name != "id" || !s.Columns[0].Desc {
		t.Errorf("sort = %+v, want the earlier id:desc preserved", s)
	}
}

// Was: a load= segment whose filter produced no conditions dropped the whole
// preload relation while the diagnostic claimed only the inner directive had
// been ignored.
func TestLoadSegmentWithNoConditionsStillPreloads(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("a=1 load=[T:sort=id:desc]")
	buildErr := f.BuildE(adapters.RawAdapter{})
	if buildErr == nil {
		t.Error("expected diagnostics")
	}
	if _, ok := f.GetPreloads()["T"]; !ok {
		t.Error("relation T was dropped; want it preloaded unconditioned")
	}
}

// Was: unbalanced parentheses and dangling connectors were silently dropped
// with no BuildE diagnostic at all.
func TestStructuralDiagnostics(t *testing.T) {
	for _, dsl := range []string{"(id=1", "id=1) or name=\"x\"", "id=1 and", "or id=1", "not"} {
		f := figo.New()
		_ = f.AddFiltersFromString(dsl)
		if err := f.BuildE(adapters.RawAdapter{}); err == nil {
			t.Errorf("BuildE(%q) = nil, want a diagnostic", dsl)
		}
	}
}

// Was: "name =" (and glued "name=") built the comparison `name` = ” with no
// diagnostic; an explicit empty string is spelled name="".
func TestValuelessOperatorDiagnosed(t *testing.T) {
	for _, dsl := range []string{"id=1 and name =", "id=1 and name="} {
		f := figo.New()
		_ = f.AddFiltersFromString(dsl)
		buildErr := f.BuildE(adapters.RawAdapter{})
		where, args, _ := adapters.BuildRawWhere(f)
		if buildErr == nil {
			t.Errorf("BuildE(%q) = nil, want a diagnostic", dsl)
		}
		if where != "`id` = ?" {
			t.Errorf("where(%q) = %q args=%v, want the valueless comparison dropped", dsl, where, args)
		}
		// name="" must still build an explicit empty-string comparison.
		f2 := figo.New()
		_ = f2.AddFiltersFromString(`name=""`)
		if err := f2.BuildE(adapters.RawAdapter{}); err != nil {
			t.Errorf(`BuildE(name="") = %v, want nil`, err)
		}
		w2, a2, _ := adapters.BuildRawWhere(f2)
		if w2 != "`name` = ?" || len(a2) != 1 || a2[0] != "" {
			t.Errorf(`name="" rendered %q %v`, w2, a2)
		}
	}
}

// Was: page= presence inside load= was detected by comparing against the zero
// Page, so page=skip:0,take:0 escaped the "not supported" diagnostic.
func TestZeroPageInsideLoadDiagnosed(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("a=1 load=[T:id=1 page=skip:0,take:0]")
	err := f.BuildE(adapters.RawAdapter{})
	if err == nil || !strings.Contains(err.Error(), "page= inside load=") {
		t.Errorf("BuildE = %v, want the page=-inside-load diagnostic", err)
	}
}

// Was: AddFiltersFromString("") was a silent no-op, so filters could never be
// cleared through the DSL API.
func TestEmptyDSLClearsFilters(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("id=1")
	f.Build(adapters.RawAdapter{})
	if w, _, _ := adapters.BuildRawWhere(f); w == "" {
		t.Fatal("setup: expected a WHERE clause")
	}

	_ = f.AddFiltersFromString("")
	f.Build(nil)
	if f.GetDSL() != "" {
		t.Errorf("GetDSL() = %q, want empty", f.GetDSL())
	}
	if w, _, _ := adapters.BuildRawWhere(f); w != "" {
		t.Errorf("where = %q, want empty after clearing the DSL", w)
	}
}

// Was: a page= parsed from the DSL survived a DSL replacement (sort=/load=
// were cleared) — the old query's pagination leaked into the new one. SetPage
// must still outlive rebuilds.
func TestPageOriginTracking(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("id=1 page=skip:40,take:5")
	f.Build(adapters.RawAdapter{})
	if p := f.GetPage(); p.Skip != 40 || p.Take != 5 {
		t.Fatalf("setup: page = %+v", p)
	}
	_ = f.AddFiltersFromString(`name="x"`)
	f.Build(nil)
	if p := f.GetPage(); p.Skip != 0 || p.Take != 20 {
		t.Errorf("page = %+v after DSL replacement, want the default {0 20}", p)
	}

	f2 := figo.New()
	f2.SetPage(7, 3)
	_ = f2.AddFiltersFromString("id=1")
	f2.Build(adapters.RawAdapter{})
	_ = f2.AddFiltersFromString(`name="x"`)
	f2.Build(nil)
	if p := f2.GetPage(); p.Skip != 7 || p.Take != 3 {
		t.Errorf("page = %+v, want SetPage's {7 3} preserved", p)
	}
}

// Was: NaN/Inf parsed to float64 and reached adapters as unparameterizable
// non-finite values; they now stay strings (like oversized integers).
func TestNonFiniteLiteralsStayStrings(t *testing.T) {
	_, args, _ := rawWhereOf(t, "a=NaN or b=Inf")
	if len(args) != 2 || args[0] != "NaN" || args[1] != "Inf" {
		t.Errorf("args = %v, want the raw strings", args)
	}
}

// Was: Walk's leaf copies shared their Values backing array with GetClauses
// snapshots, so a visitor mutating v.Values[0] corrupted prior snapshots.
func TestWalkDoesNotMutateSnapshots(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("a<in>[1,2,3]")
	f.Build(adapters.RawAdapter{})
	snapshot := f.GetClauses()

	f.Walk(func(n figo.Expr) {
		if in, ok := n.(*figo.InExpr); ok {
			in.Values[0] = int64(99)
		}
	})

	in, ok := snapshot[0].(figo.InExpr)
	if !ok {
		t.Fatalf("snapshot[0] is %T", snapshot[0])
	}
	if in.Values[0] != int64(1) {
		t.Errorf("snapshot mutated: Values[0] = %v, want 1", in.Values[0])
	}
}

// panicPlugin panics inside FilterExpr, mid-Build.
type panicPlugin struct{}

func (panicPlugin) Name() string                                      { return "panic-plugin" }
func (panicPlugin) Version() string                                   { return "0" }
func (panicPlugin) Initialize(figo.Figo) error                        { return nil }
func (panicPlugin) BeforeQuery(figo.Figo, any) error                  { return nil }
func (panicPlugin) AfterQuery(figo.Figo, any, any) error              { return nil }
func (panicPlugin) BeforeParse(_ figo.Figo, d string) (string, error) { return d, nil }
func (panicPlugin) AfterParse(figo.Figo, string) error                { return nil }
func (panicPlugin) FilterExpr(figo.Figo, figo.Expr) figo.Expr         { panic("boom") }

// Was: Build clears state before running plugin hooks, so a caller that
// recovered a hook panic kept an instance rendering a filter-less
// match-everything query. It must fail CLOSED (the canonical 1=0) instead.
func TestPluginPanicFailsClosed(t *testing.T) {
	f := figo.New()
	if err := f.RegisterPlugin(panicPlugin{}); err != nil {
		t.Fatal(err)
	}
	_ = f.AddFiltersFromString("id=1")

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the plugin panic to propagate")
			}
		}()
		f.Build(adapters.RawAdapter{})
	}()

	where, _, err := adapters.BuildRawWhere(f)
	if err != nil {
		t.Fatalf("BuildRawWhere: %v", err)
	}
	if where != "1=0" {
		t.Errorf("where = %q after plugin panic, want the fail-closed 1=0", where)
	}
}
