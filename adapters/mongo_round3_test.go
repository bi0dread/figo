package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// stageValue returns the value of the first stage carrying the given operator
// key, plus its index in the pipeline (-1 when absent).
func stageValue(pipe mongo.Pipeline, op string) (any, int) {
	for i, stage := range pipe {
		for _, e := range stage {
			if e.Key == op {
				return e.Value, i
			}
		}
	}
	return nil, -1
}

// A2: the aggregation pipeline (the load= path) must carry sort= and page= as
// $sort/$skip/$limit stages with BuildMongoFindOptions semantics.
func TestMongoAggregatePipelineIncludesSortSkipLimit(t *testing.T) {
	t.Run("sort skip limit appended in order", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[orders:id=2] sort=id:desc page=skip:10,take:5`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, nil)
		require.NoError(t, err)

		sortVal, sortIdx := stageValue(pipe, "$sort")
		require.NotEqual(t, -1, sortIdx, "missing $sort stage: %v", pipe)
		sd, ok := sortVal.(bson.D)
		require.True(t, ok, "$sort must be a bson.D, got %T", sortVal)
		require.Len(t, sd, 1)
		assert.Equal(t, "id", sd[0].Key)
		assert.Equal(t, -1, sd[0].Value)

		skipVal, skipIdx := stageValue(pipe, "$skip")
		require.NotEqual(t, -1, skipIdx, "missing $skip stage: %v", pipe)
		assert.Equal(t, int64(10), skipVal)

		limitVal, limitIdx := stageValue(pipe, "$limit")
		require.NotEqual(t, -1, limitIdx, "missing $limit stage: %v", pipe)
		assert.Equal(t, int64(5), limitVal)

		_, lookupIdx := stageValue(pipe, "$lookup")
		require.NotEqual(t, -1, lookupIdx)
		assert.True(t, lookupIdx < sortIdx && sortIdx < skipIdx && skipIdx < limitIdx,
			"stages must come after the lookup as $sort,$skip,$limit: %v", pipe)
	})

	t.Run("take zero means no limit and skip zero omitted", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[orders:id=2] page=skip:0,take:0`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, nil)
		require.NoError(t, err)
		_, limitIdx := stageValue(pipe, "$limit")
		assert.Equal(t, -1, limitIdx, "take:0 must not render $limit: %v", pipe)
		_, skipIdx := stageValue(pipe, "$skip")
		assert.Equal(t, -1, skipIdx, "skip:0 must not render $skip: %v", pipe)
	})

	t.Run("GetQuery aggregate path carries the stages too", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[orders:id=2] sort=id:asc page=skip:3,take:7`))
		f.Build(MongoAdapter{})
		q, ok := MongoAdapter{}.GetQuery(f, nil, "AGG")
		require.True(t, ok)
		agg, ok := q.(MongoAggregateQuery)
		require.True(t, ok)
		_, sortIdx := stageValue(agg.Pipeline, "$sort")
		assert.NotEqual(t, -1, sortIdx)
		_, skipIdx := stageValue(agg.Pipeline, "$skip")
		assert.NotEqual(t, -1, skipIdx)
		_, limitIdx := stageValue(agg.Pipeline, "$limit")
		assert.NotEqual(t, -1, limitIdx)
	})
}

// A9: $text is illegal under $nor at any depth; MongoDB rejects it
// server-side, so the build must fail with a clear error instead of emitting
// invalid syntax.
func TestMongoNotOfFullTextErrors(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{FullTextSearchExpr{Query: "banned"}}})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$text")
	})

	t.Run("nested under AND", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{AndExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: 1},
			FullTextSearchExpr{Query: "banned"},
		}}}})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$text")
	})

	t.Run("plain full text still works", func(t *testing.T) {
		f := New()
		f.AddFilter(FullTextSearchExpr{Query: "fine"})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Contains(t, m, "$text")
	})
}

// A10: NOT over an operand that renders a match-all fragment ({}) must match
// NOTHING ($nor over match-all), not drop the operand and return {} — which
// matched the whole collection while raw/ES/GORM matched nothing.
func TestMongoNotOverVacuousTrueMatchesNothing(t *testing.T) {
	matchNothing := bson.M{"$nor": []bson.M{{}}}

	t.Run("empty ArrayContains operand", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{ArrayContainsExpr{Field: "tags", Values: nil}}})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Equal(t, matchNothing, m)
	})

	t.Run("empty AND operand", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{AndExpr{}}})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Equal(t, matchNothing, m)
	})

	t.Run("empty NotIn operand still renders the negated predicate", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{NotInExpr{Field: "id", Values: []any{}}}})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		nor, ok := m["$nor"].([]bson.M)
		require.True(t, ok, "got %v", m)
		require.Len(t, nor, 1)
	})

	t.Run("no-operand NOT stays the vacuous-true identity", func(t *testing.T) {
		f := New()
		f.AddFilter(NotExpr{})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Empty(t, m)
	})
}

// A14: empty-string sort columns must be skipped on the Mongo sort paths.
func TestMongoSkipsEmptySortColumns(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(MongoAdapter{})
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}, {Name: "id", Desc: true}}})

	opts := BuildMongoFindOptions(f)
	sd, ok := opts.Sort.(bson.D)
	require.True(t, ok, "sort must be set for the surviving column")
	require.Len(t, sd, 1)
	assert.Equal(t, "id", sd[0].Key)

	// All-empty sort: no sort at all.
	f2 := New()
	require.NoError(t, f2.AddFiltersFromString(`a=1`))
	f2.Build(MongoAdapter{})
	f2.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}}})
	assert.Nil(t, BuildMongoFindOptions(f2).Sort)
}
