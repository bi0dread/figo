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

// SnakeCaseNaming's conversion contract, pinned against the exact outputs of
// the gobeam/stringy implementation it replaced: words split at separators
// and lower→UPPER boundaries only — acronym runs stay glued, digits never
// open a boundary, separator runs collapse, symbols pass through, leading
// underscores are preserved.
func TestSnakeCaseNamingContract(t *testing.T) {
	cases := map[string]string{
		"userId":         "user_id",
		"userName":       "user_name",
		"userID":         "user_id",
		"parentID":       "parent_id",
		"HTTPServer":     "httpserver",
		"XMLHttpRequest": "xmlhttp_request",
		"OrderItemsV2":   "order_items_v2",
		"myURL2Path":     "my_url2path",
		"USERId":         "userid",
		"ABC":            "abc",
		"ID":             "id",
		"id":             "id",
		"N":              "n",
		"user123":        "user123",
		"a1B2":           "a1b2",
		"snake_case":     "snake_case",
		"user__name":     "user_name",
		"user.age":       "user_age",
		"user-name":      "user_name",
		"user name":      "user_name",
		"price$":         "price$",
		"_id":            "_id",
		"__v":            "__v",
		"سن":             "سن",
		"émailAddress":   "émail_address",
	}
	for in, want := range cases {
		assert.Equal(t, want, SnakeCaseNaming(in), "input %q", in)
	}
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
