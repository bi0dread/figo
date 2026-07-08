package figo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCustomNamingFunc(t *testing.T) {
	t.Run("OverridesStrategy", func(t *testing.T) {
		f := New()
		// Even with the default snake_case strategy active, the custom func wins.
		f.SetNamingFunc(func(field string) string { return "t_" + strings.ToUpper(field) })
		f.AddFiltersFromString(`userId=1 and status="x"`)
		f.Build(RawAdapter{})
		where, _ := BuildRawWhere(f)
		assert.Contains(t, where, "`t_USERID`")
		assert.Contains(t, where, "`t_STATUS`")
		assert.NotContains(t, where, "user_id") // snake_case did not run
	})

	t.Run("NilFallsBackToStrategy", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(func(field string) string { return "X" })
		f.SetNamingFunc(nil) // remove the custom func
		f.SetNamingStrategy(NAMING_STRATEGY_SNAKE_CASE)
		f.AddFiltersFromString(`userId=1`)
		f.Build(RawAdapter{})
		where, _ := BuildRawWhere(f)
		assert.Contains(t, where, "`user_id`")
	})

	t.Run("GetNamingFunc", func(t *testing.T) {
		f := New()
		assert.Nil(t, f.GetNamingFunc())
		f.SetNamingFunc(func(s string) string { return s })
		assert.NotNil(t, f.GetNamingFunc())
	})
}

func TestUnknownStrategyLeavesNameUnchanged(t *testing.T) {
	f := New()
	f.SetNamingStrategy("does_not_exist")
	f.AddFiltersFromString(`userId=1`)
	f.Build(RawAdapter{})
	where, _ := BuildRawWhere(f)
	// Previously an unknown strategy blanked the column name; now it is a no-op.
	assert.Contains(t, where, "`userId`")
}

func TestCloneCarriesNamingFunc(t *testing.T) {
	f := New()
	f.SetNamingFunc(func(field string) string { return "c_" + field })
	f.AddFiltersFromString(`id=1`)
	f.Build(RawAdapter{})

	c := f.Clone()
	c.AddFilter(EqExpr{Field: c.GetNamingFunc()("added"), Value: int64(2)})
	c.AddFiltersFromString(`name="x"`)
	c.Build()

	where, _ := BuildRawWhere(c)
	assert.Contains(t, where, "`c_name`", "clone should apply the inherited naming func")
}
