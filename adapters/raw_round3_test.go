package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A11: a programmatic EqExpr/NeqExpr with a nil Value must canonicalize to the
// adapter's IsNull/NotNull rendering everywhere (the DSL parses x=null into
// IsNullExpr already; the programmatic path diverged).
func TestNilValueEqNeqCanonicalAcrossAdapters(t *testing.T) {
	newNil := func() Figo {
		f := New()
		f.AddFilter(EqExpr{Field: "x", Value: nil})
		f.AddFilter(NeqExpr{Field: "y", Value: nil})
		return f
	}

	t.Run("raw", func(t *testing.T) {
		f := newNil()
		f.Build(RawAdapter{})
		sql, args, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "`x` IS NULL AND `y` IS NOT NULL", sql)
		assert.Empty(t, args, "no bind args for NULL predicates")
	})

	t.Run("gorm", func(t *testing.T) {
		f := newNil()
		f.Build(GormAdapter{})
		sql := gormWhere(t, f)
		assert.Contains(t, sql, "IS NULL")
		assert.Contains(t, sql, "IS NOT NULL")
	})

	t.Run("mongo", func(t *testing.T) {
		f := newNil()
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		parts, ok := m["$and"].([]bson.M)
		require.True(t, ok, "got %v", m)
		assert.Equal(t, bson.M{"x": nil}, parts[0], "same as IsNullExpr")
		assert.Equal(t, bson.M{"y": bson.M{"$ne": nil}}, parts[1], "same as NotNullExpr")
	})

	t.Run("elasticsearch", func(t *testing.T) {
		f := newNil()
		f.Build(ElasticsearchAdapter{})
		q, err := BuildElasticsearchQuery(f)
		require.NoError(t, err)
		must := q.Query["bool"].(map[string]interface{})["must"].([]map[string]interface{})
		require.Len(t, must, 2)
		// Eq nil == IsNullExpr: must_not exists.
		mn := must[0]["bool"].(map[string]interface{})["must_not"].(map[string]interface{})
		assert.Contains(t, mn, "exists")
		// Neq nil == NotNullExpr: exists.
		assert.Contains(t, must[1], "exists")
	})
}

// A13: a non-finite float in the literal-expansion (display) path must not be
// spliced verbatim as invalid SQL; it renders NULL, which no comparison
// matches (fail closed).
func TestRawNonFiniteFloatLiteralFailsClosed(t *testing.T) {
	for name, v := range map[string]float64{"NaN": math.NaN(), "PosInf": math.Inf(1), "NegInf": math.Inf(-1)} {
		t.Run(name, func(t *testing.T) {
			f := New()
			f.AddFilter(EqExpr{Field: "price", Value: v})
			f.Build(RawAdapter{})
			sql, ok := RawAdapter{}.GetSqlString(f, "t")
			require.True(t, ok)
			assert.Contains(t, sql, "`price` = NULL", "non-finite float must fail closed: %q", sql)
			assert.NotContains(t, sql, "NaN", "%q", sql)
			assert.NotContains(t, sql, "Inf", "%q", sql)
		})
	}

	// Finite floats are unchanged.
	f := New()
	f.AddFilter(EqExpr{Field: "price", Value: 1.5})
	f.Build(RawAdapter{})
	sql, ok := RawAdapter{}.GetSqlString(f, "t")
	require.True(t, ok)
	assert.Contains(t, sql, "`price` = 1.5")
}

// A14: empty-string sort columns must be skipped, never ORDER BY “.
func TestRawSkipsEmptySortColumns(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(RawAdapter{})
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}, {Name: "id", Desc: true}}})
	sql, _, err := BuildRawSelect(f, "t")
	require.NoError(t, err)
	assert.NotContains(t, sql, "``", "empty sort column leaked: %q", sql)
	assert.Contains(t, sql, "ORDER BY `id` DESC", "%q", sql)

	// All-empty sort: no ORDER BY at all.
	f2 := New()
	require.NoError(t, f2.AddFiltersFromString(`a=1`))
	f2.Build(RawAdapter{})
	f2.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}}})
	sql2, _, err := BuildRawSelect(f2, "t")
	require.NoError(t, err)
	assert.NotContains(t, sql2, "ORDER BY", "%q", sql2)
}

// A10 parity on the SQL side: NOT over an operand that renders no predicate
// (vacuously true, e.g. an empty AND identity) must match NOTHING, while the
// no-operand NOT identity stays match-all.
func TestRawNotOverVacuousTrueFailsClosed(t *testing.T) {
	f := New()
	f.AddFilter(NotExpr{Operands: []Expr{AndExpr{}}})
	f.Build(RawAdapter{})
	sql, args, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", sql)
	assert.Empty(t, args)

	// NOT of an empty NOT-IN (true for every row) already failed closed; pin it.
	f2 := New()
	f2.AddFilter(NotExpr{Operands: []Expr{NotInExpr{Field: "id", Values: []any{}}}})
	f2.Build(RawAdapter{})
	sql2, _, err := BuildRawWhere(f2)
	require.NoError(t, err)
	assert.Equal(t, "NOT (1=1)", sql2)

	// The no-operand identity is unchanged.
	f3 := New()
	f3.AddFilter(NotExpr{})
	f3.Build(RawAdapter{})
	sql3, _, err := BuildRawWhere(f3)
	require.NoError(t, err)
	assert.Equal(t, "", sql3)
}
