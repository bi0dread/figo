package figo_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// where builds the DSL with the raw adapter and returns the WHERE fragment,
// its bind args and the BuildE diagnostic.
func h7where(t *testing.T, dsl string) (string, []any, error) {
	t.Helper()
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	require.NoError(t, f.AddFiltersFromString(dsl))
	buildErr := f.BuildE(RawAdapter{})
	w, a, err := BuildRawWhere(f)
	require.NoError(t, err)
	return w, a, buildErr
}

// H5. A programmatic filter added AFTER the DSL was cleared must survive the
// next Build. `builtFromDSL` is instance-wide, so the wipe used to be
// retroactive and took the new clause with it — an unfiltered scan where the
// caller had just installed a tenant scope.
func TestH5_ProgrammaticFilterSurvivesClearedDSL(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(RawAdapter{})
	require.NoError(t, f.AddFiltersFromString(""))
	f.AddFilter(EqExpr{Field: "tenant_id", Value: 7})
	f.Build(RawAdapter{})

	w, args, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`tenant_id` = ?", w, "the filter added with no DSL set must not be wiped by the stale DSL state")
	assert.Equal(t, []any{7}, args)
	assert.Len(t, f.GetClauses(), 1)

	// The cleared DSL's own clauses are still gone.
	assert.NotContains(t, w, "`a`")
}

// H6. A keyword glued to a load= directive's closing ']' must not restart the
// bracket hunt past it and swallow the rest of the DSL.
func TestH6_LoadDirectiveGluedConnector(t *testing.T) {
	glued, gluedArgs, gluedErr := h7where(t, `load=[Orders:id=1]and tenant_id=7 and secret=false`)
	spaced, spacedArgs, spacedErr := h7where(t, `load=[Orders:id=1] and tenant_id=7 and secret=false`)

	assert.Equal(t, spaced, glued, "a glued connector must parse like a spaced one")
	assert.Equal(t, spacedArgs, gluedArgs)
	assert.NoError(t, gluedErr)
	assert.NoError(t, spacedErr)
	assert.Equal(t, "(`tenant_id` = ? AND `secret` = ?)", glued)
}

// H7. A value's closing ')' / ']' ends the token, exactly as a closing quote
// at depth 0 already did. Otherwise a glued connector was absorbed into the
// value, corrupting the bounds AND silently downgrading OR to AND.
func TestH7_ClosingParenAndBracketEndTheToken(t *testing.T) {
	t.Run("between then and", func(t *testing.T) {
		w, args, err := h7where(t, `price<bet>(10..20)and b=2`)
		assert.NoError(t, err)
		assert.Equal(t, "(`price` BETWEEN ? AND ? AND `b` = ?)", w)
		assert.Equal(t, []any{int64(10), int64(20), int64(2)}, args)
	})
	t.Run("between then or", func(t *testing.T) {
		w, args, err := h7where(t, `price<bet>(10..20)or x=1`)
		assert.NoError(t, err)
		assert.Contains(t, w, " OR ", "the glued OR must not degrade to AND")
		assert.Equal(t, []any{int64(10), int64(20), int64(1)}, args)
	})
	t.Run("in list then and", func(t *testing.T) {
		w, args, err := h7where(t, `age<in>[1,2]and x=1`)
		assert.NoError(t, err)
		assert.Equal(t, "(`age` IN (?,?) AND `x` = ?)", w)
		assert.Equal(t, []any{int64(1), int64(2), int64(1)}, args)
	})
	t.Run("grouping parens still work", func(t *testing.T) {
		w, args, err := h7where(t, `(a=1 and b<in>[1,2]) or c=3`)
		assert.NoError(t, err)
		assert.Equal(t, "((`a` = ? AND `b` IN (?,?)) OR `c` = ?)", w)
		assert.Equal(t, []any{int64(1), int64(1), int64(2), int64(3)}, args)
	})
	t.Run("quoted between bounds still work", func(t *testing.T) {
		w, args, err := h7where(t, `x<bet>("a".."b") and y=1`)
		assert.NoError(t, err)
		assert.Equal(t, "(`x` BETWEEN ? AND ? AND `y` = ?)", w)
		assert.Equal(t, []any{"a", "b", int64(1)}, args)
	})
}

// H8. <null>/<notnull> are self-delimiting: they take no value and end in '>',
// so a glued keyword must start a new token instead of being swallowed.
func TestH8_NullOperatorsAreSelfDelimiting(t *testing.T) {
	t.Run("glued or stays or", func(t *testing.T) {
		w, args, err := h7where(t, `deleted_at<null>or tenant_id=7`)
		assert.NoError(t, err)
		assert.Equal(t, "(`deleted_at` IS NULL OR `tenant_id` = ?)", w)
		assert.Equal(t, []any{int64(7)}, args)
	})
	t.Run("glued predicate is not dropped", func(t *testing.T) {
		w, _, err := h7where(t, `a<null>b=2 and c=3`)
		assert.NoError(t, err)
		assert.Contains(t, w, "`b` = ?", "the predicate glued after <null> must not vanish")
		assert.Contains(t, w, "`c` = ?")
	})
	t.Run("notnull too", func(t *testing.T) {
		w, _, err := h7where(t, `a<notnull>and b=2`)
		assert.NoError(t, err)
		assert.Equal(t, "(`a` IS NOT NULL AND `b` = ?)", w)
	})
	t.Run("spaced form unchanged", func(t *testing.T) {
		w, _, err := h7where(t, `a<null> or b=2`)
		assert.NoError(t, err)
		assert.Equal(t, "(`a` IS NULL OR `b` = ?)", w)
	})
}

// H15. A panicking AfterParse hook must not leave a DSL another plugin already
// rejected armed on the instance.
type h7PanicPlugin struct{ name string }

func (p h7PanicPlugin) Name() string                    { return p.name }
func (p h7PanicPlugin) Version() string                 { return "1" }
func (p h7PanicPlugin) Initialize(Figo) error           { return nil }
func (p h7PanicPlugin) BeforeQuery(Figo, any) error     { return nil }
func (p h7PanicPlugin) AfterQuery(Figo, any, any) error { return nil }
func (p h7PanicPlugin) BeforeParse(_ Figo, dsl string) (string, error) {
	return dsl, nil
}
func (p h7PanicPlugin) AfterParse(Figo, string) error { panic("observer blew up") }

type h7RejectPlugin struct{ name string }

func (p h7RejectPlugin) Name() string                    { return p.name }
func (p h7RejectPlugin) Version() string                 { return "1" }
func (p h7RejectPlugin) Initialize(Figo) error           { return nil }
func (p h7RejectPlugin) BeforeQuery(Figo, any) error     { return nil }
func (p h7RejectPlugin) AfterQuery(Figo, any, any) error { return nil }
func (p h7RejectPlugin) BeforeParse(_ Figo, dsl string) (string, error) {
	return dsl, nil
}
func (p h7RejectPlugin) AfterParse(Figo, string) error { return errors.New("rejected") }

func TestH15_PanickingAfterParseHookRollsBackTheRejectedDSL(t *testing.T) {
	f := New()
	require.NoError(t, f.RegisterPlugin(h7RejectPlugin{name: "rejector"}))
	require.NoError(t, f.RegisterPlugin(h7PanicPlugin{name: "panicker"}))

	func() {
		defer func() {
			assert.NotNil(t, recover(), "the plugin panic must still propagate")
		}()
		_ = f.AddFiltersFromString(`a=1 and b=2 and c=3`)
	}()

	assert.Equal(t, "", f.GetDSL(), "a DSL rejected by another plugin must not stay armed when a later hook panics")

	f.Build(RawAdapter{})
	w, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "", w, "the rejected DSL must not be buildable afterwards")
}

// M1. An unterminated quote swallows the rest of the DSL into one value and can
// WIDEN the query; the spaced form was diagnosed, the unspaced form was silent.
func TestM1_UnterminatedQuoteIsDiagnosed(t *testing.T) {
	_, _, err := h7where(t, `not name="x and tenant_id=5`)
	require.Error(t, err, "an unterminated quote must reach BuildE")
	assert.Contains(t, err.Error(), "unterminated")

	// A well-formed quoted value is still clean.
	_, _, ok := h7where(t, `name="x and y" and tenant_id=5`)
	assert.NoError(t, ok)
}

// M2. A partial or malformed page= directive must not claim the components of
// the caller's SetPage that it never set — Build has to stay idempotent.
func TestM2_PartialPageDirectiveKeepsSetPage(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 page=skip:5`))
		f.SetPage(0, 100)
		f.Build(RawAdapter{})
		first := f.GetPage()
		f.Build(RawAdapter{})
		assert.Equal(t, Page{Skip: 5, Take: 100}, first)
		assert.Equal(t, first, f.GetPage(), "Build must be idempotent")
	})
	t.Run("malformed", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 page=garbage`))
		f.SetPage(3, 100)
		f.Build(RawAdapter{})
		first := f.GetPage()
		f.Build(RawAdapter{})
		assert.Equal(t, Page{Skip: 3, Take: 100}, first, "a malformed page= must not take ownership")
		assert.Equal(t, first, f.GetPage())
	})
	t.Run("full directive still wins and resets", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 page=skip:2,take:7`))
		f.Build(RawAdapter{})
		assert.Equal(t, Page{Skip: 2, Take: 7}, f.GetPage())
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		f.Build(RawAdapter{})
		assert.Equal(t, Page{Skip: 0, Take: 0}, f.GetPage(), "a DSL-owned page resets when the DSL drops it")
	})
}

// M3. GetClauses/GetPreloads must hand out snapshots that cannot be written
// back through into the live instance.
func TestM3_SnapshotsAreDeepCopies(t *testing.T) {
	t.Run("clauses", func(t *testing.T) {
		f := New()
		f.AddFilter(InExpr{Field: "id", Values: []any{1, 2, 3}})
		f.Build(RawAdapter{})

		snap := f.GetClauses()
		snap[0].(InExpr).Values[0] = 999

		_, args, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, []any{1, 2, 3}, args, "a snapshot write must not reach the live instance")
	})
	t.Run("preloads", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[R:b<in>[1,2]]`))
		f.Build(RawAdapter{})

		snap := f.GetPreloads()
		snap["R"][0].(InExpr).Values[0] = 999

		assert.Equal(t, []any{int64(1), int64(2)}, f.GetPreloads()["R"][0].(InExpr).Values)
	})
}

// M12. A directive is not an operand, so the connector written next to it must
// be absorbed instead of reported as dangling — figo's own canonical examples
// were being rejected by callers following the documented BuildE contract.
func TestM12_DirectiveJoinedByConnectorIsNotDangling(t *testing.T) {
	cases := []struct {
		dsl   string
		where string
	}{
		{`load=[orders:first_name="x"] and first_name="y"`, "`first_name` = ?"},
		{`a=1 and sort=id:asc`, "`a` = ?"},
		{`sort=id:asc and a=1`, "`a` = ?"},
		{`a=1 and sort=id:asc and b=2`, "(`a` = ? AND `b` = ?)"},
		{`a=1 and page=skip:0,take:5`, "`a` = ?"},
		{`a=1 or sort=id:asc or b=2`, "(`a` = ? OR `b` = ?)"},
	}
	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			w, _, err := h7where(t, tc.dsl)
			assert.NoError(t, err, "valid DSL must not report a dangling connector")
			assert.Equal(t, tc.where, w)
		})
	}
}

// L5. `not` in front of a BARE directive must be dropped there, not carried
// onto the next predicate (round 4 fixed only the parenthesized form).
func TestL5_NotBeforeBareDirectiveDoesNotInvertNextPredicate(t *testing.T) {
	for _, dsl := range []string{
		`not sort=id:asc and a=1`,
		`not page=skip:1 and a=1`,
		`not load=[R:x=1] and a=1`,
	} {
		t.Run(dsl, func(t *testing.T) {
			w, args, err := h7where(t, dsl)
			assert.Equal(t, "`a` = ?", w, "the next predicate must not be inverted")
			assert.Equal(t, []any{int64(1)}, args)
			require.Error(t, err, "the dropped not must be reported")
			assert.Contains(t, err.Error(), "dropped 'not'")
		})
	}
}

// A7-1/A7-2/A7-5 (one root cause): <bet> never checked its HIGH bound, so an
// absent or malformed one bound a string that SQL and BSON order above every
// number — the range silently became unbounded, BuildE nil.
func TestA7_BetweenHighBoundIsValidated(t *testing.T) {
	rejected := []string{
		`balance<bet>(5..)`,
		`balance<bet>(5....100)`,
		`p<bet>(1...5)`,
		`price<bet>(10..20..30)`,
		`balance<bet>(10..abc)`,
	}
	for _, dsl := range rejected {
		t.Run("rejected "+dsl, func(t *testing.T) {
			w, args, err := h7where(t, dsl)
			require.Error(t, err, "an unusable <bet> bound must reach BuildE")
			assert.Contains(t, err.Error(), `for operator "<bet>"`)
			assert.Equal(t, "", w, "the widened range must not be rendered")
			assert.Empty(t, args)
		})
	}

	accepted := map[string][]any{
		`balance<bet>(5..100)`: {int64(5), int64(100)},
		`name<bet>(a..z)`:      {"a", "z"},
		`name<bet>("a".."z")`:  {"a", "z"},
		`p<bet>(1.5..2.5)`:     {1.5, 2.5},
		`p<bet>(-3..-1)`:       {int64(-3), int64(-1)},
	}
	for dsl, want := range accepted {
		t.Run("accepted "+dsl, func(t *testing.T) {
			w, args, err := h7where(t, dsl)
			assert.NoError(t, err)
			assert.Contains(t, w, "BETWEEN ? AND ?")
			assert.Equal(t, want, args)
		})
	}

	// The empty LOW bound was already rejected; the asymmetry is gone.
	_, _, err := h7where(t, `balance<bet>(..100)`)
	assert.Error(t, err)
}

// A7-3 (one root cause): a blank element in an <in>/<nin> list is a separator
// artifact, not a value. It used to be typed as "" and injected, so <in>
// silently gained rows and <nin> silently hid them.
func TestA7_ListLiteralSkipsBlankElements(t *testing.T) {
	cases := []struct {
		dsl     string
		want    []any
		diagnos bool
	}{
		{`name<in>["alice",]`, []any{"alice"}, true},
		{`name<nin>["a","b",]`, []any{"a", "b"}, true},
		{`price<in>[,10,20]`, []any{int64(10), int64(20)}, true},
		{`price<in>[10,,20]`, []any{int64(10), int64(20)}, true},
		{`name<in>["a", ,"b"]`, []any{"a", "b"}, true},
		// An explicit empty-string member is still expressible, and clean.
		{`name<in>["a",""]`, []any{"a", ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			_, args, err := h7where(t, tc.dsl)
			assert.Equal(t, tc.want, args)
			if !tc.diagnos {
				assert.NoError(t, err)
				return
			}
			// The skip is reported, so BuildE still means "the built query
			// does not express all of the input".
			require.Error(t, err)
			assert.Contains(t, err.Error(), "empty element in list")
		})
	}

	// A list of nothing but separators has no members at all, so it falls back
	// to the fail-closed 1=0 sentinel rather than matching empty-string rows.
	w, _, err := h7where(t, `price<in>[,]`)
	assert.Error(t, err)
	assert.Equal(t, "1=0", w)

	// Preloads inherit the fix.
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[R:b<in>[1,2,]]`))
	f.Build(RawAdapter{})
	assert.Equal(t, []any{int64(1), int64(2)}, f.GetPreloads()["R"][0].(InExpr).Values)
}

// A7-1 (value typing): Go's float grammar is not SQL/JSON's. Digit separators
// and hex floats leaked past the "oversized ints stay strings" invariant and
// degraded to a lossy float64, selecting a DIFFERENT row above 2^53.
func TestA7_GoOnlyFloatLiteralsStayStrings(t *testing.T) {
	for _, s := range []string{"1_000", "9_007_199_254_740_993", "0x1p4", "1_0e1_0", "9_223_372_036_854_775_808"} {
		assert.IsType(t, "", ParseValue(s), "%q must not become a float64", s)
		assert.Equal(t, s, ParseValue(s))
	}
	// Ordinary numeric literals are untouched.
	assert.Equal(t, int64(1000), ParseValue("1000"))
	assert.Equal(t, float64(1000), ParseValue("1e3"))
	assert.Equal(t, 1.5, ParseValue("1.5"))
	assert.Equal(t, int64(9007199254740993), ParseValue("9007199254740993"))
	// A month name containing 'p' must still reach the date parser.
	assert.IsType(t, ParseValue("2024-09-05"), ParseValue("September 5, 2024"))
}

// A3-3. One caller-supplied identifier must render as ONE identifier, whichever
// surface it arrived on. AddFilter and SetSort now convert on the way in, so
// there is exactly one conversion per entry and none at render.
func TestA3_3_ProgrammaticFieldsUseTheNamingFunc(t *testing.T) {
	build := func(f Figo) string {
		f.Build(RawAdapter{Dialect: SQLiteDialect})
		sql, _, err := BuildRawSelect(f, "people")
		require.NoError(t, err)
		return sql
	}

	prog := New()
	prog.SetNamingFunc(func(s string) string { return "t_" + s })
	prog.AddFilter(EqExpr{Field: "userName", Value: "ada"})
	prog.AddSelectFields("userName")
	prog.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "userName"}}})
	progSQL := build(prog)

	assert.Equal(t, 3, strings.Count(progSQL, `"t_userName"`), "one identifier on all three surfaces: %s", progSQL)
	assert.NotContains(t, progSQL, `"userName"`)

	// Default SnakeCaseNaming: the programmatic spelling now matches the DSL's.
	dsl := New()
	dsl.AddFiltersFromString(`userName="ada" sort=userName:asc`)
	dsl.AddSelectFields("userName")
	dslSQL := build(dsl)

	prog2 := New()
	prog2.AddFilter(EqExpr{Field: "userName", Value: "ada"})
	prog2.AddSelectFields("userName")
	prog2.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "userName"}}})
	assert.Equal(t, dslSQL, build(prog2), "programmatic and DSL input must render the same statement")
}

// Qualified (dotted) names stay qualified: naming applies per segment, so the
// documented Walk/SetNodeField qualification recipe keeps working.
func TestA3_3_QualifiedNamesConvertPerSegment(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "users.firstName", Value: "jo"})
	f.Build(RawAdapter{})
	w, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`users`.`first_name` = ?", w)
}

// A finalizer that PRUNES the sort re-sets already-converted columns; a
// non-idempotent naming func must not convert them a second time.
func TestA3_3_SetSortDoesNotDoubleConvertStoredColumns(t *testing.T) {
	f := New()
	f.SetNamingFunc(func(s string) string { return "t_" + s })
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "id"}, {Name: "name"}}})
	require.Equal(t, "t_id", f.GetSort().Columns[0].Name)

	// Re-set the surviving subset, as FieldsPlugin's enforceSort does.
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: "t_id"}}})
	assert.Equal(t, "t_id", f.GetSort().Columns[0].Name, "a stored column must not be converted twice")
}

// NormalizeExprFields is the exported binding point for expressions that reach
// an instance without passing AddFilter (e.g. a clause injected by a
// FinalizeClauses plugin).
func TestNormalizeExprFields(t *testing.T) {
	naming := func(s string) string { return "t_" + s }

	got := NormalizeExprFields(AndExpr{Operands: []Expr{
		EqExpr{Field: "tenantId", Value: 1},
		NotExpr{Operands: []Expr{InExpr{Field: "roleId", Values: []any{1, 2}}}},
		OrderBy{Columns: []OrderByColumn{{Name: "createdAt", Desc: true}}},
	}}, naming)

	and := got.(AndExpr)
	assert.Equal(t, "t_tenantId", and.Operands[0].(EqExpr).Field)
	assert.Equal(t, "t_roleId", and.Operands[1].(NotExpr).Operands[0].(InExpr).Field)
	assert.Equal(t, "t_createdAt", and.Operands[2].(OrderBy).Columns[0].Name)

	// nil naming is a no-op, and the input is not mutated.
	orig := EqExpr{Field: "x", Value: 1}
	assert.Equal(t, orig, NormalizeExprFields(orig, nil))
	_ = NormalizeExprFields(orig, naming)
	assert.Equal(t, "x", orig.Field)
}

// SetPageString silently normalized garbage on a programmatic API with no
// diagnostic channel at all. SetPageStringE reports what it could not use.
func TestSetPageStringE_ReportsDiscardedInput(t *testing.T) {
	cases := []struct {
		in      string
		want    Page
		wantErr string
	}{
		{"skip:1,take:5", Page{Skip: 1, Take: 5}, ""},
		{"skip:abc", Page{Skip: 0, Take: 0}, "invalid page value"},
		{"garbage", Page{Skip: 0, Take: 0}, "malformed page segment"},
		{"skip:5,take:7,extra:1", Page{Skip: 5, Take: 7}, "unknown page key"},
		{"skip:-4,take:-9", Page{Skip: 0, Take: 0}, "negative page value"},
		{"", Page{Skip: 0, Take: 0}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			f := New()
			err := f.SetPageStringE(tc.in)
			assert.Equal(t, tc.want, f.GetPage())
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// The compatibility wrapper still applies what parsed.
	f := New()
	f.SetPageString("skip:2,take:3")
	assert.Equal(t, Page{Skip: 2, Take: 3}, f.GetPage())
}

// A7-2. A predicate written `field <op>value` (space before the operator, none
// after) was dropped WHOLE — including a spaced `<null>` that merely happened
// to sit before ')'. Dropping a conjunct widens the query.
func TestA7_HalfSpacedOperatorIsNotDropped(t *testing.T) {
	cases := []struct {
		dsl   string
		where string
	}{
		{`(tenant_id=1 and tag <null>)`, "(`tenant_id` = ? AND `tag` IS NULL)"},
		{`tenant_id=1 and t <notnull>`, "(`tenant_id` = ? AND `t` IS NOT NULL)"},
		{`tenant_id=1 and price <in>[10,15]`, "(`tenant_id` = ? AND `price` IN (?,?))"},
		{`tenant_id=1 and price <bet>(1..2)`, "(`tenant_id` = ? AND `price` BETWEEN ? AND ?)"},
		{`tenant_id=1 and price =10`, "(`tenant_id` = ? AND `price` = ?)"},
		{`tenant_id=1 and price >1`, "(`tenant_id` = ? AND `price` > ?)"},
		{`tenant_id=1 and price !=1`, "(`tenant_id` = ? AND `price` != ?)"},
		{`tenant_id=1 and name =^"x"`, "(`tenant_id` = ? AND `name` LIKE ?)"},
		{`tenant_id=1 and name .=^"x"`, "(`tenant_id` = ? AND LOWER(`name`) LIKE LOWER(?))"},
		// Controls: the forms that already worked must be unchanged.
		{`tenant_id=1 and price = 10`, "(`tenant_id` = ? AND `price` = ?)"},
		{`tenant_id=1 and price=10`, "(`tenant_id` = ? AND `price` = ?)"},
		{`(tenant_id=1 and tag<null>)`, "(`tenant_id` = ? AND `tag` IS NULL)"},
	}
	for _, tc := range cases {
		t.Run(tc.dsl, func(t *testing.T) {
			w, _, err := h7where(t, tc.dsl)
			assert.NoError(t, err)
			assert.Equal(t, tc.where, w)
		})
	}

	// Two bare field names still have no operator between them.
	_, _, err := h7where(t, `a b`)
	assert.Error(t, err)
}

// A7-4. A repeated sort= keeps last-wins, but the discard must be reported —
// BuildE promises a nil error means the built query expresses all of the input.
func TestA7_RepeatedSortIsReported(t *testing.T) {
	_, _, err := h7where(t, `price>0 sort=tenant_id:asc sort=price:desc`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overrides an earlier sort=")

	// The equivalent single directive keeps both columns and stays clean.
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	require.NoError(t, f.AddFiltersFromString(`price>0 sort=tenant_id:asc,price:desc`))
	assert.NoError(t, f.BuildE(RawAdapter{}))
	require.NotNil(t, f.GetSort())
	assert.Len(t, f.GetSort().Columns, 2)
}

// A10-1. Nested load= made PARSING quadratic in time and in live heap, and no
// QueryLimits setting could bound it (limits run AfterParse, and the tree they
// measure is one clause). The content of a nested load= is discarded anyway, so
// it must be skipped without being parsed.
func TestA10_NestedLoadDoesNotRecurse(t *testing.T) {
	build := func(n int) time.Duration {
		dsl := "a=1 " + strings.Repeat("load=[T:", n) + "x=1" + strings.Repeat("]", n)
		start := time.Now()
		f := New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		_ = f.BuildE(RawAdapter{})
		return time.Since(start)
	}

	// Quadratic growth makes 4x the nesting cost ~16x the time; linear costs
	// ~4x. The generous ceiling below still fails hard on the old behaviour
	// (n=1000 was ~60ms, n=4000 ~700ms-2s; both are ~1ms now).
	small := build(1000)
	large := build(4000)
	assert.Less(t, large, 40*small+20*time.Millisecond,
		"nested load= must not be super-linear (n=1000 %v, n=4000 %v)", small, large)
	assert.Less(t, large, 200*time.Millisecond, "nested load= parse must stay cheap, got %v", large)

	// The known-good shape still parses, with the nested directive reported.
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=x load=[A:load=[B:z=1] and y=2]`))
	err := f.BuildE(RawAdapter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested load= inside load=")
	preloads := f.GetPreloads()
	assert.NotContains(t, preloads, "B")
	require.Len(t, preloads["A"], 1)
	assert.Equal(t, EqExpr{Field: "y", Value: int64(2)}, preloads["A"][0])
}
