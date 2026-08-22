package plugins

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guarded builds an instance carrying the guard, feeds it the DSL, and — like
// a caller that ignores the returned error — builds anyway. It returns the
// AddFiltersFromString error and the rendered WHERE, which is what proves the
// refusal narrows rather than widens.
func guarded(t *testing.T, g *InjectionGuardPlugin, dsl string) (error, string) {
	t.Helper()
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(g))

	addErr := f.AddFiltersFromString(dsl)
	f.Build(adapters.RawAdapter{}) // the caller IGNORES addErr

	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	return addErr, where
}

// ---------------------------------------------------------------------------
// the reported case
// ---------------------------------------------------------------------------

// The exact DSL from the stage-express report. Before the guard this rendered
// WHERE `1` = ? with addErr == nil and BuildE == nil, so the service had no
// signal at all and forwarded the engine's refusal — which quotes the whole
// statement — straight back to the caller.
func TestReportedDSLIsRefusedAndFailsClosed(t *testing.T) {
	const reported = `page=skip:0,take:50 and sort=median_price:desc or 1=1`

	err, where := guarded(t, NewInjectionGuardPlugin(), reported)

	require.Error(t, err, "the reported DSL was accepted")
	assert.Contains(t, err.Error(), `filter field "1"`)
	assert.Contains(t, err.Error(), "is only digits, which names no column")
	assert.Equal(t, "1=0", where,
		"a caller ignoring the error must get a query matching nothing, not an unfiltered scan")
}

// Every adapter has to express the refusal, not just the one the report came
// from. An empty OrExpr is figo's canonical never-true clause; this pins what
// each backend actually emits for it.
func TestRefusalRendersNeverTrueOnEveryAdapter(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	require.Error(t, f.AddFiltersFromString(`sort=median_price:desc or 1=1`))
	f.Build(nil)

	require.Equal(t, []figo.Expr{figo.OrExpr{}}, f.GetClauses(),
		"the clause list must be replaced by the pill, not merely extended with it")

	where, args, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
	assert.Empty(t, args)

	filter, err := adapters.BuildMongoFilter(f)
	require.NoError(t, err)
	raw, err := json.Marshal(filter)
	require.NoError(t, err)
	assert.JSONEq(t, `{"$nor":[{}]}`, string(raw))

	esq, err := adapters.BuildElasticsearchQuery(f)
	require.NoError(t, err)
	raw, err = json.Marshal(esq)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "match_none")
}

// The hostile identifier must not reach the engine through ORDER BY or the
// SELECT list either — reaching the engine at all is what leaked the schema.
//
// The two positions arrive by different routes and are closed differently. A
// sort= directive dies with the DSL when core refuses the instance, so nothing
// is left to render. A projection set through SetSelectFields is instance
// state that SURVIVES the refusal, so the finalizer has to strip it entry by
// entry.
func TestHostileSortAndProjectionAreStrippedFromTheRender(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	f.SetSelectFields("title", "(select 1)")
	require.Error(t, f.AddFiltersFromString(`sort=median_price:desc,1:desc`))
	f.Build(adapters.RawAdapter{})

	sel, _, err := adapters.BuildRawSelect(f, "t")
	require.NoError(t, err)
	assert.Contains(t, sel, "1=0")
	assert.NotContains(t, sel, "ORDER BY", "the refused sort= still reached the statement")
	assert.NotContains(t, sel, "`1`", "the hostile identifier still reached the statement")
	assert.NotContains(t, sel, "select 1", "the hostile projection still reached the statement")
	assert.Contains(t, sel, "`title`", "a clean projection entry was dropped along with the bad one")
}

// The same two positions reached programmatically, where there is no DSL to
// roll back and the finalizer's own screen is the only thing running. Only the
// offending entries go: a clean sort column and a clean projection column
// alongside a hostile one are left exactly as the caller set them.
func TestProgrammaticSortAndProjectionKeepTheCleanEntries(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	f.SetSelectFields("title", "(select 1)")
	f.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{
		{Name: "median_price", Desc: true},
		{Name: "1", Desc: true},
	}})
	f.Build(adapters.RawAdapter{})

	sel, _, err := adapters.BuildRawSelect(f, "t")
	require.NoError(t, err)
	assert.Contains(t, sel, "1=0", "a hostile sort key alone did not fail the query closed")
	assert.NotContains(t, sel, "`1`", "the hostile identifier still reached the statement")
	assert.NotContains(t, sel, "select 1", "the hostile projection still reached the statement")
	assert.Contains(t, sel, "`title`", "a clean projection entry was dropped along with the bad one")
	assert.Contains(t, sel, "`median_price`", "a clean sort column was dropped along with the bad one")

	// The two positions differ in whether the prune sticks, and that is core's
	// asymmetry rather than this plugin's choice. SetSelectFields under
	// inFinalize writes the RENDER only, and finalizeClauses re-derives the
	// render from selectFieldsAsked on every Build — so widening the policy and
	// rebuilding gets the caller's full request back. The sort has no such
	// "asked" copy, so a finalizer's SetSort is the instance's sort from then
	// on; FieldsPlugin's sort pruning behaves identically.
	assert.Equal(t, map[string]bool{"title": true}, f.GetSelectFields(),
		"this render's projection")
	require.NotNil(t, f.GetSort())
	assert.Equal(t, []figo.OrderByColumn{{Name: "median_price", Desc: true}}, f.GetSort().Columns)

	// Removing the cause restores the projection, which is what non-sticky
	// means; the sort stays pruned.
	f.SetSelectFields("title")
	f.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "median_price", Desc: true}}})
	f.Build(adapters.RawAdapter{})
	sel, _, err = adapters.BuildRawSelect(f, "t")
	require.NoError(t, err)
	assert.NotContains(t, sel, "1=0", "the instance stayed poisoned after the cause was removed")
	assert.Contains(t, sel, "ORDER BY `median_price` DESC")
}

// ---------------------------------------------------------------------------
// the families
// ---------------------------------------------------------------------------

func TestHostileIdentifiersAreRefused(t *testing.T) {
	cases := []struct {
		dsl  string
		want string // substring of the message that names the position
	}{
		// literals in the field position — the reported family
		{`1=1`, `filter field "1"`},
		{`0=0`, `filter field "0"`},
		{`id=5 or 1=1`, `filter field "1"`},
		{`not 1=1`, `filter field "1"`},
		{`1<null>`, `filter field "1"`},
		{`1<notnull>`, `filter field "1"`},
		{`1<in>[1,2]`, `filter field "1"`},
		{`1<nin>[1,2]`, `filter field "1"`},

		// directive adjacency — how the reported `or` disappeared
		{`sort=x:desc or 1=1`, `filter field "1"`},
		{`page=skip:0,take:50 or 1=1`, `filter field "1"`},

		// unrecognized <op>: the field absorbs "<word" and the operator
		// silently becomes ">". `price<>500` is an ordinary SQL habit.
		{`price<>500`, `filter field "price<"`},
		{`x<foo>1`, `filter field "x<foo"`},
		{`a<like>%x%`, `filter field "a<like"`},

		// comment / terminator / quote bytes are legal identifier characters
		{`x--=1`, `filter field "x--"`},
		{`x;=1`, `filter field "x;"`},
		{`x/*y*/=1`, `filter field "x/*y*/"`},
		{"`x`=1", "filter field \"`x`\""},
		{`x'y=1`, `filter field "x'y"`},

		// sort keys
		{`sort=1:desc`, `sort key "1"`},
		{`sort=x--:desc`, `sort key "x--"`},
		{`sort=x;drop:desc`, `sort key "x;drop"`},
		{`sort=a:asc,1:desc`, `sort key "1"`},

		// preloads: relation name and condition alike
		{`load=[Rel:1=1]`, `filter field "1"`},
		{`load=[1:id=1]`, `preload relation "1"`},
		{"load=[Rel`x:id=1]", "preload relation \"Rel`x\""},
		{`load=[Rel--:id=1]`, `preload relation "Rel--"`},

		// invisible and bidi characters
		{"a​b=1", `filter field`},
		{"‮abc=1", `filter field`},

		// dotted-name abuse (four segments is db.schema.table.column and is
		// legal; five is not)
		{`a.b.c.d.e=1`, `dot-separated segments`},
	}

	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), tc.dsl)
			require.Error(t, err, "accepted %q", tc.dsl)
			assert.Contains(t, err.Error(), tc.want)
			assert.Equal(t, "1=0", where, "%q did not fail closed", tc.dsl)
		})
	}
}

// Ordinary queries must be completely untouched — a guard that costs its user
// legitimate filters gets switched off.
func TestLegitimateDSLPassesThrough(t *testing.T) {
	cases := []struct {
		dsl   string
		where string
	}{
		{`median_price=500`, "`median_price` = ?"},
		{`city="tehran" and title="x"`, "(`city` = ? AND `title` = ?)"},
		{`orders.total>=100`, "`orders`.`total` >= ?"},
		{`_internal=1`, "`_internal` = ?"},
		{`col$1=1`, "`col$1` = ?"},
		{`a<in>[1,2,3]`, "`a` IN (?,?,?)"},
		{`a<bet>[1..9]`, "`a` BETWEEN ? AND ?"},
		{`deleted_at<null>`, "`deleted_at` IS NULL"},
		// Real column names the first cut of the rule wrongly refused.
		{`2fa_enabled=true`, "`2fa_enabled` = ?"},
		{`$amount>0`, "`$amount` > ?"},
		{`db.schema.tbl.col=1`, "`db`.`schema`.`tbl`.`col` = ?"},
		{`id<in>[1,2,]`, "`id` IN (?,?)"},
		{`id=1 load=[Orders:]`, "`id` = ?"},
		{`page=skip:0,take:50 and sort=median_price:desc`, ""},
		{`load=[Orders:id=1]`, ""},
		{`city="tehran" and sort=median_price:desc`, "`city` = ?"},
	}

	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), tc.dsl)
			assert.NoError(t, err, "wrongly refused %q", tc.dsl)
			assert.Equal(t, tc.where, where)
		})
	}
}

// A dropped conjunct widens the query, so "figo could not parse part of this"
// is a rejection by default. BuildE reports these already; what it does not do
// is fail closed, and a caller checking only AddFiltersFromString sees nothing.
func TestParseDiagnosticsAreRejectedByDefault(t *testing.T) {
	for _, dsl := range []string{
		`a b=1`,                    // renders WHERE `b` = ?, the `a` silently gone
		`id=1 union select 1`,      // renders WHERE `id` = ?
		`id=1; drop table users`,   // renders WHERE `id` = ? with args ["1;"]
		`id=1 or`,                  // dangling connector dropped
		`sort=median_price:desc--`, // direction silently flips desc -> asc
	} {
		t.Run(dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), dsl)
			require.Error(t, err, "accepted %q", dsl)
			assert.Contains(t, err.Error(), "parse:")
			assert.Equal(t, "1=0", where)
		})
	}

	// Off, the partial render is what figo would have produced on its own.
	g := NewInjectionGuardPlugin().RejectParseDiagnostics(false)
	err, where := guarded(t, g, `a b=1`)
	assert.NoError(t, err)
	assert.Equal(t, "`b` = ?", where)
}

// ---------------------------------------------------------------------------
// the allowlist
// ---------------------------------------------------------------------------

// Syntax screening stops nonsense; only the allowlist stops probing for a
// column that merely does not exist on THIS table.
func TestAllowFieldsRejectsUnknownColumns(t *testing.T) {
	g := NewInjectionGuardPlugin().AllowFields("median_price", "city")

	err, where := guarded(t, g, `is_admin=1`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the allowed field list")
	assert.Equal(t, "1=0", where)

	err, where = guarded(t, g, `median_price>=100 and city="tehran"`)
	assert.NoError(t, err)
	assert.Equal(t, "(`median_price` >= ? AND `city` = ?)", where)

	// The allowlist covers the sort position too, which is where the reported
	// DSL smuggled its payload in.
	err, _ = guarded(t, g, `sort=is_admin:desc`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `sort key "is_admin"`)

	// A qualified spelling is accepted by its bare column.
	err, _ = guarded(t, g, `orders.city="tehran"`)
	assert.NoError(t, err)
}

// A ScopePlugin's mandatory clause is injected by the SERVER, so it must not
// have to appear in an allowlist meant for untrusted input. Screening the
// rendered clause list against the allowlist would pill every query on a
// tenant-scoped instance, and the only escape would be to allowlist the very
// column the caller must never be able to name.
func TestAllowFieldsDoesNotRejectAnInjectedScope(t *testing.T) {
	for _, guardFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("guardFirst=%v", guardFirst), func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			g := NewInjectionGuardPlugin().AllowFields("city")
			sp := NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: 7})

			if guardFirst {
				require.NoError(t, f.RegisterPlugin(g))
				require.NoError(t, f.RegisterPlugin(sp))
			} else {
				require.NoError(t, f.RegisterPlugin(sp))
				require.NoError(t, f.RegisterPlugin(g))
			}

			require.NoError(t, f.AddFiltersFromString(`city="tehran"`))
			f.Build(adapters.RawAdapter{})
			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Contains(t, where, "`city` = ?")
			assert.Contains(t, where, "`tenant_id` = ?",
				"the guard's allowlist pilled the server's own scope")
			assert.NotContains(t, where, "1=0")

			// The caller still cannot name tenant_id itself.
			f2 := figo.New()
			f2.SetNamingFunc(figo.NoChangeNaming)
			require.NoError(t, f2.RegisterPlugin(NewInjectionGuardPlugin().AllowFields("city")))
			require.Error(t, f2.AddFiltersFromString(`tenant_id=9`))
		})
	}
}

// The registration spelling need not match the DSL's: the parser converts
// names on the way in, and it is the converted name that reaches SQL.
func TestAllowFieldsMatchesTheNamingConvertedSpelling(t *testing.T) {
	g := NewInjectionGuardPlugin().AllowFields("median_price")

	f := figo.New() // default SnakeCaseNaming
	require.NoError(t, f.RegisterPlugin(g))
	assert.NoError(t, f.AddFiltersFromString(`medianPrice=1`))
}

// ---------------------------------------------------------------------------
// unicode
// ---------------------------------------------------------------------------

func TestUnicodeIdentifiers(t *testing.T) {
	strict := NewInjectionGuardPlugin()
	err, _ := guarded(t, strict, `عنوان="x"`)
	require.Error(t, err, "the default rule is ASCII-only")

	loose := NewInjectionGuardPlugin().AllowUnicodeIdentifiers(true)
	err, where := guarded(t, loose, `عنوان="x"`)
	assert.NoError(t, err)
	assert.Equal(t, "`عنوان` = ?", where)

	// Still strict about what is not a letter: zero-width, bidi override and a
	// bare combining mark are refused in both modes.
	for _, dsl := range []string{"a​b=1", "‮abc=1", "́=1"} {
		err, where := guarded(t, loose, dsl)
		require.Error(t, err, "unicode mode accepted %q", dsl)
		assert.Equal(t, "1=0", where)
	}
}

// A refused name is never echoed raw into the message: printing a control byte
// or a bidi override into a log line attacks whatever reads the log.
func TestMessageDoesNotEchoInvisibleBytesRaw(t *testing.T) {
	err, _ := guarded(t, NewInjectionGuardPlugin(), "a‮b=1")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "‮", "the bidi override was echoed verbatim")
	assert.Contains(t, err.Error(), "U+202E")
}

// ---------------------------------------------------------------------------
// the latch
// ---------------------------------------------------------------------------

// The pill must stand on EVERY later build, not just the first. Consuming the
// latch on read would leave the second Build unfiltered — and the DSL has
// already been rolled away by then, so nothing else could notice.
func TestRefusalSurvivesRepeatedBuilds(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	require.Error(t, f.AddFiltersFromString(`1=1`))

	for i := 0; i < 3; i++ {
		f.Build(adapters.RawAdapter{})
		where, _, err := adapters.BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "1=0", where, "build %d widened", i+1)
	}
}

// One bad input must not close the instance forever: a DSL that parses
// acceptably reopens it, and that is the ONLY thing that does.
func TestAcceptedDSLReopensARefusedInstance(t *testing.T) {
	g := NewInjectionGuardPlugin()
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(g))

	require.Error(t, f.AddFiltersFromString(`1=1`))
	_, rejected := g.Rejected(f)
	assert.True(t, rejected)

	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	require.Equal(t, "1=0", where)

	require.NoError(t, f.AddFiltersFromString(`city="tehran"`))
	_, rejected = g.Rejected(f)
	assert.False(t, rejected)

	f.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`city` = ?", where)
}

// Clearing the filters must not reopen a refused instance. figo short-circuits
// a blank input before any hook runs, and "no filters" means an unfiltered
// query — so treating it as a reset would let a refused caller wipe its own
// refusal and get everything.
func TestClearingFiltersDoesNotReopenARefusedInstance(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	require.Error(t, f.AddFiltersFromString(`1=1`))

	require.NoError(t, f.AddFiltersFromString(""))
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// A rejection latched on one instance must not pill another — Clone shares the
// plugin manager, so a plugin holding a single flag would poison the whole
// template.
func TestLatchIsPerInstance(t *testing.T) {
	g := NewInjectionGuardPlugin()

	bad := figo.New()
	bad.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, bad.RegisterPlugin(g))
	require.Error(t, bad.AddFiltersFromString(`1=1`))

	good := figo.New()
	good.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, good.RegisterPlugin(g))
	require.NoError(t, good.AddFiltersFromString(`city="tehran"`))

	good.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(good)
	require.NoError(t, err)
	assert.Equal(t, "`city` = ?", where)

	bad.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(bad)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// The latch table is bounded, and an instance that keeps being built is
// promoted out of the older generation rather than falling off the end.
func TestLatchTableIsBounded(t *testing.T) {
	g := NewInjectionGuardPlugin().SetMaxLatched(8)

	live := figo.New()
	live.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, live.RegisterPlugin(g))
	require.Error(t, live.AddFiltersFromString(`1=1`))

	for i := 0; i < 64; i++ {
		other := figo.New()
		other.SetNamingFunc(figo.NoChangeNaming)
		require.NoError(t, other.RegisterPlugin(g))
		require.Error(t, other.AddFiltersFromString(`1=1`))

		// Touching the live instance every round keeps it promoted.
		live.Build(adapters.RawAdapter{})
	}

	g.lmu.Lock()
	total := len(g.cur) + len(g.prev)
	g.lmu.Unlock()
	assert.LessOrEqual(t, total, 16, "latch table grew past 2*maxLatched")

	live.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(live)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where, "an in-use instance was evicted")
}

// ---------------------------------------------------------------------------
// composition with the other plugins
// ---------------------------------------------------------------------------

// pruneExprFields deletes a logical node with no operands, so a pill installed
// through FilterExpr or AddFilter is laundered into a filter-less
// match-everything query the moment a FieldsPlugin is registered. Installing
// it from FinalizeClauses — after every ExprFilter has run — is what makes it
// stick.
func TestPillSurvivesFieldsPluginPruning(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))

	fp := NewFieldsPlugin()
	fp.SetAllowedFields("city")
	fp.EnableFieldWhitelist()
	require.NoError(t, f.RegisterPlugin(fp))

	require.Error(t, f.AddFiltersFromString(`1=1`))
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where, "a registered FieldsPlugin laundered the refusal")
}

// A ScopePlugin's mandatory clause is appended after the pill. AND-ing a
// tenant filter onto a never-true clause is still never-true, so the two
// controls compose in either registration order.
func TestScopePluginComposesWithTheRefusal(t *testing.T) {
	for _, guardFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("guardFirst=%v", guardFirst), func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			g := NewInjectionGuardPlugin()
			sp := NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: 7})

			if guardFirst {
				require.NoError(t, f.RegisterPlugin(g))
				require.NoError(t, f.RegisterPlugin(sp))
			} else {
				require.NoError(t, f.RegisterPlugin(sp))
				require.NoError(t, f.RegisterPlugin(g))
			}

			require.Error(t, f.AddFiltersFromString(`1=1`))
			f.Build(adapters.RawAdapter{})
			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Contains(t, where, "1=0")
			assert.NotContains(t, where, "`1`")
		})
	}
}

// ---------------------------------------------------------------------------
// routes other than the DSL
// ---------------------------------------------------------------------------

// AddFilter and Walk bypass the parser entirely, so the stateless screen in
// FinalizeClauses — not the latch — is what closes them.
func TestProgrammaticHostileNamesFailClosed(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))

	f.AddFilter(figo.EqExpr{Field: "x`; drop", Value: 1})
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)

	// A hostile name nested inside a logical node is reached too — the screen
	// walks the tree rather than looking at top-level clauses.
	f2 := figo.New()
	f2.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f2.RegisterPlugin(NewInjectionGuardPlugin()))
	f2.AddFilter(figo.OrExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "city", Value: "tehran"},
		figo.AndExpr{Operands: []figo.Expr{figo.EqExpr{Field: "x--", Value: 1}}},
	}})
	f2.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f2)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// Walk is the one route the guard cannot cover, and this pins it so the
// boundary cannot shift unnoticed.
//
// Walk edits the built tree in place and is documented as FINAL: it re-enters
// neither the naming strategy nor plugin field policy, and the next Build
// re-parses the DSL and throws its edits away. So a name written through Walk
// is only ever rendered by rendering WITHOUT a Build in between — and no
// plugin hook runs on that path, because finalizers run inside Build. A
// visitor that retargets a column is trusted application code by construction;
// this is the same contract that lets Walk point a filter at a column a
// FieldsPlugin ignore list forbids.
func TestWalkBypassesTheGuardByDesign(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	require.NoError(t, f.AddFiltersFromString(`city="tehran"`))
	f.Build(adapters.RawAdapter{})

	f.Walk(func(n figo.Expr) {
		if _, ok := figo.NodeField(n); ok {
			figo.SetNodeField(n, "1")
		}
	})

	// Rendered directly, with no Build to run the finalizer.
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`1` = ?", where,
		"Walk now goes through a hook — the guard should cover this route")

	// The moment a Build happens, the guard is back in charge (here the DSL is
	// re-parsed, so the Walk edit is gone and the clean filter returns).
	f.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`city` = ?", where)
}

// CustomExpr is exempt by default: its handler is given the field verbatim and
// owns its own quoting, which is why both SQL adapters exempt it too.
func TestCustomExprIsExemptUnlessAsked(t *testing.T) {
	handler := func(field, _ string, value any) (string, []any, error) {
		return field + " = ?", []any{value}, nil
	}
	expr := figo.CustomExpr{Field: "ST_Distance(geom, point)", Operator: "custom", Value: 1, Handler: handler}

	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	f.AddFilter(expr)
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Contains(t, where, "ST_Distance")

	f2 := figo.New()
	f2.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f2.RegisterPlugin(NewInjectionGuardPlugin().ScreenCustomExpr(true)))
	f2.AddFilter(expr)
	f2.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f2)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// ---------------------------------------------------------------------------
// the standalone checker and concurrency
// ---------------------------------------------------------------------------

func TestCheckDSLNeedsNoInstance(t *testing.T) {
	g := NewInjectionGuardPlugin()

	vs := g.CheckDSL(`sort=median_price:desc or 1=1`, figo.NoChangeNaming)
	require.Len(t, vs, 1)
	assert.Equal(t, PositionFilterField, vs[0].Position)
	assert.Equal(t, "1", vs[0].Name)

	assert.Empty(t, g.CheckDSL(`median_price>=100`, figo.NoChangeNaming))

	// A nil naming func falls back to figo's default rather than panicking.
	assert.Empty(t, g.CheckDSL(`medianPrice=1`, nil))
}

func TestGuardIsRaceFree(t *testing.T) {
	g := NewInjectionGuardPlugin().AllowFields("city")

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			if err := f.RegisterPlugin(g); err != nil {
				t.Error(err)
				return
			}
			dsl := `city="tehran"`
			want := "`city` = ?"
			if i%2 == 0 {
				dsl = `1=1`
				want = "1=0"
			}
			_ = f.AddFiltersFromString(dsl)
			f.Build(adapters.RawAdapter{})
			where, _, err := adapters.BuildRawWhere(f)
			if err != nil {
				t.Error(err)
				return
			}
			if where != want {
				t.Errorf("goroutine %d: got %q want %q", i, where, want)
			}
		}(i)
	}
	wg.Wait()
}

// The message itemises every distinct problem and repeats none of them.
func TestViolationsAreItemisedAndDeduped(t *testing.T) {
	err, _ := guarded(t, NewInjectionGuardPlugin(), `1=1 or 1=1 or 2=2`)
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), `filter field "1"`), "a repeated field was reported twice")
	assert.Contains(t, err.Error(), `filter field "2"`)
	assert.Contains(t, err.Error(), "rejected 2 identifier(s)")
}

// ---------------------------------------------------------------------------
// regressions from the adversarial pass
// ---------------------------------------------------------------------------

// Clone() escaping the refusal was the worst defect the attack found: the
// latch is keyed by instance identity and the rejected DSL has already been
// rolled away, so a clone had neither the verdict nor the evidence and
// rendered a COMPLETELY UNFILTERED query. Registering the guard turned a
// statement the engine bounces into a full table scan that succeeds — the
// precise outcome the fail-closed half exists to prevent.
func TestCloneOfARefusedInstanceIsRefusedToo(t *testing.T) {
	for _, cloneAfterBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("cloneAfterBuild=%v", cloneAfterBuild), func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
			require.Error(t, f.AddFiltersFromString(
				`page=skip:0,take:50 and sort=median_price:desc or 1=1`))

			if cloneAfterBuild {
				f.Build(adapters.RawAdapter{})
			}
			c := f.Clone()
			c.Build(adapters.RawAdapter{})

			where, _, err := adapters.BuildRawWhere(c)
			require.NoError(t, err)
			assert.Equal(t, "1=0", where, "the clone escaped the refusal and is unfiltered")
		})
	}
}

// A FieldsPlugin policy must not defeat the clone coverage. The first attempt
// at this fix carried the refusal by appending a never-true clause through
// AddFilter — which runs registered ExprFilters, and FieldsPlugin's pruning
// deletes a logical node with no operands, so merely HAVING an ignore list
// configured (on a field the query never mentions) put the clone back to
// completely unfiltered. Keying the latch by the plugin manager, which a clone
// shares, does not care what any other plugin does.
func TestCloneStaysRefusedUnderAFieldsPluginPolicy(t *testing.T) {
	for _, policy := range []string{"ignore", "whitelist", "both"} {
		t.Run(policy, func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))

			fp := NewFieldsPlugin()
			if policy == "ignore" || policy == "both" {
				fp.AddIgnoreFields("secret") // a field the DSL never mentions
			}
			if policy == "whitelist" || policy == "both" {
				fp.SetAllowedFields("city")
				fp.EnableFieldWhitelist()
			}
			require.NoError(t, f.RegisterPlugin(fp))

			require.Error(t, f.AddFiltersFromString(`city="x" or 1=1`))
			c := f.Clone()
			c.Build(adapters.RawAdapter{})

			where, _, err := adapters.BuildRawWhere(c)
			require.NoError(t, err)
			assert.Equal(t, "1=0", where, "a FieldsPlugin policy let the clone escape")
		})
	}
}

// A clone of an instance that had already built from an EARLIER DSL. The
// rollback restores that earlier DSL, so the clone re-parses it rather than
// inheriting anything the rejection left in the clause list — which is exactly
// why the refusal cannot be carried as a clause.
func TestCloneOfAPreviouslyBuiltInstanceIsRefusedToo(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))

	require.NoError(t, f.AddFiltersFromString(`id>0 load=[Orders:id=1]`))
	f.Build(adapters.RawAdapter{})
	require.Error(t, f.AddFiltersFromString(`city="x" or 1=1 load=[Orders:1=1]`))

	c := f.Clone()
	c.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(c)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where,
		"the clone ran the previously accepted query instead of the refusal")
}

// Taking the guard OFF a refused instance no longer reopens it. The refusal
// lives in the instance's own clause state, installed by core, so it does not
// depend on the plugin still being registered — which matters because no plugin
// can defend against its own removal mid-request.
func TestRefusalSurvivesUnregisteringTheGuard(t *testing.T) {
	g := NewInjectionGuardPlugin()
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(g))
	require.Error(t, f.AddFiltersFromString(`1=1`))

	require.NoError(t, f.UnregisterPlugin(g.Name()))
	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)

	// Swapping the whole plugin manager out does not reopen it either.
	f2 := figo.New()
	f2.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f2.RegisterPlugin(NewInjectionGuardPlugin()))
	require.Error(t, f2.AddFiltersFromString(`1=1`))
	f2.SetPluginManager(figo.NewPluginManager())
	f2.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f2)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// The projection is the one screened position figo converts at RENDER time,
// so screening the stored spelling was wrong in both directions.
func TestSelectFieldsAreScreenedInTheRenderedSpelling(t *testing.T) {
	// Bypass direction: the stored name is clean, the rendered one is not.
	f := figo.New()
	f.SetNamingFunc(func(s string) string { return "1" })
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	f.SetSelectFields("name")
	require.Error(t, f.AddFiltersFromString(`id=1`))
	f.Build(adapters.RawAdapter{})

	sel, _, err := adapters.BuildRawSelect(f, "t")
	require.NoError(t, err)
	assert.NotContains(t, sel, "`1`",
		"a naming func produced the reported identifier straight into the SELECT list")

	// False-positive direction: the stored name is kebab-case, which figo's own
	// default naming func is documented to accept, and it renders as first_name.
	f2 := figo.New() // default SnakeCaseNaming
	require.NoError(t, f2.RegisterPlugin(NewInjectionGuardPlugin()))
	f2.SetSelectFields("first-name")
	require.NoError(t, f2.AddFiltersFromString(`id=1`))
	f2.Build(adapters.RawAdapter{})

	sel, _, err = adapters.BuildRawSelect(f2, "t")
	require.NoError(t, err)
	assert.Contains(t, sel, "`first_name`")
	assert.NotContains(t, sel, "1=0")
}

// AllowFields promised to match the naming-converted spelling and did not, so
// the doc's own example refused 100% of traffic. Both directions now work, and
// so does a custom naming func.
func TestAllowFieldsMatchesBothSpellings(t *testing.T) {
	cases := []struct {
		name       string
		register   []string
		naming     figo.NamingFunc
		dsl        string
		wantAccept bool
	}{
		{"doc example: registration converted", []string{"City"}, figo.SnakeCaseNaming, `city="tehran"`, true},
		{"struct field names", []string{"MedianPrice", "City"}, figo.SnakeCaseNaming, `median_price>100 and city="x"`, true},
		{"input converted", []string{"median_price"}, figo.SnakeCaseNaming, `medianPrice=1`, true},
		{"custom naming func", []string{"city"}, func(s string) string { return strings.ToUpper(s) }, `city="x"`, true},
		{"qualified input, bare registration", []string{"city"}, figo.NoChangeNaming, `users.city="x"`, true},
		{"still refuses an unlisted column", []string{"city"}, figo.NoChangeNaming, `is_admin=1`, false},
		{"still refuses a hostile one", []string{"city"}, figo.NoChangeNaming, `1=1`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(tc.naming)
			require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin().AllowFields(tc.register...)))
			err := f.AddFiltersFromString(tc.dsl)
			if tc.wantAccept {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// figo's diagnostic channel is not purely a "the query was altered" channel.
// Refusing documented, behaviour-neutral input was the fastest route to this
// plugin being switched off, so those two diagnostics are informational and
// never a rejection — while everything that really does drop a conjunct still
// is. The exact strings are pinned: if the core rewords one, this test breaks
// rather than the guard silently tightening.
func TestInformationalDiagnosticsAreNotRejections(t *testing.T) {
	for _, tc := range []struct {
		dsl   string
		where string
	}{
		{`id<in>[1,2,]`, "`id` IN (?,?)"}, // README: a blank list part is skipped
		{`id<in>[1,,2]`, "`id` IN (?,?)"}, //
		{`status<nin>["a",,"b"]`, "`status` NOT IN (?,?)"},
		{`id=1 load=[Orders:]`, "`id` = ?"}, // a first-class core case
	} {
		t.Run(tc.dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), tc.dsl)
			assert.NoError(t, err, "informational diagnostic became a rejection")
			assert.Equal(t, tc.where, where)
		})
	}

	assert.True(t, informationalDiagnostic(
		`empty element in list "[1,2,]" ignored (write "" for an explicit empty string)`))
	assert.True(t, informationalDiagnostic(
		`load=[Orders:...] filter "" produced no conditions; relation preloaded unfiltered`))
	assert.False(t, informationalDiagnostic(`unrecognized token "a" (no operator found)`),
		"a dropped conjunct must stay a rejection")
}

// Real schemas hold names the first cut of the rule refused. The load-bearing
// clause is "no segment is only digits" — the reported bug's exact shape —
// not "no segment starts with a digit".
func TestIdentifierRuleMatchesRealSchemas(t *testing.T) {
	accepted := []string{
		`2fa_enabled=true`, `$amount>0`, `col$1=1`, `_internal=1`, `__x=1`,
		`select=1`, `order=1`, `group=1`, `MixedCase=1`, `db.schema.tbl.col=1`,
	}
	for _, dsl := range accepted {
		err, _ := guarded(t, NewInjectionGuardPlugin(), dsl)
		assert.NoError(t, err, "wrongly refused %q", dsl)
	}

	refused := []string{`1=1`, `0=0`, `42=1`, `a.0.b=1`, `a.b.c.d.e=1`}
	for _, dsl := range refused {
		err, where := guarded(t, NewInjectionGuardPlugin(), dsl)
		assert.Error(t, err, "accepted %q", dsl)
		assert.Equal(t, "1=0", where)
	}
}

// Document-store field names are not SQL columns. One recipe relaxes the rule
// for them, and the escape hatch cannot be used to disable the screen.
func TestDocumentStoreRelaxation(t *testing.T) {
	g := NewInjectionGuardPlugin().
		AllowExtraRunes("-@").
		AllowNumericSegments(true).
		SetMaxSegments(8)

	for _, dsl := range []string{
		`@timestamp>"2024-01-01"`, `user-agent="curl"`,
		`meta.tags.0="x"`, `profile.address.geo.lat=1`,
	} {
		err, _ := guarded(t, g, dsl)
		assert.NoError(t, err, "relaxed mode wrongly refused %q", dsl)
	}

	// The hard floor holds however the guard is configured: a rune that can
	// terminate or comment a statement is refused even if listed.
	hard := NewInjectionGuardPlugin().AllowExtraRunes("`\";'")
	for _, dsl := range []string{"`x`=1", `x;=1`, `x'y=1`} {
		err, where := guarded(t, hard, dsl)
		assert.Error(t, err, "AllowExtraRunes disabled the screen for %q", dsl)
		assert.Equal(t, "1=0", where)
	}
}

// The length cap is MySQL's, which counts characters, not bytes — and it is
// AllowUnicodeIdentifiers mode where the difference bites.
func TestSegmentLengthIsCountedInRunes(t *testing.T) {
	g := NewInjectionGuardPlugin().AllowUnicodeIdentifiers(true)

	err, _ := guarded(t, g, strings.Repeat("名", 22)+`=1`) // 22 runes, 66 bytes
	assert.NoError(t, err, "a 22-character column name was refused as 66 bytes")

	err, _ = guarded(t, g, strings.Repeat("名", 65)+`=1`) // 65 runes
	assert.Error(t, err, "the cap is not being enforced at all")
}

// figo.Walk visits an already-pointer node without recursing, so every
// identifier under a pointer logical node was invisible to the screen.
func TestPointerLogicalNodesAreScreened(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	g := NewInjectionGuardPlugin()
	require.NoError(t, f.RegisterPlugin(g))

	f.AddFilter(&figo.AndExpr{Operands: []figo.Expr{figo.EqExpr{Field: "1", Value: 1}}})
	vs, rejected := g.Rejected(f)
	assert.True(t, rejected, "a pointer logical node hid its whole subtree")
	require.NotEmpty(t, vs)
	assert.Equal(t, "1", vs[0].Name)
}

// The programmatic path has no error channel — AddFilter returns nothing and a
// clause finalizer cannot fail a Build — so a refusal there is silent by
// construction. Rejected is the signal, and it answers without a latch.
func TestRejectedReportsTheProgrammaticPath(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	g := NewInjectionGuardPlugin()
	require.NoError(t, f.RegisterPlugin(g))

	_, rejected := g.Rejected(f)
	assert.False(t, rejected, "a clean instance reported a rejection")

	f.AddFilter(figo.EqExpr{Field: "x--", Value: 1})
	vs, rejected := g.Rejected(f)
	require.True(t, rejected)
	assert.Equal(t, PositionFilterField, vs[0].Position)
	assert.Equal(t, "x--", vs[0].Name)

	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// A refused instance holds exactly one never-true clause however many times it
// is refused, so a pooled instance — or a client retrying — cannot be made to
// accumulate state on input the attacker chooses.
func TestRepeatedRefusalsDoNotAccumulateState(t *testing.T) {
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))

	for i := 0; i < 500; i++ {
		require.Error(t, f.AddFiltersFromString(fmt.Sprintf(`%d=1`, i)))
	}
	assert.Equal(t, []figo.Expr{figo.OrExpr{}}, f.GetClauses())

	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where)
}

// Release drops the recorded REASON, not the refusal. Reopening an instance is
// core's business and takes a DSL that parses; if Release reopened it, a caller
// could hand an attacker's refused request straight back to the database.
func TestReleaseClearsTheReasonNotTheRefusal(t *testing.T) {
	g := NewInjectionGuardPlugin()
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.RegisterPlugin(g))

	require.Error(t, f.AddFiltersFromString(`1=1`))
	g.Release(f)

	f.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where, "Release reopened a refused instance")

	require.NoError(t, f.AddFiltersFromString(`city="x"`))
	f.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`city` = ?", where)
}

// The hole that killed two earlier designs: a sibling clone parsing a CLEAN
// DSL must not reopen a refused sibling. With the refusal held per instance by
// core there is no shared cell to clear.
func TestSiblingCleanParseDoesNotReopenARefusedInstance(t *testing.T) {
	tmpl := figo.New()
	tmpl.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, tmpl.RegisterPlugin(NewInjectionGuardPlugin()))

	hostile := tmpl.Clone()
	clean := tmpl.Clone()

	require.Error(t, hostile.AddFiltersFromString(`city="berlin" or 1=1`))
	require.NoError(t, clean.AddFiltersFromString(`city="berlin"`))

	hostile.Build(adapters.RawAdapter{})
	where, _, err := adapters.BuildRawWhere(hostile)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where, "a sibling's clean parse reopened the refused instance")

	clean.Build(adapters.RawAdapter{})
	where, _, err = adapters.BuildRawWhere(clean)
	require.NoError(t, err)
	assert.Equal(t, "`city` = ?", where, "a sibling's refusal closed an innocent query")
}

// The screen must be LINEAR in the size of the expression tree.
//
// An earlier cut handled pointer logical nodes with a *AndExpr arm in the Walk
// visitor. figo.Walk's value arm recurses into the operands and THEN hands the
// visitor a pointer to the copy, so that arm fired for every ordinary value
// node too and re-walked the subtree it had just screened: 3*2^d-2 screen
// calls for depth d. A 321-byte query of plain nested parentheses — well
// inside what dslResourceGuard admits and what the complexity suite pins as
// supported — took 79 seconds and 16 GB, on input the guard ACCEPTS. That is a
// remote denial of service reachable from legitimate DSL, so it is pinned by
// wall clock rather than by shape.
func TestScreeningIsLinearInTreeDepth(t *testing.T) {
	const depth = 40 // exponential cost here would be ~2^40 screen calls

	var b strings.Builder
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "f%d=%d and (", i, i)
	}
	b.WriteString("z=1")
	b.WriteString(strings.Repeat(")", depth))
	dsl := b.String()

	done := make(chan string, 1)
	go func() {
		f := figo.New()
		f.SetNamingFunc(figo.NoChangeNaming)
		if err := f.RegisterPlugin(NewInjectionGuardPlugin()); err != nil {
			done <- "register: " + err.Error()
			return
		}
		if err := f.AddFiltersFromString(dsl); err != nil {
			done <- "refused a legitimate nested query: " + err.Error()
			return
		}
		f.Build(adapters.RawAdapter{})
		f.Build(adapters.RawAdapter{}) // the render screen runs on EVERY Build
		where, _, err := adapters.BuildRawWhere(f)
		if err != nil {
			done <- "render: " + err.Error()
			return
		}
		done <- "ok:" + where
	}()

	select {
	case got := <-done:
		require.True(t, strings.HasPrefix(got, "ok:"), got)
		assert.Contains(t, got, "`z` = ?")
		assert.NotContains(t, got, "1=0")
	case <-time.After(10 * time.Second):
		t.Fatal("screening a 40-deep nested query did not finish in 10s — the walk is superlinear again")
	}

	// The pointer nodes the removed arms existed for are still screened.
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	g := NewInjectionGuardPlugin()
	require.NoError(t, f.RegisterPlugin(g))
	f.AddFilter(&figo.AndExpr{Operands: []figo.Expr{
		&figo.OrExpr{Operands: []figo.Expr{
			figo.EqExpr{Field: "city", Value: "x"},
			&figo.NotExpr{Operands: []figo.Expr{figo.EqExpr{Field: "1", Value: 1}}},
		}},
	}})
	_, rejected := g.Rejected(f)
	assert.True(t, rejected, "a hostile leaf under nested pointer nodes was not reached")
}

// A panic while screening must not widen the query. The probe parse runs the
// APPLICATION'S naming func over attacker-chosen field names, so a naming func
// that panics on some spelling panics inside this hook — and a panic escaping
// AddFiltersFromString would take core's refusal with it.
func TestPanicWhileScreeningFailsClosed(t *testing.T) {
	boom := func(s string) string {
		parts := strings.Split(s, "_")
		return parts[0] + strings.ToUpper(parts[1][:1]) + parts[1][1:] // panics with no '_'
	}

	f := figo.New()
	f.SetNamingFunc(boom)
	g := NewInjectionGuardPlugin()
	require.NoError(t, f.RegisterPlugin(g))

	err := f.AddFiltersFromString(`city="berlin"`)
	require.Error(t, err, "the panic escaped instead of becoming a refusal")
	assert.Contains(t, err.Error(), "panic while screening")

	f.Build(adapters.RawAdapter{})
	where, _, buildErr := adapters.BuildRawWhere(f)
	require.NoError(t, buildErr)
	assert.Equal(t, "1=0", where, "a panic in the guard left the query unfiltered")

	vs, rejected := g.Rejected(f)
	assert.True(t, rejected)
	assert.Equal(t, PositionParse, vs[0].Position)
}
