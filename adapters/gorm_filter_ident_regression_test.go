package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type gormFilterIdentUser struct {
	ID  int
	Age int
}

// A FILTER-position identifier with a control byte or an empty dot-segment
// sailed past this adapter into the dialector, while the SELECT list
// (gormProjectionName) and the raw adapter (validateIdent) both refuse the
// same two classes everywhere. Every field-bearing expression is now screened
// in toGormClauseWithFigo and the render fails closed via AddError.
func TestGormFilterFieldIdentScreenFailsClosed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&gormFilterIdentUser{}))
	require.NoError(t, db.Create(&gormFilterIdentUser{ID: 1, Age: 30}).Error)

	for _, field := range []string{"a\x00b", "a..b", "..."} {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		f.AddFilter(EqExpr{Field: field, Value: 1})
		f.Build(GormAdapter{})

		sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormFilterIdentUser{}))
		assert.False(t, ok, "filter field %q rendered %q instead of failing closed", field, sql)

		var rows []map[string]any
		assert.Error(t, ApplyGorm(f, db.Model(&gormFilterIdentUser{})).Find(&rows).Error,
			"filter field %q executed and returned %v", field, rows)
		assert.Empty(t, rows, "filter field %q returned rows", field)
	}

	// Control: an ordinary filter still renders and executes.
	f := New()
	require.NoError(t, f.AddFiltersFromString("age > 18"))
	f.Build(GormAdapter{})
	sql, ok := GormAdapter{}.GetSqlString(f, db.Model(&gormFilterIdentUser{}))
	require.True(t, ok, "ordinary filter failed to render")
	assert.Contains(t, sql, "age")

	// CustomExpr stays exempt: its handler receives the field verbatim and
	// owns quoting/validation by contract, exactly like the raw adapter.
	fc := New()
	fc.SetNamingFunc(NoChangeNaming)
	fc.AddFilter(CustomExpr{
		Field:    "a\x00b",
		Operator: "noop",
		Value:    1,
		Handler: func(field, operator string, value any) (string, []any, error) {
			return "1=1", nil, nil
		},
	})
	fc.Build(GormAdapter{})
	_, okc := GormAdapter{}.GetSqlString(fc, db.Model(&gormFilterIdentUser{}))
	assert.True(t, okc, "CustomExpr must remain exempt from the ident screen")
}
