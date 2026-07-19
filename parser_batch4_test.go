package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
)

// #9: operator characters inside a quoted value must not split the token.
func TestQuotedValueWithOperatorChars(t *testing.T) {
	cases := map[string][]any{
		`name="x=y"`:             {"x=y"},
		`note="a>=b"`:            {"a>=b"},
		`s="a<b"`:                {"a<b"},
		`url="http://h?a=1&b=2"`: {"http://h?a=1&b=2"},
	}
	for dsl, wantArgs := range cases {
		where, args := whereFor(t, dsl)
		assert.Contains(t, where, "= ?", dsl)
		assert.Equal(t, wantArgs, args, dsl)
	}
}

// #10: field names that merely start with sort/page/load are real filters.
func TestKeywordPrefixedFieldsAreFilters(t *testing.T) {
	for _, tc := range []struct {
		dsl, col string
	}{
		{`sortOrder=5`, "`sort_order`"},
		{`pageCount=10`, "`page_count`"},
		{`loadedAt>100`, "`loaded_at`"},
	} {
		where, _ := whereFor(t, tc.dsl)
		assert.Contains(t, where, tc.col, tc.dsl)
	}

	// Real sort=/page= directives still work and don't leak into the WHERE.
	f := New()
	f.AddFiltersFromString(`id=1 sort=id:desc page=skip:5,take:10`)
	f.Build(RawAdapter{})
	where, _, _ := BuildRawWhere(f)
	assert.Equal(t, "`id` = ?", where)
	assert.NotNil(t, f.GetSort())
	assert.Equal(t, Page{Skip: 5, Take: 10}, f.GetPage())
}

// #11: adjacent expressions (missing operator, or a filter after load=/sort=/page=)
// are combined with an implicit AND, not silently dropped.
func TestAdjacentExpressionsImpliedAnd(t *testing.T) {
	for _, dsl := range []string{
		`a=1 b=2`,
		`a=1 load=[T:id=3] b=2`,
		`a=1 sort=id:desc b=2`,
	} {
		where, args := whereFor(t, dsl)
		assert.Contains(t, where, "AND", dsl)
		assert.Len(t, args, 2, dsl)
	}
}

// #12: <in> lists with quoted elements/commas are parsed correctly.
func TestInListWithQuotedCommas(t *testing.T) {
	where, args := whereFor(t, `name<in>["a,b","c"]`)
	assert.Contains(t, where, "IN (?,?)")
	assert.Equal(t, []any{"a,b", "c"}, args)

	// Unquoted numeric lists still work.
	_, nums := whereFor(t, `id<in>[1,2,3]`)
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, nums)
}

// #23: a bare sort=/page= must not emit a bogus empty-field clause.
func TestEmptySortPageNoBogusClause(t *testing.T) {
	for _, dsl := range []string{`sort=`, `page=`} {
		where, _ := whereFor(t, dsl)
		assert.Empty(t, where, dsl)
	}
	// With a real filter alongside an empty sort, only the filter remains.
	where, _ := whereFor(t, `id=1 sort=`)
	assert.Equal(t, "`id` = ?", where)
}

// Regression guard: the earlier paren and BETWEEN fixes still hold.
func TestBatch4DoesNotRegressParensOrBetween(t *testing.T) {
	where, _ := whereFor(t, `(a=1 or b=2) and c=3`)
	assert.Equal(t, "((`a` = ? OR `b` = ?) AND `c` = ?)", where)

	bw, bargs := whereFor(t, `price<bet>(10..20)`)
	assert.Contains(t, bw, "BETWEEN")
	assert.Equal(t, []any{int64(10), int64(20)}, bargs)
}
