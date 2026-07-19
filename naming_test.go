package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCustomNamingFunc(t *testing.T) {
	t.Run("OverridesDefault", func(t *testing.T) {
		f := New()
		// The custom func replaces the default snake_case conversion.
		f.SetNamingFunc(func(field string) string { return "t_" + strings.ToUpper(field) })
		f.AddFiltersFromString(`userId=1 and status="x"`)
		f.Build(RawAdapter{})
		where, _, _ := BuildRawWhere(f)
		assert.Contains(t, where, "`t_USERID`")
		assert.Contains(t, where, "`t_STATUS`")
		assert.NotContains(t, where, "user_id") // snake_case did not run
	})

	t.Run("NilResetsToDefault", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(func(field string) string { return "X" })
		f.SetNamingFunc(nil) // reset to the default (SnakeCaseNaming)
		f.AddFiltersFromString(`userId=1`)
		f.Build(RawAdapter{})
		where, _, _ := BuildRawWhere(f)
		assert.Contains(t, where, "`user_id`")
	})

	t.Run("GetNamingFunc", func(t *testing.T) {
		f := New()
		assert.NotNil(t, f.GetNamingFunc(), "default SnakeCaseNaming is active from New()")
		assert.Equal(t, "user_id", f.GetNamingFunc()("userId"))
		f.SetNamingFunc(func(s string) string { return s })
		assert.Equal(t, "userId", f.GetNamingFunc()("userId"))
	})
}

func TestNoChangeNamingLeavesNameUnchanged(t *testing.T) {
	f := New()
	f.SetNamingFunc(NoChangeNaming)
	f.AddFiltersFromString(`userId=1`)
	f.Build(RawAdapter{})
	where, _, _ := BuildRawWhere(f)
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
	c.Build(nil)

	where, _, _ := BuildRawWhere(c)
	assert.Contains(t, where, "`c_name`", "clone should apply the inherited naming func")
}
