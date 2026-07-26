package adapters

import (
	"database/sql"
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// R1 — buildByConditions must validate and render only the segments the caller
// asked for. Rendering all of them up front let an input owned by ONE segment
// fail a render that never requested it, and because figo.GetSqlString has no
// error channel the failure reached the caller as "" — which, spliced into
// "SELECT * FROM items " + <WHERE segment>, executed UNFILTERED.
// ---------------------------------------------------------------------------

// TestH8_R1_WhereSegmentSurvivesUnrelatedBadInput executes the composed
// statement against SQLite: the scoped rows must come back, not every row.
func TestH8_R1_WhereSegmentSurvivesUnrelatedBadInput(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items(id INTEGER, tenant TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]any{{1, "A"}, {2, "A"}, {3, "B"}, {4, "B"}, {5, "B"}, {6, "B"}} {
		if _, err := db.Exec(`INSERT INTO items VALUES(?,?)`, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(stmt string) []int {
		rows, err := db.Query(stmt)
		if err != nil {
			t.Fatalf("query %q: %v", stmt, err)
		}
		defer rows.Close()
		out := []int{}
		for rows.Next() {
			var id int
			var tn string
			if err := rows.Scan(&id, &tn); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}

	cases := []struct {
		name string
		dsl  string
		ctx  any
	}{
		// The only trigger the caller controls: it asked for WHERE, so it never
		// supplied a table name (the FROM segment is the only consumer).
		{"no table name", `tenant="A"`, RawContext{}},
		// DSL-supplied, i.e. untrusted: an all-dots sort key is consumed only by
		// the ORDER BY segment.
		{"dsl all-dot sort key", `tenant="A" sort=...:asc`, RawContext{Table: "items"}},
		{"dsl single-dot sort key", `tenant="A" sort=.:asc`, RawContext{Table: "items"}},
		{"both at once", `tenant="A" sort=...:asc`, RawContext{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := figo.New()
			if err := f.AddFiltersFromString(c.dsl); err != nil {
				t.Fatalf("AddFiltersFromString: %v", err)
			}
			f.Build(RawAdapter{Dialect: SQLiteDialect})

			frag, ok := RawAdapter{Dialect: SQLiteDialect}.GetSqlString(f, c.ctx, "WHERE")
			if !ok {
				t.Fatalf("WHERE segment render failed (ok=false) because of an input no requested segment consumes")
			}
			if frag == "" {
				t.Fatalf("WHERE segment is empty; a caller splicing it runs unfiltered")
			}
			got := ids("SELECT * FROM items " + frag)
			if len(got) != 2 || got[0] != 1 || got[1] != 2 {
				t.Fatalf("composed statement %q returned %v, want the 2 scoped rows [1 2]", frag, got)
			}
		})
	}
}

// TestH8_R1_RequestedSegmentStillFailsClosed is the other half: scoping the
// validation must NOT reopen M11 / A10-A7-5 / A10-A7-6. When the caller DOES
// ask for the segment that owns the bad input, the render must still fail.
func TestH8_R1_RequestedSegmentStillFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		dsl  string
		sel  []string
		ctx  any
		segs []string
	}{
		{"empty select field via SELECT", `a=1`, []string{""}, RawContext{Table: "t"}, []string{"SELECT", "WHERE"}},
		{"control byte select field via SELECT", `a=1`, []string{"a\x00b"}, RawContext{Table: "t"}, []string{"SELECT"}},
		{"empty table via FROM", `a=1`, nil, RawContext{}, []string{"FROM", "WHERE"}},
		{"control byte table via FROM", `a=1`, nil, RawContext{Table: "t\x00x"}, []string{"FROM"}},
		{"all-dot sort key via ORDER BY", `a=1 sort=...:asc`, nil, RawContext{Table: "t"}, []string{"ORDER BY", "WHERE"}},
		{"all-dot sort key via SORT alias", `a=1 sort=...:asc`, nil, RawContext{Table: "t"}, []string{"SORT"}},
		{"all-dot sort key via ORDERBY alias", `a=1 sort=...:asc`, nil, RawContext{Table: "t"}, []string{"ORDERBY"}},
		{"unrecognized segment keyword", `a=1`, nil, RawContext{Table: "t"}, []string{"WHERE", "WHRE"}},
		// The full SELECT consumes every input, so all of them still fail there.
		{"full select, bad sort key", `a=1 sort=...:asc`, nil, RawContext{Table: "t"}, nil},
		{"full select, bad select field", `a=1`, []string{""}, RawContext{Table: "t"}, nil},
		{"full select, no table", `a=1`, nil, RawContext{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, d := range []*SQLDialect{MySQLDialect, PostgresDialect, SQLiteDialect} {
				f := figo.New()
				if err := f.AddFiltersFromString(c.dsl); err != nil {
					t.Fatalf("AddFiltersFromString: %v", err)
				}
				if len(c.sel) > 0 {
					f.SetSelectFields(c.sel...)
				}
				f.Build(RawAdapter{Dialect: d})
				a := RawAdapter{Dialect: d}
				if s, ok := a.GetSqlString(f, c.ctx, c.segs...); ok {
					t.Fatalf("%s: render succeeded with %q; the segment owning the bad input must fail closed", d.Name, s)
				}
				if q, ok := a.GetQuery(f, c.ctx, c.segs...); ok || q != nil {
					t.Fatalf("%s: GetQuery succeeded (%v); must fail closed", d.Name, q)
				}
			}
		})
	}
}

// TestH8_R1_UnrelatedSegmentsUnchanged pins the segments that were already
// right, so scoping cannot quietly drop one.
func TestH8_R1_UnrelatedSegmentsUnchanged(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`a=1 sort=age:desc page=2 take=5`); err != nil {
		t.Fatal(err)
	}
	f.SetSelectFields("name", "age")
	f.Build(RawAdapter{Dialect: MySQLDialect})
	a := RawAdapter{Dialect: MySQLDialect}

	cases := []struct {
		segs []string
		want string
	}{
		{[]string{"SELECT"}, "SELECT `age`, `name`"},
		{[]string{"FROM"}, "FROM `t`"},
		{[]string{"WHERE"}, "WHERE (`a` = 1 AND `take` = 5)"},
		{[]string{"ORDER BY"}, "ORDER BY `age` DESC"},
		{[]string{"SORT"}, "ORDER BY `age` DESC"},
		{[]string{"LIMIT"}, "LIMIT 20"},
		{[]string{"PAGE"}, "LIMIT 20"},
		{[]string{"JOIN", "WHERE"}, "WHERE (`a` = 1 AND `take` = 5)"},
		{[]string{"GROUP BY", "WHERE"}, "WHERE (`a` = 1 AND `take` = 5)"},
		{[]string{"SELECT", "FROM", "WHERE", "ORDER BY", "LIMIT"},
			"SELECT `age`, `name` FROM `t` WHERE (`a` = 1 AND `take` = 5) ORDER BY `age` DESC LIMIT 20"},
	}
	for _, c := range cases {
		got, ok := a.GetSqlString(f, RawContext{Table: "t"}, c.segs...)
		if !ok || got != c.want {
			t.Fatalf("segments %v: got (%q, %v), want (%q, true)", c.segs, got, ok, c.want)
		}
	}
}

// TestH8_R1_ValidateIdentAlwaysFailsClosed sweeps every unrenderable identifier
// through every tree position on every render entry point. An identifier the
// dialect cannot quote into executable SQL must produce an ERROR — never a
// fragment with the offending predicate quietly removed, and never a non-empty
// fragment carrying the unrenderable name with ok=true. This is the guard for
// the tightening ef28331 introduced (A10/A7-5, A10/A7-6): scoping it per
// segment (R1) must not have opened a hole anywhere else.
func TestH8_R1_ValidateIdentAlwaysFailsClosed(t *testing.T) {
	// Non-empty names only: a leaf with an EMPTY Field is deliberately left to
	// the per-case rendering (see the comment in exprToSQL — an unsupported
	// expression type also reports an empty field and must keep its own,
	// clearer error). That gap is pre-existing and unchanged here.
	bad := []string{"a\x00b", "...", ".", "a..b", "a\x1fb", "a\x7fb", "a\nb"}
	for _, name := range bad {
		trees := map[string]figo.Expr{
			"leaf":      figo.EqExpr{Field: name, Value: 1},
			"and":       figo.AndExpr{Operands: []figo.Expr{figo.EqExpr{Field: "ok", Value: 1}, figo.EqExpr{Field: name, Value: 2}}},
			"or":        figo.OrExpr{Operands: []figo.Expr{figo.EqExpr{Field: "ok", Value: 1}, figo.EqExpr{Field: name, Value: 2}}},
			"not":       figo.NotExpr{Operands: []figo.Expr{figo.EqExpr{Field: name, Value: 2}}},
			"nested x3": figo.AndExpr{Operands: []figo.Expr{figo.OrExpr{Operands: []figo.Expr{figo.NotExpr{Operands: []figo.Expr{figo.EqExpr{Field: name, Value: 2}}}}}}},
			"in":        figo.InExpr{Field: name, Values: []any{1, 2}},
			"between":   figo.BetweenExpr{Field: name, Low: 1, High: 2},
			"isnull":    figo.IsNullExpr{Field: name},
			"like":      figo.LikeExpr{Field: name, Value: "x"},
		}
		for label, e := range trees {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			f.AddFilter(figo.EqExpr{Field: "scope", Value: 7})
			f.AddFilter(e)
			f.Build(RawAdapter{})

			if _, _, err := BuildRawWhere(f); err == nil {
				t.Fatalf("%q/%s: BuildRawWhere succeeded", name, label)
			}
			if _, _, err := BuildRawSelect(f, "t"); err == nil {
				t.Fatalf("%q/%s: BuildRawSelect succeeded", name, label)
			}
			a := RawAdapter{}
			for _, segs := range [][]string{nil, {"WHERE"}, {"SELECT", "FROM", "WHERE"}, {"LIKE"}} {
				if s, ok := a.GetSqlString(f, RawContext{Table: "t"}, segs...); ok {
					t.Fatalf("%q/%s segs=%v: GetSqlString ok=true, sql=%q", name, label, segs, s)
				}
				if q, ok := a.GetQuery(f, RawContext{Table: "t"}, segs...); ok {
					t.Fatalf("%q/%s segs=%v: GetQuery ok=true, q=%v", name, label, segs, q)
				}
			}
		}

		// A sort key is consumed only by the ORDER BY segment, so a WHERE-only
		// render legitimately succeeds (R1) — but every render that DOES emit
		// the ORDER BY must fail closed.
		f := figo.New()
		f.SetNamingFunc(figo.NoChangeNaming)
		f.AddFilter(figo.EqExpr{Field: "scope", Value: 7})
		f.AddFilter(figo.OrderBy{Columns: []figo.OrderByColumn{{Name: name}}})
		f.Build(RawAdapter{})
		a := RawAdapter{}
		if _, _, err := BuildRawSelect(f, "t"); err == nil {
			t.Fatalf("%q/sort: BuildRawSelect succeeded", name)
		}
		for _, segs := range [][]string{nil, {"ORDER BY"}, {"SORT"}, {"WHERE", "ORDER BY"}} {
			if s, ok := a.GetSqlString(f, RawContext{Table: "t"}, segs...); ok {
				t.Fatalf("%q/sort segs=%v: GetSqlString ok=true, sql=%q", name, segs, s)
			}
		}
		if s, ok := a.GetSqlString(f, RawContext{Table: "t"}, "WHERE"); !ok || s != "WHERE `scope` = 7" {
			t.Fatalf("%q/sort WHERE-only: got (%q, %v), want the predicate", name, s, ok)
		}
	}
}

// ---------------------------------------------------------------------------
// R4 — expandSliceArgs was the one placeholder scanner without hunt #6 L4's
// backslash handling, so it disagreed with expandPlaceholders about where a
// MySQL string literal ends: the slice was left unexpanded and handed to
// database/sql (which cannot bind it) while the debug SQL showed an expanded
// IN list.
// ---------------------------------------------------------------------------

func TestH8_R4_ExpandSliceArgsHonoursBackslashEscape(t *testing.T) {
	handler := func(frag string) func(string, string, any) (string, []any, error) {
		return func(field, op string, val any) (string, []any, error) {
			return frag, []any{[]any{1, 2, 3}}, nil
		}
	}
	cases := []struct {
		name        string
		dialect     *SQLDialect
		frag        string
		wantSQL     string
		wantArgs    int
		wantDisplay string
	}{
		{
			name: "control: no backslash", dialect: MySQLDialect,
			frag:    `note = 'x' AND id IN (?)`,
			wantSQL: "(note = 'x' AND id IN (?,?,?))", wantArgs: 3,
			wantDisplay: "(note = 'x' AND id IN (1,2,3))",
		},
		{
			name: "MySQL backslash-escaped quote inside the literal", dialect: MySQLDialect,
			frag:    `note = '\'' AND id IN (?)`,
			wantSQL: `(note = '\'' AND id IN (?,?,?))`, wantArgs: 3,
			wantDisplay: `(note = '\'' AND id IN (1,2,3))`,
		},
		{
			name: "two escaped quotes", dialect: MySQLDialect,
			frag:    `note = '\'\'' AND id IN (?)`,
			wantSQL: `(note = '\'\'' AND id IN (?,?,?))`, wantArgs: 3,
			wantDisplay: `(note = '\'\'' AND id IN (1,2,3))`,
		},
		{
			// On SQLite/Postgres a backslash is NOT an escape, so '\' closes the
			// literal and the following "' AND id IN (" opens a new one: the '?'
			// is literal text. Both scanners must reach that SAME conclusion —
			// the fix is dialect-gated, so this case is untouched by it.
			name: "SQLite: backslash is not an escape", dialect: SQLiteDialect,
			frag:    `note = '\'' AND id IN (?)`,
			wantSQL: `(note = '\'' AND id IN (?))`, wantArgs: 1,
			wantDisplay: `(note = '\'' AND id IN (?))`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := figo.New()
			f.AddFilter(figo.CustomExpr{Field: "id", Operator: "custom", Handler: handler(c.frag)})
			f.Build(RawAdapter{Dialect: c.dialect})
			gotSQL, gotArgs, err := buildWhereFromExprs(c.dialect, f.GetClauses())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if gotSQL != c.wantSQL {
				t.Fatalf("sql = %q, want %q", gotSQL, c.wantSQL)
			}
			if len(gotArgs) != c.wantArgs {
				t.Fatalf("args = %#v, want %d of them", gotArgs, c.wantArgs)
			}
			// The two scanners must agree about whether that '?' is a
			// placeholder: expandPlaceholders is the backslash-aware reference,
			// so its expansion of the fragment expandSliceArgs produced must
			// leave nothing dangling and must not have substituted a value into
			// a '?' that expandSliceArgs decided was literal text.
			if display := expandPlaceholders(c.dialect, gotSQL, gotArgs); display != c.wantDisplay {
				t.Fatalf("display = %q, want %q (the two scanners disagree about the literal)", display, c.wantDisplay)
			}
			// The bind path must never hand a slice to database/sql.
			for i, a := range gotArgs {
				if _, isSlice := sliceArgLen(a); isSlice && c.dialect.EscapeBackslash {
					t.Fatalf("arg %d is still a slice (%#v); database/sql cannot bind it", i, a)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// I3 — docExprSupport omitted ArrayOverlapsExpr, so the one type the helper
// exists to qualify lost its hint while its sibling kept it.
// ---------------------------------------------------------------------------

func TestH8_I3_DocExprSupportCoversEveryAdvancedType(t *testing.T) {
	const hint = " (rendered by the MongoDB/Elasticsearch adapters only)"
	// Every advanced type below has a case arm in BOTH mongo.go's exprToMongo
	// and elasticsearch.go's exprToES.
	rendered := []figo.Expr{
		figo.JsonPathExpr{Field: "meta", Path: "$.a", Value: 1},
		figo.ArrayContainsExpr{Field: "tags", Values: []any{"a"}},
		figo.ArrayOverlapsExpr{Field: "tags", Values: []any{"a", "b"}},
		figo.FullTextSearchExpr{Field: "body", Query: "x"},
		figo.GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 3},
	}
	for _, e := range rendered {
		if got := docExprSupport(e); got != hint {
			t.Fatalf("%T: docExprSupport = %q, want %q", e, got, hint)
		}
		if err := errUnsupportedExpr(e); !strings.Contains(err.Error(), hint) {
			t.Fatalf("%T: error %q is missing the adapter hint", e, err)
		}
	}
	// A type no adapter renders must NOT get a false hint (the reason the
	// helper was introduced).
	for _, e := range []figo.Expr{&figo.EqExpr{Field: "a", Value: 1}} {
		if got := docExprSupport(e); got != "" {
			t.Fatalf("%T: docExprSupport = %q, want no hint", e, got)
		}
	}
}

// ---------------------------------------------------------------------------
// G3 (raw half) — normalizeColumnName converted the WHOLE dotted string while
// the core converts per '.'-separated segment, so a qualified projection name
// was spelled differently from the same name in the WHERE clause.
// ---------------------------------------------------------------------------

func TestH8_G3Raw_QualifiedProjectionMatchesWhereSpelling(t *testing.T) {
	// AddFilter is the route the core normalizes per '.'-separated segment
	// (figo.go normalizeFieldName), and it is the contract the projection must
	// match. Note the input is ALREADY snake_case in every segment, so a
	// per-segment conversion is the identity and no naming intent is at stake.
	f := figo.New() // default SnakeCaseNaming
	f.AddFilter(figo.EqExpr{Field: "k8_users.age", Value: 30})
	f.SetSelectFields("k8_users.age")
	f.Build(RawAdapter{Dialect: MySQLDialect})

	got, ok := RawAdapter{Dialect: MySQLDialect}.GetSqlString(f, RawContext{Table: "k8_users"},
		"SELECT", "FROM", "WHERE")
	if !ok {
		t.Fatal("render failed")
	}
	const want = "SELECT `k8_users`.`age` FROM `k8_users` WHERE `k8_users`.`age` = 30"
	if got != want {
		t.Fatalf("got  %q\nwant %q\n(the projection and the WHERE must spell one column one way)", got, want)
	}

	// The naming func must still run on each segment.
	f2 := figo.New()
	f2.AddFilter(figo.EqExpr{Field: "users.firstName", Value: "x"})
	f2.SetSelectFields("users.firstName")
	f2.Build(RawAdapter{Dialect: MySQLDialect})
	got2, _ := RawAdapter{Dialect: MySQLDialect}.GetSqlString(f2, RawContext{Table: "users"}, "SELECT", "WHERE")
	if got2 != "SELECT `users`.`first_name` WHERE `users`.`first_name` = 'x'" {
		t.Fatalf("per-segment naming not applied: %q", got2)
	}

	// A single-segment name is untouched by the change.
	f3 := figo.New()
	if err := f3.AddFiltersFromString(`firstName="x"`); err != nil {
		t.Fatal(err)
	}
	f3.SetSelectFields("firstName")
	f3.Build(RawAdapter{Dialect: MySQLDialect})
	got3, _ := RawAdapter{Dialect: MySQLDialect}.GetSqlString(f3, RawContext{Table: "users"}, "SELECT", "WHERE")
	if got3 != "SELECT `first_name` WHERE `first_name` = 'x'" {
		t.Fatalf("unqualified name changed: %q", got3)
	}

	// An empty dot segment must still reach validateIdent and fail closed
	// (A10/A7-6) rather than being turned into a name by the naming func.
	f4 := figo.New()
	f4.SetSelectFields("a..b")
	f4.Build(RawAdapter{Dialect: MySQLDialect})
	if s, ok := (RawAdapter{Dialect: MySQLDialect}).GetSqlString(f4, RawContext{Table: "t"}, "SELECT"); ok {
		t.Fatalf("projection with an empty dot segment rendered %q; must fail closed", s)
	}
}
