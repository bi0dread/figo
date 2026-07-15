package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A value literally equal to "and"/"or"/"not" must stay an ordinary value and
// must never be promoted to a logical operator.
func TestKeywordValuesStayValues(t *testing.T) {
	t.Run("quoted and as value", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`name="and"`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq, ok := clauses[0].(EqExpr)
		require.True(t, ok, "expected EqExpr, got %T", clauses[0])
		assert.Equal(t, "name", eq.Field)
		assert.Equal(t, "and", eq.Value)
	})

	t.Run("quoted not does not negate a neighbor", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a="not" and b=1`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		and, ok := clauses[0].(AndExpr)
		require.True(t, ok, "expected AndExpr, got %T", clauses[0])
		require.Len(t, and.Operands, 2)
		_, isNot := and.Operands[0].(NotExpr)
		assert.False(t, isNot, "NOT must not appear: %#v", and.Operands)
	})

	t.Run("quoted or keeps both conditions", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`logic="or" or c=2`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		or, ok := clauses[0].(OrExpr)
		require.True(t, ok, "expected OrExpr, got %T", clauses[0])
		assert.Len(t, or.Operands, 2)
	})
}

func buildOneCritical(t *testing.T, dsl string) Expr {
	t.Helper()
	f := New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(RawAdapter{})
	clauses := f.GetClauses()
	require.Len(t, clauses, 1, "dsl %q parsed to %d clauses", dsl, len(clauses))
	return clauses[0]
}

// Tabs and newlines must separate tokens exactly like spaces.
func TestWhitespaceVariantsSeparateTokens(t *testing.T) {
	expectOr := func(t *testing.T, dsl string) {
		t.Helper()
		f := New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		f.Build(RawAdapter{})
		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		or, ok := clauses[0].(OrExpr)
		require.True(t, ok, "expected OrExpr for %q, got %#v", dsl, clauses[0])
		require.Len(t, or.Operands, 2)
		a := or.Operands[0].(EqExpr)
		assert.Equal(t, int64(1), a.Value, "value corrupted for %q", dsl)
	}

	expectOr(t, "a=1\tor b=2")
	expectOr(t, "a=1\nor b=2")
	expectOr(t, "a=1 \r\n or b=2")

	t.Run("spaced operator with quoted value containing operator chars", func(t *testing.T) {
		assert.Equal(t, EqExpr{Field: "name", Value: "a=b"}, buildOneCritical(t, `name = "a=b"`))
		assert.Equal(t, EqExpr{Field: "url", Value: "http://x.com?a=1"}, buildOneCritical(t, `url = "http://x.com?a=1"`))
		assert.Equal(t, EqExpr{Field: "name", Value: "va<l"}, buildOneCritical(t, `name = "va<l"`))
	})

	t.Run("spaced in-list with inner spaces", func(t *testing.T) {
		in, ok := buildOneCritical(t, `x <in> [1, 2]`).(InExpr)
		require.True(t, ok)
		assert.Equal(t, []any{int64(1), int64(2)}, in.Values)

		in2, ok := buildOneCritical(t, `x <in> ["a b", "c"]`).(InExpr)
		require.True(t, ok)
		assert.Equal(t, []any{"a b", "c"}, in2.Values)
	})

	t.Run("quoted whitespace preserved", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString("a=\"x\ty\""))
		f.Build(RawAdapter{})
		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq := clauses[0].(EqExpr)
		assert.Equal(t, "x\ty", eq.Value)
	})
}
