package figo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type gormRegModel struct {
	ID   int
	Name string
	Age  int
}

func newGormRegDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&gormRegModel{}))
	return db
}

// GetQuery must apply figo's filters, exactly like GetSqlString does.
func TestGormGetQueryAppliesFilters(t *testing.T) {
	db := newGormRegDB(t)

	f := New()
	require.NoError(t, f.AddFiltersFromString(`name="x"`))
	f.Build(GormAdapter{})

	q := f.GetQuery(db.Model(&gormRegModel{}))
	require.NotNil(t, q)
	sq, ok := q.(SQLQuery)
	require.True(t, ok)
	assert.Contains(t, sq.SQL, "name")
	assert.Contains(t, sq.SQL, "?")
	assert.Equal(t, []any{"x"}, sq.Args)
}

// The no-conditionType call must render the complete SELECT, not "".
func TestGormFullQueryWithoutConditionType(t *testing.T) {
	db := newGormRegDB(t)

	f := New()
	require.NoError(t, f.AddFiltersFromString(`name="x" sort=id:desc page=skip:0,take:5`))
	f.Build(GormAdapter{})

	sql := f.GetSqlString(db.Model(&gormRegModel{}))
	assert.Contains(t, sql, "SELECT")
	assert.Contains(t, sql, "gorm_reg_models")
	assert.Contains(t, sql, "name")
	assert.Contains(t, sql, "LIMIT")
}

// A caller-scoped DB (e.g. tenant filter) must get figo's filters applied on
// top of its own scope — not have them silently dropped.
func TestGormPreScopedDBKeepsFigoFilters(t *testing.T) {
	db := newGormRegDB(t)

	f := New()
	require.NoError(t, f.AddFiltersFromString(`name="x"`))
	f.Build(GormAdapter{})

	scoped := db.Model(&gormRegModel{}).Where("age > ?", 18)
	sql := f.GetSqlString(scoped, "WHERE")
	assert.Contains(t, sql, "age", "caller scope must remain")
	assert.Contains(t, sql, "name", "figo filter must not be dropped")
}

// A DB that already went through ApplyGorm must not be double-applied.
func TestGormNoDoubleApply(t *testing.T) {
	db := newGormRegDB(t)

	f := New()
	require.NoError(t, f.AddFiltersFromString(`name="x"`))
	f.Build(GormAdapter{})

	applied := ApplyGorm(f, db.Model(&gormRegModel{}))
	sql := f.GetSqlString(applied, "WHERE")
	assert.Equal(t, 1, countOccurrences(sql, "name"), "filter applied twice: %s", sql)
}

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
