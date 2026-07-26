package plugins

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type hunt8Row struct {
	ID     int
	Name   string
	Tenant int
}

func hunt8DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(`CREATE TABLE hunt8_rows (id INTEGER, name TEXT, tenant INTEGER)`).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Exec(`INSERT INTO hunt8_rows VALUES (1,'alice',42),(2,'bob',42),(3,'OTHER-TENANT-SECRET',99)`).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// H8/P8-1: an unmatched '[' in an UNQUOTED value keeps the tokenizer's bracket
// depth above zero, so the token that carries it runs to end-of-input and
// absorbs every following conjunct — including a mandatory scope an application
// appended to untrusted input. The parser reports nothing (BuildE stays nil),
// so widening the gate to the bracket class made STRICT mode accept it: the
// advertised gate silently deleted an arbitrary suffix of the query. Under a
// leading `not` the surviving predicate matches essentially every row.
func TestH8_P8_1_BracketSwallowingTheTailStaysRejected(t *testing.T) {
	swallowing := []string{
		`not name=[x and tenant_id=42`,
		`b=[2 and c=3`,
		`b=a[b and c=3`,
		`not b=a[b and not c=3`,
		`a=1 and b=[2 and c=3`,
		// The '[' need not lead the value, and an unmatched CLOSER earlier in
		// the input must not cancel a later opener.
		`b=x]y[z and tenant_id=42`,
		// The absorbed token can also be split into a fabricated FIELD name
		// (`[a` = "1 and b=2"), and the bracket can land inside the connector
		// itself, which destroys the connector (`a=1 a[nd b=2` filters a column
		// called "a[nd_b").
		`[a=1 and tenant_id=42`,
		`a=1 a[nd tenant_id=42`,
		// An unclosed load= directive swallows the tail the same way. This one
		// the parser DOES diagnose ("unclosed load= directive"), so the gate
		// already rejected it at ef28331 — it is here so a future widening of
		// the gate cannot quietly take it with the rest.
		`load=[Orders:total>100 and tenant_id=7`,
	}
	for _, dsl := range swallowing {
		f := figo.New()
		if err := f.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil {
			f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
			where, args, _ := adapters.BuildRawWhere(f)
			t.Errorf("strict accepted tail-swallowing DSL %q: WHERE %s args=%v (BuildE=%v)",
				dsl, where, args, f.BuildE(adapters.RawAdapter{Dialect: adapters.SQLiteDialect}))
		}
	}

	// The same inputs must stay rejected with repair enabled: repair may only
	// pass on input that survives validation, never the original.
	for _, dsl := range swallowing {
		f := figo.New()
		if err := f.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil && f.GetDSL() == dsl {
			t.Errorf("repair passed tail-swallowing DSL %q through unchanged", dsl)
		}
	}
}

// The end-to-end consequence, executed: with the scope conjunct swallowed the
// query returns another tenant's row. Rejecting the DSL is the only outcome
// that keeps that row out, since no diagnostic surfaces the deletion.
func TestH8_P8_1_SwallowedScopeNeverReachesTheDatabase(t *testing.T) {
	db := hunt8DB(t)

	f := figo.New()
	if err := f.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	untrusted := `not name=[x`
	err := f.AddFiltersFromString(untrusted + ` and tenant_id=42`)
	if err == nil {
		f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
		where, args, bwErr := adapters.BuildRawWhere(f)
		if bwErr != nil {
			t.Fatalf("BuildRawWhere: %v", bwErr)
		}
		var got []hunt8Row
		if e := db.Raw("SELECT * FROM hunt8_rows WHERE "+where, args...).Scan(&got).Error; e != nil {
			t.Fatalf("query: %v", e)
		}
		for _, r := range got {
			if r.Tenant != 42 {
				t.Fatalf("strict accepted %q and the query returned tenant %d row %q (WHERE %s args=%v)",
					untrusted+` and tenant_id=42`, r.Tenant, r.Name, where, args)
			}
		}
		t.Fatalf("strict accepted a DSL whose scope conjunct was swallowed: WHERE %s args=%v", where, args)
	}
	if !strings.Contains(err.Error(), "unmatched opening bracket") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}
}

// The other direction, and the reason the sketched "reject any unbalanced
// bracket in a value" fix was not taken: everything hunt #7 (A5/A7-1, A5/A7-4)
// made acceptable must STAY acceptable. A bracket or paren that is genuinely
// part of a value swallows nothing — there is no whitespace after it for the
// token to absorb — and an unmatched '(' never extends a token at all.
func TestH8_P8_1_ParserValidDelimitersInValuesStayAccepted(t *testing.T) {
	valid := []string{
		`a=1]]`,
		`qty>2]`,
		`code=x]`,
		`status=[draft`,
		`phone=^(555`,
		`phone=^(555 and tenant_id=42`,
		`name<in>[Report(2024,3.14]`,
		`name="a[b" and x<null>`,
		`x<in>[1,2]`,
		`x<bet>(1..5)`,
		`load=[Orders:total>100] and tenant_id=7`,
	}
	for _, dsl := range valid {
		f := figo.New()
		if err := f.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err != nil {
			t.Errorf("strict rejected parser-valid DSL %q: %v", dsl, err)
		}
	}
	// A genuinely unclosed list/range keeps being rejected by foldedDelimiter,
	// not by the bracket class, so the two checks do not depend on each other.
	for _, dsl := range []string{`x<in>[1,2`, `x<nin>[1,2`, `x<bet>(1..2`, `x<in>[a`} {
		f := figo.New()
		if err := f.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil {
			t.Errorf("strict accepted an unclosed list/range %q", dsl)
		}
	}
}

// bracketSwallowsTail is the whole classification, so pin its truth table
// directly: whitespace after an unmatched opener means a token was absorbed.
func TestH8_P8_1_BracketSwallowsTailTruthTable(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`not name=[x and tenant_id=42`, true},
		{`b=x]y[z and c=3`, true},
		{`b=x[[y] and c=3`, true},
		{`load=[Orders:total>100 and tenant_id=7`, true},
		{`status=[draft`, false},
		{`x<in>[1,2`, false},
		{`a=1]]`, false},
		{`qty>2]`, false},
		{`load=[Orders:total>100] and tenant_id=7`, false},
		{`name="a[b" and x<null>`, false}, // the '[' is inside quotes
		{`name="a[b c" and x=1`, false},   // ditto, whitespace and all
		{`phone=^(555 and tenant_id=42`, false},
		{`a=1 and b=2`, false},
	} {
		if got := bracketSwallowsTail(tc.in); got != tc.want {
			t.Errorf("bracketSwallowsTail(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// CRITIQUE §1(a): this commit made the gate's bracket validator quote-AWARE
// while the parser's load= rescan stayed quote-BLIND, so the two disagree in
// the fail-open direction — strict accepts `load=[Orders:name="a]b"] and
// tenant_id=7` and the parser then truncates the tail. The parser half is
// CORE-1 (figo.go, not this package). What this test pins is that the GATE is
// not independently wrong: the DSL is quote-balanced, so no bracket error is
// raised for the gate to override, and the plugin must not start rejecting it.
// Once CORE-1 lands the tail survives, and the assertion below turns from
// "documented as CORE-1's" into a real check without needing an edit.
func TestH8_P8_1_QuotedBracketInLoadIsNotTheGatesDoing(t *testing.T) {
	const dsl = `load=[Orders:name="a]b"] and tenant_id=7`

	bare := figo.New()
	if err := bare.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("bare parse: %v", err)
	}
	bare.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
	bareWhere, _, _ := adapters.BuildRawWhere(bare)

	strict := figo.New()
	if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := strict.AddFiltersFromString(dsl); err != nil {
		t.Fatalf("strict rejected quote-balanced DSL %q: %v", dsl, err)
	}
	strict.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
	strictWhere, _, _ := adapters.BuildRawWhere(strict)

	// The gate must be transparent here: registering the plugin may not change
	// what the parser produced, in either direction.
	if strictWhere != bareWhere {
		t.Fatalf("SyntaxPlugin changed the render: bare %q vs strict %q", bareWhere, strictWhere)
	}
	if !strings.Contains(bareWhere, "tenant_id") {
		t.Skipf("parser still drops the tail of %q (CORE-1, figo.go's quote-blind load= rescan); "+
			"the gate agrees with the parser, which is all this package controls", dsl)
	}
	if !strings.Contains(strictWhere, "tenant_id") {
		t.Fatalf("strict mode lost the tail the parser kept: %q", strictWhere)
	}
}

// H8/P8-4 (plugin half): FinalizeClauses' fail-closed substitution must be a
// function of the projection it is handed, with no memory of an earlier
// fail-closed render. Once the caller narrows the projection to permitted
// columns the plugin hands back the clause list it was given, unchanged.
//
// The remaining half of P8-4 is NOT in this package: figo.go writes the
// finalizer's output back over the instance's authoritative clause list
// (`f.clauses = finalized`), so the never-true pill from render #1 becomes the
// input to render #2 and no public call can recover it. See the report's
// needs_from_other_areas — this test is what proves the plugin side is clean,
// so hunt #9 does not come looking here.
func TestH8_P8_4_FieldsPluginFailClosedRenderHasNoMemory(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("secret")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	callerClauses := []figo.Expr{figo.EqExpr{Field: "tenant_id", Value: 42}}

	// Render #1: the projection consists only of forbidden columns, so the
	// plugin must fail the render closed (H6/P2 — do not hand back the
	// forbidden column, and do not clear the set into a SELECT *).
	f.SetSelectFields("secret")
	first := fp.FinalizeClauses(f, callerClauses)
	if len(first) != 1 {
		t.Fatalf("fail-closed render returned %d clauses, want the never-true clause: %#v", len(first), first)
	}
	if _, ok := first[0].(figo.OrExpr); !ok {
		t.Fatalf("fail-closed render returned %#v, want figo.OrExpr{}", first[0])
	}

	// Render #2: the caller fixes the projection. The plugin must return the
	// caller's clauses — not the pill, and not a pruned version of it.
	f.SetSelectFields("name")
	second := fp.FinalizeClauses(f, callerClauses)
	if len(second) != len(callerClauses) {
		t.Fatalf("after the projection was corrected the plugin returned %#v, want %#v", second, callerClauses)
	}
	if eq, ok := second[0].(figo.EqExpr); !ok || eq.Field != "tenant_id" {
		t.Fatalf("after the projection was corrected the plugin returned %#v, want the caller's clause", second[0])
	}

	// And feeding the pill back in (which is what figo.go currently does) must
	// not make the plugin invent a second one: the plugin is not the thing that
	// makes the state persist.
	third := fp.FinalizeClauses(f, first)
	if len(third) != 1 {
		t.Fatalf("plugin altered a clause list it was handed: %#v", third)
	}
}
