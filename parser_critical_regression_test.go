package figo

import (
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

// Dropping an ignored field must remove its condition from the logical
// structure without leaving the neighboring NOT/AND/OR to re-bind elsewhere.
func TestIgnoreFieldsDoNotOrphanOperators(t *testing.T) {
	t.Run("not on ignored field does not retarget", func(t *testing.T) {
		f := New()
		f.AddIgnoreFields("secret")
		require.NoError(t, f.AddFiltersFromString(`not secret=1 and a=2`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq, ok := clauses[0].(EqExpr)
		require.True(t, ok, "expected bare EqExpr on a, got %#v", clauses[0])
		assert.Equal(t, "a", eq.Field)
	})

	t.Run("or survives an ignored middle condition", func(t *testing.T) {
		f := New()
		f.AddIgnoreFields("secret")
		require.NoError(t, f.AddFiltersFromString(`a=1 and secret=2 or b=3`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		or, ok := clauses[0].(OrExpr)
		require.True(t, ok, "OR must survive, got %#v", clauses[0])
		assert.Len(t, or.Operands, 2)
	})

	t.Run("ignore matches across naming strategies", func(t *testing.T) {
		f := New() // default snake_case strategy
		f.AddIgnoreFields("user_name")
		require.NoError(t, f.AddFiltersFromString(`userName=1 and a=2`))
		f.Build(RawAdapter{})

		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		eq, ok := clauses[0].(EqExpr)
		require.True(t, ok, "expected bare EqExpr on a, got %#v", clauses[0])
		assert.Equal(t, "a", eq.Field)
	})

	t.Run("ignored fields pruned from preloads", func(t *testing.T) {
		f := New()
		f.AddIgnoreFields("secret")
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:secret=1]`))
		f.Build(RawAdapter{})

		assert.Empty(t, f.GetPreloads()["Orders"])
	})
}

// A leading NOT is valid and must survive all three entry paths unchanged.
func TestLeadingNotNeverStripped(t *testing.T) {
	assertNegated := func(t *testing.T, f Figo) {
		t.Helper()
		clauses := f.GetClauses()
		require.Len(t, clauses, 1)
		not, ok := clauses[0].(NotExpr)
		require.True(t, ok, "expected NotExpr, got %#v", clauses[0])
		require.Len(t, not.Operands, 1)
		eq, ok := not.Operands[0].(EqExpr)
		require.True(t, ok)
		assert.Equal(t, "deleted", eq.Field)
	}

	t.Run("plain", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`not deleted=true`))
		f.Build(RawAdapter{})
		assertNegated(t, f)
	})

	t.Run("with repair enabled", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromStringWithRepair(`not deleted=true`, true))
		f.Build(RawAdapter{})
		assertNegated(t, f)
	})

	t.Run("with repair disabled (validation)", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromStringWithRepair(`not deleted=true`, false))
		f.Build(RawAdapter{})
		assertNegated(t, f)
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
