package plugins

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

func sqliteSQL(t *testing.T, f figo.Figo, table string) string {
	t.Helper()
	s, ok := adapters.RawAdapter{Dialect: adapters.SQLiteDialect}.GetSqlString(f, table)
	if !ok {
		t.Fatalf("render failed for %s", table)
	}
	return s
}

// H6/P1: the whitelist projection fallback wrote the RAW allowed list through
// SetSelectFields, so a column the same plugin's ignore list refuses in WHERE
// and ORDER BY came back in the SELECT list.
func TestH6_P1_WhitelistProjectionFallbackHonoursIgnoreList(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("id", "password_hash")
	fp.EnableFieldWhitelist()
	fp.AddIgnoreFields("password_hash")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("password_hash") // the only requested column is forbidden
	if err := f.AddFiltersFromString(`id>0`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	if got := f.GetSelectFields(); got["password_hash"] {
		t.Fatalf("ignored column substituted back into the projection: %v", got)
	}
	if sql := sqliteSQL(t, f, "users"); strings.Contains(sql, "password_hash") {
		t.Fatalf("ignored column reached the SELECT list: %s", sql)
	}
}

// H6/P2: with no whitelist to substitute, a projection made up ONLY of
// forbidden columns used to be left intact, returning exactly the column the
// guide promises AddSelectFields cannot return. It must now fail closed.
func TestH6_P2_AllForbiddenProjectionFailsClosed(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("password_hash")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("password_hash")
	if err := f.AddFiltersFromString(`id>0`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	sql := sqliteSQL(t, f, "users")
	if !strings.Contains(sql, "1=0") {
		t.Fatalf("forbidden-only projection did not fail closed: %s", sql)
	}
	if strings.Contains(sql, `"id" > 0`) {
		t.Fatalf("the caller's filter survived alongside the never-true clause: %s", sql)
	}
}

// Same shape, but every whitelisted column is also ignored: there is no safe
// substitute, so this must fail closed too.
func TestH6_P2_WhitelistEntirelyIgnoredFailsClosed(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("password_hash")
	fp.EnableFieldWhitelist()
	fp.AddIgnoreFields("password_hash")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("password_hash")
	if err := f.AddFiltersFromString(`id>0`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	if sql := sqliteSQL(t, f, "users"); !strings.Contains(sql, "1=0") {
		t.Fatalf("want a never-true clause, got %s", sql)
	}
}

// A projection with at least one permitted column must NOT fail closed —
// the narrowing path is unaffected.
func TestH6_P2_PartiallyPermittedProjectionStillNarrows(t *testing.T) {
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("password_hash")

	f := figo.New()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	f.AddSelectFields("id", "password_hash")
	if err := f.AddFiltersFromString(`id>0`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	sql := sqliteSQL(t, f, "users")
	if strings.Contains(sql, "1=0") || strings.Contains(sql, "password_hash") {
		t.Fatalf("want a narrowed projection and a live filter, got %s", sql)
	}
	if !strings.Contains(sql, `"id" > 0`) {
		t.Fatalf("filter lost: %s", sql)
	}
}

// The projection fail-closed must hold in BOTH finalizer registration orders:
// a scope injected before it must not survive as the only clause, and one
// injected after it must not reopen the query.
func TestH6_P2_FailsClosedInBothFinalizerOrders(t *testing.T) {
	for _, fieldsFirst := range []bool{true, false} {
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("password_hash")
		sp := NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: int64(1)})

		f := figo.New()
		var err error
		if fieldsFirst {
			if err = f.RegisterPlugin(fp); err == nil {
				err = f.RegisterPlugin(sp)
			}
		} else {
			if err = f.RegisterPlugin(sp); err == nil {
				err = f.RegisterPlugin(fp)
			}
		}
		if err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		f.AddSelectFields("password_hash")
		if err := f.AddFiltersFromString(`id>0`); err != nil {
			t.Fatalf("AddFiltersFromString: %v", err)
		}
		f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

		if sql := sqliteSQL(t, f, "users"); !strings.Contains(sql, "1=0") {
			t.Errorf("fieldsFirst=%v: want a never-true clause, got %s", fieldsFirst, sql)
		}
	}
}

// H7/A3-2: under NoChangeNaming the naming func collapses nothing, so a
// byte-exact ignore-list lookup was defeated by a case variant or a
// table-qualified spelling — while MySQL/SQLite resolve both to the very
// column the ignore list exists to hide.
func TestH7_A3_2_IgnoreListMatchesCaseAndQualifiedSpellings(t *testing.T) {
	for _, dsl := range []string{
		`password="h"`,
		`Password="h"`,
		`PASSWORD="h"`,
		`pAssword="h"`,
		`probe_users.password="h"`,
	} {
		f := figo.New()
		f.SetNamingFunc(figo.NoChangeNaming)
		fp := NewFieldsPlugin()
		fp.AddIgnoreFields("password")
		if err := f.RegisterPlugin(fp); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err != nil {
			t.Fatalf("AddFiltersFromString(%q): %v", dsl, err)
		}
		f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

		sql := sqliteSQL(t, f, "users")
		if strings.Contains(strings.ToLower(sql), "password") {
			t.Errorf("%q: ignored column survived: %s", dsl, sql)
		}
	}
}

// The same bypass switched a ValidationPlugin deny-rule off.
func TestH7_A3_2_ValidationRulesMatchCaseAndQualifiedSpellings(t *testing.T) {
	for _, dsl := range []string{
		`password="h"`,
		`Password="h"`,
		`PASSWORD="h"`,
		`pAssword="h"`,
		`probe_users.password="h"`,
	} {
		f := figo.New()
		f.SetNamingFunc(figo.NoChangeNaming)
		vp := NewValidationPlugin()
		vp.AddRule(ValidationRule{Field: "password", Rule: "deny", Handler: func(field, rule string, value any) error {
			return errDenied{}
		}})
		if err := f.RegisterPlugin(vp); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil {
			t.Errorf("%q: deny rule did not fire", dsl)
		}
	}
}

type errDenied struct{}

func (errDenied) Error() string { return "denied" }

// Negative control for the canonicalisation: a QUALIFIED registration keeps
// its qualifier, so registering "orders.secret" must not silently deny the
// unrelated top-level "secret" column, and an unrelated column must survive.
func TestH7_A3_2_CanonicalMatchDoesNotOverPrune(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	fp := NewFieldsPlugin()
	fp.AddIgnoreFields("orders.secret")
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`secret="x" and passwords="y"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	sql := sqliteSQL(t, f, "users")
	if !strings.Contains(sql, `"secret"`) || !strings.Contains(sql, `"passwords"`) {
		t.Fatalf("unrelated columns were pruned: %s", sql)
	}
}

// The ALLOW side stays byte-exact: broadening it would admit spellings the
// policy never approved. A whitelist of {id} must still refuse "ID".
func TestH7_A3_2_WhitelistStaysExactAndFailsClosed(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	fp := NewFieldsPlugin()
	fp.SetAllowedFields("id")
	fp.EnableFieldWhitelist()
	if err := f.RegisterPlugin(fp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`ID=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})

	if sql := sqliteSQL(t, f, "users"); strings.Contains(sql, "WHERE") {
		t.Fatalf("whitelist admitted a spelling it was not given: %s", sql)
	}
}

// H7/A7-1 + A7-4 (one root cause: the two `false` gate flags in
// validateDSLSyntaxClassified). A lone bracket or paren inside an unquoted
// VALUE is parser-valid DSL: strict mode must not reject it, and repair must
// be the IDENTITY on it. Repair used to delete the closer and append one at
// end-of-input, changing both the queried value and its Go type.
func TestH7_A7_1_A7_4_ParserValidBracketsAndParensAreNotRejectedOrRewritten(t *testing.T) {
	valid := []string{
		`code=x]`,
		`qty>2]`,
		`name<in>[Report(2024,3.14]`,
		`phone=^(555 and tenant_id=42`,
	}
	for _, dsl := range valid {
		strict := figo.New()
		if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := strict.AddFiltersFromString(dsl); err != nil {
			t.Errorf("strict rejected parser-valid DSL %q: %v", dsl, err)
		}

		repair := figo.New()
		if err := repair.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := repair.AddFiltersFromString(dsl); err != nil {
			t.Errorf("repair rejected parser-valid DSL %q: %v", dsl, err)
			continue
		}
		if got := repair.GetDSL(); got != dsl {
			t.Errorf("repair rewrote parser-valid DSL: sent %q, stored %q", dsl, got)
		}
	}

	// Genuine structural imbalance IS diagnosed by the parser, so the gate
	// still lets strict reject it and repair fix it.
	for _, tc := range []struct{ dsl, repaired string }{
		{`(a=1 and b=2`, `(a=1 and b=2)`},
		{`a=1)`, `a=1`},
	} {
		strict := figo.New()
		if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := strict.AddFiltersFromString(tc.dsl); err == nil {
			t.Errorf("strict accepted genuinely unbalanced %q", tc.dsl)
		}
		repair := figo.New()
		if err := repair.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := repair.AddFiltersFromString(tc.dsl); err != nil {
			t.Errorf("repair failed on %q: %v", tc.dsl, err)
			continue
		}
		if got := repair.GetDSL(); got != tc.repaired {
			t.Errorf("repair(%q) = %q, want %q", tc.dsl, got, tc.repaired)
		}
	}
}

// Extending the gate to the bracket/paren classes must not let a genuinely
// broken list or range through: the parser folds an unclosed delimiter into
// the first element WITHOUT a diagnostic, so the gate's tree inspection is the
// only thing that can see it.
func TestH7_A7_1_UnclosedListOrRangeStaysRejected(t *testing.T) {
	for _, dsl := range []string{`x<in>[1,2`, `x<nin>[1,2`, `x<bet>(1..2`, `x<in>[a`, `x<in>[[a,2]`} {
		strict := figo.New()
		if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := strict.AddFiltersFromString(dsl); err == nil {
			t.Errorf("strict accepted an unclosed list/range %q", dsl)
		}
	}
	// A well-formed list or range, and a bracket that is genuinely part of a
	// value, must still pass.
	for _, dsl := range []string{`x<in>[1,2]`, `x<bet>(1..2)`, `status=[draft`, `a=1]]`} {
		strict := figo.New()
		if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := strict.AddFiltersFromString(dsl); err != nil {
			t.Errorf("strict rejected parser-valid %q: %v", dsl, err)
		}
	}
}

// H7/A7-2: the core parser has exactly one quote rune. Modelling the apostrophe as a
// second one made repair append a closing apostrophe at end-of-input, which
// landed inside an UNRELATED predicate's value and fabricated predicates.
func TestH7_A7_2_ApostropheIsNotAQuoteCharacter(t *testing.T) {
	// The appended ')' must close the group; the apostrophe must be left in
	// the value it belongs to and no other value may change.
	f := figo.New()
	if err := f.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`a=b' and (c=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	if got := f.GetDSL(); got != `a=b' and (c=1)` {
		t.Fatalf("stored DSL = %q, want %q", got, `a=b' and (c=1)`)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
	if sql := sqliteSQL(t, f, "items"); !strings.Contains(sql, `"c" = 1`) {
		t.Fatalf("an unrelated predicate was corrupted (int64 1 became a string): %s", sql)
	}

	// A missing operand must NOT be repaired into a fabricated value.
	g := figo.New()
	if err := g.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := g.AddFiltersFromString(`note=don't and b=`); err == nil {
		t.Fatalf("repair invented a value for the missing operand: %q", g.GetDSL())
	}

	// Closing `t>"` into `t>""` satisfies every structural check but parses to
	// an empty value — the artifact parsedExprLooksClean exists to refuse.
	h := figo.New()
	if err := h.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := h.AddFiltersFromString(`t>"`); err == nil {
		t.Fatalf("repair produced an empty-value predicate: %q", h.GetDSL())
	}

	// A dangling connector after an apostrophe value is still stripped, and no
	// phantom quote is appended to shield it.
	k := figo.New()
	if err := k.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := k.AddFiltersFromString(`name=O'Brien and`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	if got := k.GetDSL(); got != `name=O'Brien` {
		t.Fatalf("stored DSL = %q, want %q", got, `name=O'Brien`)
	}
}

// H7/A7-3: the leading and/or checks used a regex word boundary, so they fired
// on any FIELD NAME starting with and/or followed by a non-word byte. `or.id=5`
// was silently retargeted to `id`, and `or=5` to an unfiltered scan.
func TestH7_A7_3_LeadingConnectorNeedsATokenBoundary(t *testing.T) {
	legal := []string{
		`or.id=5 and tenant_id=42`,
		`or=5`,
		`and-x=1`,
		`and.x=1 and tenant_id=42`,
		`order_id=5`,
	}
	for _, dsl := range legal {
		strict := figo.New()
		if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := strict.AddFiltersFromString(dsl); err != nil {
			t.Errorf("strict rejected legal DSL %q: %v", dsl, err)
		}

		repair := figo.New()
		if err := repair.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := repair.AddFiltersFromString(dsl); err != nil {
			t.Errorf("repair rejected legal DSL %q: %v", dsl, err)
			continue
		}
		if got := repair.GetDSL(); got != dsl {
			t.Errorf("repair rewrote legal DSL: sent %q, stored %q", dsl, got)
		}
	}

	// A genuine dangling leading connector is still stripped.
	repair := figo.New()
	if err := repair.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := repair.AddFiltersFromString(`and x=1`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	if got := repair.GetDSL(); got != `x=1` {
		t.Fatalf("repair(%q) = %q, want %q", `and x=1`, got, `x=1`)
	}
	strict := figo.New()
	if err := strict.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := strict.AddFiltersFromString(`and x=1`); err == nil {
		t.Fatal("strict accepted a genuine leading connector")
	}
}

// H6/P4: DSL rejected in BeforeParse left no trace at all, and with repair
// enabled the trail recorded only the REPAIRED string.
func TestH6_P4_AuditRecordsTheDSLAsReceived(t *testing.T) {
	t.Run("RejectedInBeforeParse", func(t *testing.T) {
		ap := NewAuditPlugin(nil, 50)
		f := figo.New()
		if err := f.RegisterPlugin(ap); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.RegisterPlugin(NewSyntaxPlugin(false)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(`name="john and`); err == nil {
			t.Fatal("expected the syntax rejection")
		}
		h := ap.History()
		if len(h) != 1 || h[0].Kind != "parse-attempt" || h[0].DSL != `name="john and` {
			t.Fatalf("rejected DSL is invisible to the trail: %+v", h)
		}
	})

	t.Run("RepairedInput", func(t *testing.T) {
		ap := NewAuditPlugin(nil, 50)
		f := figo.New()
		if err := f.RegisterPlugin(ap); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.RegisterPlugin(NewSyntaxPlugin(true)); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(`(name="john" and age>25`); err != nil {
			t.Fatalf("AddFiltersFromString: %v", err)
		}
		h := ap.History()
		if len(h) != 2 || h[0].DSL != `(name="john" and age>25` {
			t.Fatalf("what the caller sent is not in the trail: %+v", h)
		}
		if h[1].DSL != `(name="john" and age>25)` {
			t.Fatalf("the repaired DSL is not in the trail: %+v", h)
		}
	})
}

// H7/A7-5: the parameterized path recorded the query SHAPE and dropped every
// bound value, and a Query type the plugin does not model degraded to a bare
// Go type name.
func TestH7_A7_5_AuditRecordsBoundValuesAndQueryContents(t *testing.T) {
	ap := NewAuditPlugin(nil, 50)
	f := figo.New()
	if err := f.RegisterPlugin(ap); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`tenant_id=2 and secret="beta"`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
	_ = f.GetQuery(adapters.RawContext{Table: "accts"})

	var q *AuditEntry
	for i := range ap.History() {
		if e := ap.History()[i]; e.Kind == "query" {
			q = &e
		}
	}
	if q == nil {
		t.Fatal("no query entry recorded")
	}
	if len(q.Args) != 2 {
		t.Fatalf("bound values missing from the trail: %+v", q)
	}
	if q.DSL == "" {
		t.Fatalf("query entry cannot be tied back to its parse: %+v", q)
	}

	// Mongo's Query type is not modelled by the plugin: it must still record
	// contents, not just the Go type name.
	ap2 := NewAuditPlugin(nil, 50)
	g := figo.New()
	if err := g.RegisterPlugin(ap2); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := g.AddFiltersFromString(`tenant_id=2`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	g.Build(adapters.MongoAdapter{})
	_ = g.GetQuery(nil)
	for _, e := range ap2.History() {
		if e.Kind != "query" {
			continue
		}
		if e.Result == "adapters.MongoFindQuery" || !strings.Contains(e.Result, "tenant_id") {
			t.Fatalf("query entry carries no information: %q", e.Result)
		}
	}
}

// H7/A7-6: the DSL's literal form decides the parsed Go type, so a value
// written as a number/date/bool silently skipped the string-shaped built-ins.
func TestH7_A7_6_BuiltinValidatorsFailClosedOnNonStrings(t *testing.T) {
	for _, dsl := range []string{`email="nope"`, `email=12345`, `email=2024-01-31`, `email=true`, `email=1.5`} {
		f := figo.New()
		vp := NewValidationPlugin()
		vp.RegisterValidator(EmailValidator{})
		vp.AddRule(ValidationRule{Field: "email", Rule: "email"})
		if err := f.RegisterPlugin(vp); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil {
			t.Errorf("%q: the email rule did not fire", dsl)
		}
	}

	for _, dsl := range []string{`name="ab"`, `name=12`, `name=2024-01-31`} {
		f := figo.New()
		vp := NewValidationPlugin()
		vp.RegisterValidator(MinLengthValidator{})
		vp.AddRule(ValidationRule{Field: "name", Rule: "min_length"})
		if err := f.RegisterPlugin(vp); err != nil {
			t.Fatalf("RegisterPlugin: %v", err)
		}
		if err := f.AddFiltersFromString(dsl); err == nil {
			t.Errorf("%q: the min_length rule did not fire", dsl)
		}
	}

	// A valid string value still passes.
	f := figo.New()
	vp := NewValidationPlugin()
	vp.RegisterValidator(EmailValidator{})
	vp.AddRule(ValidationRule{Field: "email", Rule: "email"})
	if err := f.RegisterPlugin(vp); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := f.AddFiltersFromString(`email="a@b.com"`); err != nil {
		t.Fatalf("valid email rejected: %v", err)
	}

	// RequiredValidator has no type assertion by design: a non-nil, non-empty
	// value of any type satisfies "required".
	g := figo.New()
	vp2 := NewValidationPlugin()
	vp2.RegisterValidator(RequiredValidator{})
	vp2.AddRule(ValidationRule{Field: "n", Rule: "required"})
	if err := g.RegisterPlugin(vp2); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := g.AddFiltersFromString(`n=12`); err != nil {
		t.Fatalf("required rule wrongly rejected a non-string value: %v", err)
	}
}
