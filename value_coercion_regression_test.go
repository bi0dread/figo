package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildOne(t *testing.T, dsl string) Expr {
	t.Helper()
	f := New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(RawAdapter{})
	clauses := f.GetClauses()
	require.Len(t, clauses, 1, "dsl %q", dsl)
	return clauses[0]
}

// Quoting is the DSL's "keep this a string" mechanism and must be honored.
func TestQuotedScalarsStayStrings(t *testing.T) {
	assert.Equal(t, EqExpr{Field: "code", Value: "0123"}, buildOne(t, `code="0123"`))
	assert.Equal(t, EqExpr{Field: "flag", Value: "true"}, buildOne(t, `flag="true"`))
	assert.Equal(t, EqExpr{Field: "x", Value: "null"}, buildOne(t, `x="null"`))
	assert.Equal(t, EqExpr{Field: "n", Value: "1.5"}, buildOne(t, `n="1.5"`))
	assert.Equal(t, EqExpr{Field: "d", Value: "2020-01-01"}, buildOne(t, `d="2020-01-01"`))
	assert.Equal(t, EqExpr{Field: "s", Value: "  spaced  "}, buildOne(t, `s="  spaced  "`))
}

// x=null / x!=null mean IS NULL / IS NOT NULL, never the string "<nil>".
func TestNullComparisonSemantics(t *testing.T) {
	assert.Equal(t, IsNullExpr{Field: "x"}, buildOne(t, `x=null`))
	assert.Equal(t, NotNullExpr{Field: "x"}, buildOne(t, `x!=null`))
}

// Unquoted date literals must reach the adapters as time.Time.
func TestUnquotedDatesSurviveToAST(t *testing.T) {
	eq, ok := buildOne(t, `d>=2020-01-05`).(GteExpr)
	require.True(t, ok)
	d, ok := eq.Value.(time.Time)
	require.True(t, ok, "expected time.Time, got %T (%v)", eq.Value, eq.Value)
	assert.Equal(t, 2020, d.Year())
	assert.Equal(t, time.January, d.Month())
	assert.Equal(t, 5, d.Day())

	// BETWEEN must type its bounds the same way as scalar comparisons.
	bet, ok := buildOne(t, `d<bet>(2020-01-01..2021-01-01)`).(BetweenExpr)
	require.True(t, ok)
	_, lowIsTime := bet.Low.(time.Time)
	_, highIsTime := bet.High.(time.Time)
	assert.True(t, lowIsTime, "Low: %T", bet.Low)
	assert.True(t, highIsTime, "High: %T", bet.High)
}

// Integers beyond int64 must not silently degrade to a lossy float64.
func TestBigIntegerDoesNotBecomeFloat(t *testing.T) {
	eq, ok := buildOne(t, `x=9223372036854775808`).(EqExpr)
	require.True(t, ok)
	assert.Equal(t, "9223372036854775808", eq.Value)

	eqMax, ok := buildOne(t, `x=9223372036854775807`).(EqExpr)
	require.True(t, ok)
	assert.Equal(t, int64(9223372036854775807), eqMax.Value)
}

// Float literals stay floats; ints stay ints.
func TestNumericLiteralTypesPreserved(t *testing.T) {
	assert.Equal(t, EqExpr{Field: "p", Value: float64(0)}, buildOne(t, `p=0.0`))
	assert.Equal(t, EqExpr{Field: "p", Value: int64(0)}, buildOne(t, `p=0`))
}
