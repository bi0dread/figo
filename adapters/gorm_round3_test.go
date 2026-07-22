package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newRound3DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// gormWhere renders just the WHERE clause of a figo instance via sqlite DryRun.
func gormWhere(t *testing.T, f Figo) string {
	t.Helper()
	sql, ok := GormAdapter{}.GetSqlString(f, newRound3DB(t).Table("t"), "WHERE")
	require.True(t, ok, "gorm render must succeed")
	return sql
}

// A1: GORM's clause.Not unwraps a lone AndConditions operand and distributes
// the negation, turning NOT(a AND b) — NAND — into NOT a AND NOT b — NOR.
// The NOT must be preserved faithfully for every operand shape.
func TestGormNotPreservesNandVsNor(t *testing.T) {
	t.Run("not over AND stays NAND", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`not (a=1 and b=2)`))
		f.Build(GormAdapter{})
		sql := gormWhere(t, f)
		assert.Contains(t, sql, "NOT", "the NOT must survive: %q", sql)
		assert.Contains(t, sql, "AND", "the inner conjunction must survive: %q", sql)
		assert.NotContains(t, sql, "<>", "NOT must not be distributed over the conjuncts: %q", sql)
	})

	t.Run("not over OR stays NOR", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`not (a=1 or b=2)`))
		f.Build(GormAdapter{})
		sql := gormWhere(t, f)
		assert.Contains(t, sql, "NOT", "%q", sql)
		assert.Contains(t, sql, "OR", "%q", sql)
		assert.NotContains(t, sql, "<>", "%q", sql)
	})

	t.Run("programmatic multi-operand NotExpr stays NOR", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: int64(1)},
			EqExpr{Field: "b", Value: int64(2)},
		}})
		f.Build(GormAdapter{})
		sql := gormWhere(t, f)
		assert.Contains(t, sql, "NOT", "%q", sql)
		assert.Contains(t, sql, "OR", "NOR is NOT(a OR b): %q", sql)
	})

	t.Run("plain not", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`not a=1`))
		f.Build(GormAdapter{})
		sql := gormWhere(t, f)
		assert.Contains(t, sql, "NOT", "%q", sql)
		assert.Contains(t, sql, "= 1", "%q", sql)
	})

	// The gorm rendering must agree with the raw adapter on which logical
	// operator appears under the NOT.
	t.Run("agrees with raw adapter", func(t *testing.T) {
		for _, dsl := range []string{`not (a=1 and b=2)`, `not (a=1 or b=2)`} {
			fRaw := New()
			require.NoError(t, fRaw.AddFiltersFromString(dsl))
			fRaw.Build(RawAdapter{})
			rawSQL, _, err := BuildRawWhere(fRaw)
			require.NoError(t, err)

			fGorm := New()
			require.NoError(t, fGorm.AddFiltersFromString(dsl))
			fGorm.Build(GormAdapter{})
			gormSQL := gormWhere(t, fGorm)

			assert.Equal(t, strings.Contains(rawSQL, " AND "), strings.Contains(gormSQL, " AND "), "dsl=%q raw=%q gorm=%q", dsl, rawSQL, gormSQL)
			assert.Equal(t, strings.Contains(rawSQL, " OR "), strings.Contains(gormSQL, " OR "), "dsl=%q raw=%q gorm=%q", dsl, rawSQL, gormSQL)
		}
	})
}

// A10 parity: NOT over an operand that renders no predicate (vacuously true)
// must match NOTHING, not silently vanish.
func TestGormNotOverVacuousTrueFailsClosed(t *testing.T) {
	f := New()
	f.AddFilter(NotExpr{Operands: []Expr{AndExpr{}}})
	f.Build(GormAdapter{})
	sql := gormWhere(t, f)
	assert.Contains(t, sql, "1=0", "NOT(TRUE) must match nothing: %q", sql)

	// The no-operand identity is unchanged: no predicate at all.
	f2 := New()
	f2.AddFilter(NotExpr{})
	f2.Build(GormAdapter{})
	sql2, ok := GormAdapter{}.GetSqlString(f2, newRound3DB(t).Table("t"), "WHERE")
	require.True(t, ok)
	assert.NotContains(t, sql2, "1=0", "empty NOT stays the vacuous-true identity: %q", sql2)
}

// A12: the SELECT column list must be rendered in deterministic (sorted)
// order, not map-iteration order, matching the raw and ES adapters.
func TestGormSelectFieldsDeterministicOrder(t *testing.T) {
	for i := 0; i < 10; i++ {
		f := New()
		f.AddSelectFields("zeta", "alpha", "mu")
		f.Build(GormAdapter{})
		sql, ok := GormAdapter{}.GetSqlString(f, newRound3DB(t).Table("t"))
		require.True(t, ok)
		ia, im, iz := strings.Index(sql, "alpha"), strings.Index(sql, "mu"), strings.Index(sql, "zeta")
		require.True(t, ia >= 0 && im >= 0 && iz >= 0, "all columns must render: %q", sql)
		assert.True(t, ia < im && im < iz, "columns must be sorted: %q", sql)
	}
}

// A14: an empty-string sort column must be skipped, never rendered as
// ORDER BY “ (core gains a diagnostic; adapters stay defensive).
func TestGormSkipsEmptySortColumns(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(GormAdapter{})
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}, {Name: "id", Desc: true}}})
	sql, ok := GormAdapter{}.GetSqlString(f, newRound3DB(t).Table("t"))
	require.True(t, ok)
	assert.NotContains(t, sql, "``", "empty sort column must not render: %q", sql)
	assert.Contains(t, sql, "ORDER BY", "the real sort column must remain: %q", sql)

	// All-empty sort: no ORDER BY at all.
	f2 := New()
	require.NoError(t, f2.AddFiltersFromString(`a=1`))
	f2.Build(GormAdapter{})
	f2.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}}})
	sql2, ok := GormAdapter{}.GetSqlString(f2, newRound3DB(t).Table("t"))
	require.True(t, ok)
	assert.NotContains(t, sql2, "ORDER BY", "%q", sql2)
}
