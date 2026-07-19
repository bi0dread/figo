package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildOnePositional(t *testing.T, dsl string) Expr {
	t.Helper()
	f := New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(RawAdapter{})
	clauses := f.GetClauses()
	require.Len(t, clauses, 1, "dsl %q parsed to %d clauses", dsl, len(clauses))
	return clauses[0]
}

// The precedence reducer used to pair operators with expressions by index in
// two parallel slices; an implicit connector shifted the pairing one slot, so
// "a=1 not b=2" negated a instead of b. NOT must always wrap the expression
// that FOLLOWS it in source order.
func TestNotBindsFollowingOperandUnderImplicitAnd(t *testing.T) {
	and, ok := buildOnePositional(t, `a=1 not b=2`).(AndExpr)
	require.True(t, ok)
	require.Len(t, and.Operands, 2)
	assert.Equal(t, EqExpr{Field: "a", Value: int64(1)}, and.Operands[0])
	assert.Equal(t, NotExpr{Operands: []Expr{EqExpr{Field: "b", Value: int64(2)}}}, and.Operands[1])
}

// The explicit-connector forms were already correct and must stay bit-for-bit.
func TestNotBindingWithExplicitConnectorsUnchanged(t *testing.T) {
	t.Run("leading not", func(t *testing.T) {
		and := buildOnePositional(t, `not a=1 and b=2`).(AndExpr)
		assert.Equal(t, NotExpr{Operands: []Expr{EqExpr{Field: "a", Value: int64(1)}}}, and.Operands[0])
		assert.Equal(t, EqExpr{Field: "b", Value: int64(2)}, and.Operands[1])
	})

	t.Run("not after and", func(t *testing.T) {
		and := buildOnePositional(t, `a=1 and not b=2`).(AndExpr)
		assert.Equal(t, EqExpr{Field: "a", Value: int64(1)}, and.Operands[0])
		assert.Equal(t, NotExpr{Operands: []Expr{EqExpr{Field: "b", Value: int64(2)}}}, and.Operands[1])
	})

	t.Run("stacked not", func(t *testing.T) {
		outer := buildOnePositional(t, `not not a=1`).(NotExpr)
		inner := outer.Operands[0].(NotExpr)
		assert.Equal(t, EqExpr{Field: "a", Value: int64(1)}, inner.Operands[0])
	})

	t.Run("not over group", func(t *testing.T) {
		outer := buildOnePositional(t, `not (a=1 or b=2)`).(NotExpr)
		_, isOr := outer.Operands[0].(OrExpr)
		assert.True(t, isOr)
	})
}

// The same index drift mispaired binary connectors when an implicit AND was
// present: "a=1 b=2 or c=3" attached the OR to (a,b). An explicit connector
// must pair the expressions at its source position; only the leftover
// adjacent expressions combine under the implicit AND, last.
func TestExplicitConnectorPairsAtSourcePosition(t *testing.T) {
	t.Run("implicit then or", func(t *testing.T) {
		and, ok := buildOnePositional(t, `a=1 b=2 or c=3`).(AndExpr)
		require.True(t, ok)
		require.Len(t, and.Operands, 2)
		assert.Equal(t, EqExpr{Field: "a", Value: int64(1)}, and.Operands[0])
		assert.Equal(t, OrExpr{Operands: []Expr{
			EqExpr{Field: "b", Value: int64(2)},
			EqExpr{Field: "c", Value: int64(3)},
		}}, and.Operands[1])
	})

	t.Run("or then implicit", func(t *testing.T) {
		and, ok := buildOnePositional(t, `a=1 or b=2 c=3`).(AndExpr)
		require.True(t, ok)
		require.Len(t, and.Operands, 2)
		assert.Equal(t, OrExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: int64(1)},
			EqExpr{Field: "b", Value: int64(2)},
		}}, and.Operands[0])
		assert.Equal(t, EqExpr{Field: "c", Value: int64(3)}, and.Operands[1])
	})
}

// isSimpleFieldName was ASCII-only, so a non-Latin field name followed by a
// SPACED operator failed to combine and emitted a predicate on an empty
// column (`سن > 5` rendered "`` > ''"). Any Unicode letter/digit is a valid
// field-name character; the unspaced form already worked and must keep working.
func TestUnicodeFieldNamesWithSpacedOperator(t *testing.T) {
	t.Run("arabic spaced", func(t *testing.T) {
		gt, ok := buildOnePositional(t, `سن > 5`).(GtExpr)
		require.True(t, ok, "expected GtExpr, got %#v", buildOnePositional(t, `سن > 5`))
		assert.Equal(t, "سن", gt.Field)
		assert.Equal(t, int64(5), gt.Value)
	})

	t.Run("accented spaced", func(t *testing.T) {
		eq, ok := buildOnePositional(t, `émail = "x"`).(EqExpr)
		require.True(t, ok)
		assert.Equal(t, "émail", eq.Field)
		assert.Equal(t, "x", eq.Value)
	})

	t.Run("accented unspaced still works", func(t *testing.T) {
		eq, ok := buildOnePositional(t, `émail="x"`).(EqExpr)
		require.True(t, ok)
		assert.Equal(t, "émail", eq.Field)
	})
}
