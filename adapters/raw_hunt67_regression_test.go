package adapters

import (
	"database/sql"
	"runtime"
	"strings"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// rawTestDB opens an in-memory SQLite database seeded with two tables so a
// rendered statement can be EXECUTED, not merely string-compared: several of
// these defects only show up as wrong rows.
func rawTestDB(t *testing.T) *sql.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	require.NoError(t, err)
	d, err := g.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE items (id INTEGER, tenant TEXT, name TEXT)`)
	mustExec(t, d, `INSERT INTO items VALUES (1,'A','alice'),(2,'A','bob'),(3,'B','carol'),(4,'B','dave')`)
	mustExec(t, d, `CREATE TABLE orders (id INTEGER, status TEXT)`)
	mustExec(t, d, `INSERT INTO orders VALUES (10,'ok'),(11,'ok')`)
	return d
}

func mustExec(t *testing.T, d *sql.DB, stmt string) {
	t.Helper()
	_, err := d.Exec(stmt)
	require.NoError(t, err)
}

// queryIDs executes a rendered query and returns the id column of every row.
func queryIDs(t *testing.T, d *sql.DB, q figo.SQLQuery) []int64 {
	t.Helper()
	rows, err := d.Query(q.SQL, q.Args...)
	require.NoError(t, err, "statement did not execute: %s", q.SQL)
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		var tenant, name string
		require.NoError(t, rows.Scan(&id, &tenant, &name))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func sqliteQuery(t *testing.T, f figo.Figo) figo.SQLQuery {
	t.Helper()
	a := RawAdapter{Dialect: SQLiteDialect}
	q, ok := a.GetQuery(f, RawContext{Table: "items"})
	require.True(t, ok, "GetQuery failed")
	return q.(figo.SQLQuery)
}

// A1/A3-1: a CustomExpr handler fragment is spliced into the AND join, and
// SQL's AND binds tighter than OR — so a compound fragment re-associated the
// whole WHERE. With a mandatory scope filter that returns rows of another
// tenant.
func TestA3_1_CustomExprCompoundFragmentIsParenthesized(t *testing.T) {
	d := rawTestDB(t)
	anyOf := func() figo.CustomExpr {
		return figo.CustomExpr{Field: "id", Operator: "anyof",
			Handler: func(field, op string, v any) (string, []any, error) {
				return "id = ? OR id = ?", []any{int64(3), int64(4)}, nil
			}}
	}

	// scope(tenant='A') AND (id=3 OR id=4) -> no row (3 and 4 are tenant B).
	f := figo.New()
	f.AddFilter(figo.AndExpr{Operands: []figo.Expr{
		figo.EqExpr{Field: "tenant", Value: "A"},
		anyOf(),
	}})
	f.Build(RawAdapter{Dialect: SQLiteDialect})
	q := sqliteQuery(t, f)
	assert.Equal(t, `SELECT * FROM "items" WHERE ("tenant" = ? AND (id = ? OR id = ?)) LIMIT 20`, q.SQL)
	assert.Empty(t, queryIDs(t, d, q), "rows outside the tenant scope came back: %s", q.SQL)

	// Two compound fragments joined at the top level: also empty.
	f2 := figo.New()
	f2.AddFilter(figo.CustomExpr{Field: "id", Operator: "anyof",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "id = ? OR id = ?", []any{int64(1), int64(2)}, nil
		}})
	f2.AddFilter(anyOf())
	f2.Build(RawAdapter{Dialect: SQLiteDialect})
	q2 := sqliteQuery(t, f2)
	assert.Equal(t, `SELECT * FROM "items" WHERE (id = ? OR id = ?) AND (id = ? OR id = ?) LIMIT 20`, q2.SQL)
	assert.Empty(t, queryIDs(t, d, q2))

	// An empty fragment is still dropped entirely (no stray "()").
	f3 := figo.New()
	f3.AddFilter(figo.EqExpr{Field: "tenant", Value: "A"})
	f3.AddFilter(figo.CustomExpr{Field: "id", Operator: "noop",
		Handler: func(field, op string, v any) (string, []any, error) { return "", nil, nil }})
	f3.Build(RawAdapter{Dialect: SQLiteDialect})
	assert.Equal(t, `SELECT * FROM "items" WHERE "tenant" = ? LIMIT 20`, sqliteQuery(t, f3).SQL)
}

// H9: with a JOIN emitted next to an unqualified main WHERE, any column both
// tables have made the statement unexecutable ("ambiguous column name: id").
// H10: the JOIN had no key condition, so it multiplied the primary rows and a
// preload filter matching nothing emptied the main result.
func TestH9_H10_PreloadDoesNotAlterTheMainResultSet(t *testing.T) {
	d := rawTestDB(t)

	// H9: "id" exists on both tables.
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`id=1 load=[orders:status="ok"]`))
	f.Build(RawAdapter{Dialect: SQLiteDialect})
	q := sqliteQuery(t, f)
	assert.NotContains(t, q.SQL, "JOIN")
	assert.Equal(t, []int64{1}, queryIDs(t, d, q))

	// H10: a preload must neither multiply nor annihilate the primary rows.
	base := figo.New()
	require.NoError(t, base.AddFiltersFromString(`name<notnull>`))
	base.Build(RawAdapter{Dialect: SQLiteDialect})
	want := queryIDs(t, d, sqliteQuery(t, base))
	require.Len(t, want, 4)

	for _, dsl := range []string{
		`name<notnull> load=[orders:status="ok"]`,
		`name<notnull> load=[orders:status="nonexistent"]`,
		`name<notnull> load=[orders:status="ok"|items:id=1]`,
	} {
		fp := figo.New()
		require.NoError(t, fp.AddFiltersFromString(dsl))
		fp.Build(RawAdapter{Dialect: SQLiteDialect})
		got := sqliteQuery(t, fp)
		assert.NotContains(t, got.SQL, "JOIN", dsl)
		assert.Equal(t, want, queryIDs(t, d, got), "preload changed the primary row set: %s", dsl)
	}

	// The preload filters are still reachable for a caller's own second query.
	fp := figo.New()
	require.NoError(t, fp.AddFiltersFromString(`id=1 load=[orders:status="ok"]`))
	fp.Build(RawAdapter{Dialect: SQLiteDialect})
	pre, err := BuildRawPreloads(fp)
	require.NoError(t, err)
	assert.Equal(t, `"status" = ?`, pre["orders"].Where)
}

// H11: quoting must come from the RECEIVER's dialect, like placeholder
// numbering already did. An instance built with nil, with another adapter, or
// with a *RawAdapter holds no value RawAdapter, so quoting fell back to MySQL
// while the receiver numbered the binds: backticks with $n, valid on no engine.
// The parameterized form is essential here — GetSqlString inlines literals and
// hides the $n half of the mismatch.
func TestH11_DialectComesFromTheReceiver(t *testing.T) {
	recv := RawAdapter{Dialect: PostgresDialect}
	const want = `SELECT * FROM "t" WHERE ("id" = $1 AND "name" = $2) LIMIT 20`

	build := func(a figo.Adapter) figo.Figo {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(`id=1 and name="x"`))
		f.Build(a)
		return f
	}
	cases := map[string]figo.Figo{
		"built with nil":             build(nil),
		"built with GormAdapter":     build(GormAdapter{}),
		"built with *RawAdapter":     build(&RawAdapter{Dialect: PostgresDialect}),
		"built with value adapter":   build(RawAdapter{Dialect: PostgresDialect}),
		"built with a MySQL adapter": build(RawAdapter{}),
	}
	for name, f := range cases {
		q, ok := recv.GetQuery(f, RawContext{Table: "t"})
		require.True(t, ok, name)
		assert.Equal(t, want, q.(figo.SQLQuery).SQL, name)
		assert.NotContains(t, q.(figo.SQLQuery).SQL, "`", name)
	}

	// The receiver-less package helpers read the instance's adapter, and must
	// accept the pointer form too.
	fp := build(&RawAdapter{Dialect: PostgresDialect})
	where, _, err := BuildRawWhere(fp)
	require.NoError(t, err)
	assert.Equal(t, `("id" = $1 AND "name" = $2)`, where)
}

// M11: an empty select field rendered SELECT "" — rejected by Postgres/MySQL,
// a constant column on SQLite. It now fails closed like every other
// unrenderable input.
func TestM11_EmptySelectFieldFailsClosed(t *testing.T) {
	f := figo.New()
	f.AddSelectFields("")
	f.Build(RawAdapter{})

	sqlStr, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
	assert.False(t, ok)
	assert.Empty(t, sqlStr)

	_, _, err := BuildRawSelect(f, "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name segment")

	// Segment rendering takes the same path.
	_, okSeg := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"}, "SELECT", "FROM")
	assert.False(t, okSeg)
}

// L1: the debugging view must denote the same instant as the bound parameter.
func TestL1_TimeLiteralKeepsZoneAndFraction(t *testing.T) {
	tv := time.Date(2026, 7, 25, 10, 30, 0, 123456789, time.FixedZone("IST", 3*3600+1800))
	f := figo.New()
	f.AddFilter(figo.GtExpr{Field: "created_at", Value: tv})
	f.Build(RawAdapter{})

	sqlStr, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
	require.True(t, ok)
	assert.Contains(t, sqlStr, "'2026-07-25 10:30:00.123456789+03:30'")

	// A whole second in UTC keeps the offset and drops the empty fraction.
	f2 := figo.New()
	f2.AddFilter(figo.GtExpr{Field: "created_at", Value: time.Date(2026, 7, 25, 10, 30, 0, 0, time.UTC)})
	f2.Build(RawAdapter{})
	sql2, _ := RawAdapter{}.GetSqlString(f2, RawContext{Table: "t"})
	assert.Contains(t, sql2, "'2026-07-25 10:30:00+00:00'")
}

// L2: an unrecognized segment keyword was dropped in silence (so a typo
// returned a statement missing that clause with ok=true), and LIMIT/OFFSET/PAGE
// were not deduped ("LIMIT 10 LIMIT 10").
func TestL2_ConditionTypeIsValidatedAndDeduped(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`id=1 sort=id:desc page=skip:5,take:10`))
	f.Build(RawAdapter{})
	a := RawAdapter{}

	_, ok := a.GetSqlString(f, RawContext{Table: "t"}, "SELECT", "FROM", "WHRE")
	assert.False(t, ok, "a typo'd segment keyword must not silently drop the clause")

	_, _, err := AdapterRawGetSql(f, RawContext{Table: "t"}, "SELECT", "NOPE")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized conditionType")

	// "ORDERBY" is now an alias, the way "GROUPBY" already was.
	s, ok := a.GetSqlString(f, RawContext{Table: "t"}, "SELECT", "FROM", "ORDERBY")
	require.True(t, ok)
	assert.Equal(t, "SELECT * FROM `t` ORDER BY `id` DESC", s)

	for _, tc := range []struct {
		segs []string
		want string
	}{
		{[]string{"SELECT", "FROM", "LIMIT", "LIMIT"}, "SELECT * FROM `t` LIMIT 10"},
		{[]string{"SELECT", "FROM", "OFFSET", "OFFSET"}, "SELECT * FROM `t` OFFSET 5"},
		{[]string{"SELECT", "FROM", "PAGE", "PAGE"}, "SELECT * FROM `t` LIMIT 10 OFFSET 5"},
		{[]string{"SELECT", "FROM", "PAGE", "LIMIT"}, "SELECT * FROM `t` LIMIT 10 OFFSET 5"},
		{[]string{"SELECT", "FROM", "LIMIT", "OFFSET"}, "SELECT * FROM `t` LIMIT 10 OFFSET 5"},
	} {
		got, ok := a.GetSqlString(f, RawContext{Table: "t"}, tc.segs...)
		require.True(t, ok, tc.segs)
		assert.Equal(t, tc.want, got, tc.segs)
	}
}

// L3: naming normalization collapses distinct spellings onto one column; the
// dedup was keyed on the pre-normalization name, so the column was emitted once
// per spelling.
func TestL3_ProjectionIsDedupedAfterNormalization(t *testing.T) {
	f := figo.New()
	f.AddSelectFields("userName", "user_name", "UserName", "id")
	f.Build(RawAdapter{})

	s, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
	require.True(t, ok)
	assert.Equal(t, "SELECT `user_name`, `id` FROM `t` LIMIT 20", s)
	assert.Equal(t, 1, strings.Count(s, "`user_name`"))
}

// L4: on a dialect where '\' escapes inside a string literal, a fragment
// containing \' flipped both placeholder scanners out of the literal, so the
// following '?' was never expanded (display path) or renumbered.
func TestL4_BackslashEscapeKeepsPlaceholderScannersInSync(t *testing.T) {
	frag := func(field, op string, v any) (string, []any, error) {
		return `name = 'a\'b' AND id = ?`, []any{int64(7)}, nil
	}
	f := figo.New()
	f.AddFilter(figo.CustomExpr{Field: "name", Operator: "x", Handler: frag})
	f.Build(RawAdapter{}) // MySQL: backslash IS an escape

	s, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
	require.True(t, ok)
	assert.Contains(t, s, "id = 7", "the placeholder after \\' was not expanded: %s", s)
	assert.NotContains(t, s, "id = ?")

	// A custom numbered dialect that also escapes backslashes must number it.
	dial := *PostgresDialect
	dial.EscapeBackslash = true
	f2 := figo.New()
	f2.AddFilter(figo.CustomExpr{Field: "name", Operator: "x", Handler: frag})
	f2.Build(RawAdapter{Dialect: &dial})
	q, ok := RawAdapter{Dialect: &dial}.GetQuery(f2, RawContext{Table: "t"})
	require.True(t, ok)
	assert.Contains(t, q.(figo.SQLQuery).SQL, "id = $1")
}

// A1/A3-3: a top-level figo.OrderBy clause rendered nowhere on the raw adapter
// while the GORM adapter applied it — the same figo state came back in a
// different order, and with a LIMIT in force that means different rows.
func TestA3_3_TopLevelOrderByClauseIsRendered(t *testing.T) {
	d := rawTestDB(t)

	f := figo.New()
	f.AddFilter(figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "name", Desc: true}}})
	f.Build(RawAdapter{Dialect: SQLiteDialect})
	q := sqliteQuery(t, f)
	assert.Contains(t, q.SQL, `ORDER BY "name" DESC`)
	assert.Equal(t, []int64{4, 3, 2, 1}, queryIDs(t, d, q))

	// Clause-list columns first, then the instance's sort — the order
	// ApplyGorm applies them in.
	f2 := figo.New()
	f2.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "id"}}})
	f2.AddFilter(figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "tenant", Desc: true}}})
	f2.Build(RawAdapter{Dialect: SQLiteDialect})
	assert.Contains(t, sqliteQuery(t, f2).SQL, `ORDER BY "tenant" DESC, "id" ASC`)
}

// A1/A3-4: GORM's clause.Expr expands a slice arg bound to a '?' right after
// '(' — the raw adapter handed the slice to the driver, which cannot bind one,
// so the same handler that works on GORM died with "unsupported type".
func TestA3_4_CustomExprSliceArgExpands(t *testing.T) {
	d := rawTestDB(t)

	f := figo.New()
	f.AddFilter(figo.CustomExpr{Field: "id", Operator: "in",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "id IN (?)", []any{[]any{int64(1), int64(3)}}, nil
		}})
	f.Build(RawAdapter{Dialect: SQLiteDialect})
	q := sqliteQuery(t, f)
	assert.Equal(t, `SELECT * FROM "items" WHERE (id IN (?,?)) LIMIT 20`, q.SQL)
	assert.Equal(t, []any{int64(1), int64(3)}, q.Args)
	assert.Equal(t, []int64{1, 3}, queryIDs(t, d, q))

	// An empty list renders NULL (matches nothing) rather than a syntax error.
	fe := figo.New()
	fe.AddFilter(figo.CustomExpr{Field: "id", Operator: "in",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "id IN (?)", []any{[]any{}}, nil
		}})
	fe.Build(RawAdapter{Dialect: SQLiteDialect})
	qe := sqliteQuery(t, fe)
	assert.Equal(t, `SELECT * FROM "items" WHERE (id IN (NULL)) LIMIT 20`, qe.SQL)
	assert.Empty(t, queryIDs(t, d, qe))

	// Values a driver CAN bind are never expanded: []byte, and a '?' that does
	// not follow '(' keeps its single arg.
	fb := figo.New()
	fb.AddFilter(figo.CustomExpr{Field: "name", Operator: "eq",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "name = ? AND id IN (?)", []any{[]byte("alice"), []any{int64(1)}}, nil
		}})
	fb.Build(RawAdapter{Dialect: SQLiteDialect})
	qb := sqliteQuery(t, fb)
	assert.Equal(t, `SELECT * FROM "items" WHERE (name = ? AND id IN (?)) LIMIT 20`, qb.SQL)
	assert.Equal(t, []byte("alice"), qb.Args[0])

	// A scalar handler is untouched.
	fs := figo.New()
	fs.AddFilter(figo.CustomExpr{Field: "id", Operator: "gt",
		Handler: func(field, op string, v any) (string, []any, error) {
			return "id > ?", []any{int64(2)}, nil
		}})
	fs.Build(RawAdapter{Dialect: SQLiteDialect})
	assert.Equal(t, []int64{3, 4}, queryIDs(t, d, sqliteQuery(t, fs)))
}

// A10/A7-2: the round-4 flattening only covers same-connector runs. Rendering
// returned a fresh string per nesting level, so alternating connectors (and a
// not-chain) re-copied the accumulated fragment at every level: O(output x
// depth) time AND allocation — 373 MB to render 54 KB of SQL at n=4000.
func TestA7_2_NestedRenderIsLinear(t *testing.T) {
	render := func(dsl string) (time.Duration, float64, int) {
		f := figo.New()
		if err := f.AddFiltersFromString(dsl); err != nil {
			t.Fatalf("AddFiltersFromString: %v", err)
		}
		f.Build(RawAdapter{})
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		start := time.Now()
		s, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
		elapsed := time.Since(start)
		runtime.ReadMemStats(&m1)
		if !ok {
			t.Fatal("render failed")
		}
		return elapsed, float64(m1.TotalAlloc-m0.TotalAlloc) / 1e6, len(s)
	}

	altNest := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i%2 == 0 {
				b.WriteString("a=1 and (")
			} else {
				b.WriteString("a=1 or (")
			}
		}
		b.WriteString("z=1")
		b.WriteString(strings.Repeat(")", n))
		return b.String()
	}

	elapsed, allocMB, out := render(altNest(4000))
	// Bug: 96ms / 373 MB for 54 KB of SQL. Fixed: ~2.5ms / ~1 MB. The bound is
	// deliberately loose — it only has to separate linear from quadratic.
	if allocMB > 30 {
		t.Fatalf("rendering %d bytes of SQL allocated %.1f MB; the quadratic render is back", out, allocMB)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("alternating-connector render took %v", elapsed)
	}

	_, notAllocMB, notOut := render(strings.Repeat("not ", 4000) + "a=1")
	if notAllocMB > 30 {
		t.Fatalf("rendering a %d-byte not-chain allocated %.1f MB", notOut, notAllocMB)
	}
}

// A10/A7-5: a NUL byte in a DSL identifier went straight through quoting into
// the SQL text — the one byte of 0x00-0x7f that makes the statement
// unparseable — with BuildE nil and ok=true, so nothing upstream could tell.
func TestA7_5_ControlByteInIdentifierFailsClosed(t *testing.T) {
	for _, dsl := range []string{"a\x00b=1", "0\x000>0", "id=1 sort=a\x00b:asc"} {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		f.Build(RawAdapter{})
		s, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
		assert.False(t, ok, "dsl=%q rendered %q", dsl, s)
	}

	// The error names the offending byte.
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString("a\x00b=1"))
	f.Build(RawAdapter{})
	_, _, err := BuildRawWhere(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "control byte")

	// A table name and a projection column go through the same guard.
	f2 := figo.New()
	require.NoError(t, f2.AddFiltersFromString("id=1"))
	f2.Build(RawAdapter{})
	_, _, err = BuildRawSelect(f2, "t\x00b")
	require.Error(t, err)

	// Ordinary identifiers, including the quote rune, still render.
	f3 := figo.New()
	require.NoError(t, f3.AddFiltersFromString("`weird`=1"))
	f3.Build(RawAdapter{})
	_, ok := RawAdapter{}.GetSqlString(f3, RawContext{Table: "t"})
	assert.True(t, ok)
}

// A10/A7-6: an identifier made only of dots rendered empty quoted segments
// (“.“.“), a syntax error, on both the WHERE and the ORDER BY path — the
// ORDER BY guard tested the whole name rather than its segments.
func TestA7_6_AllDotsIdentifierFailsClosed(t *testing.T) {
	for _, dsl := range []string{"...<0", "id=1 sort=...:asc"} {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		f.Build(RawAdapter{})
		s, ok := RawAdapter{}.GetSqlString(f, RawContext{Table: "t"})
		assert.False(t, ok, "dsl=%q rendered %q", dsl, s)
	}

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString("...<0"))
	f.Build(RawAdapter{})
	_, _, err := BuildRawWhere(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty name segment")

	// A dotted qualified reference is still legal.
	f2 := figo.New()
	f2.AddFilter(figo.EqExpr{Field: "users.first_name", Value: "x"})
	f2.Build(RawAdapter{})
	where, _, err := BuildRawWhere(f2)
	require.NoError(t, err)
	assert.Equal(t, "`users`.`first_name` = ?", where)
}
