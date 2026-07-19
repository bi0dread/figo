package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawAdapterPostgresDialect(t *testing.T) {
	newPG := func(dsl string) Figo {
		f := New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		f.Build(RawAdapter{Dialect: PostgresDialect})
		return f
	}

	t.Run("QuotingAndNumberedPlaceholders", func(t *testing.T) {
		f := newPG(`id=1 and name="x" sort=id:desc page=skip:5,take:10`)
		q, ok := f.GetQuery(RawContext{Table: "users"}).(SQLQuery)
		require.True(t, ok)

		assert.Equal(t,
			`SELECT * FROM "users" WHERE ("id" = $1 AND "name" = $2) ORDER BY "id" DESC LIMIT 10 OFFSET 5`,
			q.SQL)
		assert.Equal(t, []any{int64(1), "x"}, q.Args)
	})

	t.Run("InListNumbersEveryElement", func(t *testing.T) {
		f := newPG(`id<in>[1,2,3] and age>18`)
		q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)
		assert.Contains(t, q.SQL, `"id" IN ($1,$2,$3)`)
		assert.Contains(t, q.SQL, `"age" > $4`)
	})

	t.Run("RegexUsesTildeOperator", func(t *testing.T) {
		f := newPG(`email=~"gmail"`)
		q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)
		assert.Contains(t, q.SQL, `"email" ~ $1`)
	})

	t.Run("InterpolatedSqlStringStaysUnnumbered", func(t *testing.T) {
		f := newPG(`id=1 and name="O'Brien"`)
		sql := f.GetSqlString(RawContext{Table: "t"})
		assert.Contains(t, sql, `"id" = 1`)
		assert.Contains(t, sql, `"name" = 'O''Brien'`)
		assert.NotContains(t, sql, "$1", "literal interpolation must not leave placeholders")
	})

	t.Run("BackslashNotDoubled", func(t *testing.T) {
		f := New()
		f.AddFilter(EqExpr{Field: "path", Value: `a\b`})
		f.Build(RawAdapter{Dialect: PostgresDialect})
		sql := f.GetSqlString(RawContext{Table: "t"})
		assert.Contains(t, sql, `'a\b'`, "Postgres treats backslash literally")
	})

	t.Run("BareOffsetUsesLimitAll", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`id=1`))
		f.SetPage(7, 0)
		f.Build(RawAdapter{Dialect: PostgresDialect})
		sql := f.GetSqlString(RawContext{Table: "t"})
		assert.Contains(t, sql, "LIMIT ALL OFFSET 7")
	})

	t.Run("BuildRawHelpersNumbered", func(t *testing.T) {
		f := newPG(`a=1 and b=2 load=[Orders:price>10]`)
		where, args, _ := BuildRawWhere(f)
		assert.Contains(t, where, `"a" = $1`)
		assert.Contains(t, where, `"b" = $2`)
		assert.Len(t, args, 2)

		pre, _ := BuildRawPreloads(f)
		assert.Contains(t, pre["Orders"].Where, `"price" > $1`)
	})

	t.Run("CustomCaseInsensitiveRegex", func(t *testing.T) {
		pg := *PostgresDialect
		pg.RegexOperator = "~*"
		f := New()
		require.NoError(t, f.AddFiltersFromString(`email=~"gmail"`))
		f.Build(RawAdapter{Dialect: &pg})
		q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)
		assert.Contains(t, q.SQL, `"email" ~* $1`)
	})
}

func TestRawAdapterSQLiteDialect(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1`))
	f.SetPage(4, 0)
	f.Build(RawAdapter{Dialect: SQLiteDialect})

	q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)
	assert.Contains(t, q.SQL, `"id" = ?`, "SQLite keeps ? placeholders with double-quoted identifiers")
	assert.Contains(t, q.SQL, "LIMIT -1 OFFSET 4", "SQLite bare-offset uses LIMIT -1")
}

// The zero-value adapter must keep the historical MySQL rendering.
func TestRawAdapterDefaultDialectIsMySQL(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 and email=~"x"`))
	f.Build(RawAdapter{})

	q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)
	assert.Contains(t, q.SQL, "`id` = ?")
	assert.Contains(t, q.SQL, "`email` REGEXP ?")
}

// A '?' inside a quoted identifier or string literal must never be renumbered.
func TestNumberPlaceholdersIsQuoteAware(t *testing.T) {
	assert.Equal(t, `SELECT * FROM "a?b" WHERE "x" = $1`,
		numberPlaceholders(`SELECT * FROM "a?b" WHERE "x" = ?`))
	assert.Equal(t, `WHERE "note" = $1 AND "q" = 'why?'`,
		numberPlaceholders(`WHERE "note" = ? AND "q" = 'why?'`))
	// Two-digit numbering.
	in := "?,?,?,?,?,?,?,?,?,?,?"
	assert.Equal(t, "$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11", numberPlaceholders(in))
}
