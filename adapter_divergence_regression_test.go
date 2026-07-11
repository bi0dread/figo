package figo

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// NotExpr{a,b} means "none match" — every adapter must implement NOT(a OR b).
func TestNotExprSemanticsConsistent(t *testing.T) {
	newNot := func() Figo {
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: int64(1)},
			EqExpr{Field: "b", Value: int64(2)},
		}})
		return f
	}

	t.Run("raw", func(t *testing.T) {
		f := newNot()
		f.Build(RawAdapter{})
		sql, args := BuildRawWhere(f)
		assert.Equal(t, "NOT ((`a` = ? OR `b` = ?))", sql)
		assert.Equal(t, []any{int64(1), int64(2)}, args)
	})

	t.Run("elasticsearch keeps every operand", func(t *testing.T) {
		f := newNot()
		f.Build(ElasticsearchAdapter{})
		q, err := BuildElasticsearchQuery(f)
		require.NoError(t, err)
		js, _ := json.Marshal(q)
		assert.Contains(t, string(js), `"a":1`)
		assert.Contains(t, string(js), `"b":2`, "second NOT operand must not be dropped")
	})

	t.Run("mongo", func(t *testing.T) {
		f := newNot()
		f.Build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.NoError(t, err)
		nor, ok := filter["$nor"].([]bson.M)
		require.True(t, ok)
		assert.Len(t, nor, 2)
	})
}

// Empty IN/NOT-IN lists must produce valid, match-nothing/match-everything
// queries on Mongo and Elasticsearch (nil slices marshaled to null, which
// both servers reject at runtime).
func TestEmptyInListsValidOnMongoAndES(t *testing.T) {
	t.Run("mongo empty $in is an array", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`id<in>[]`))
		f.Build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.NoError(t, err)
		raw, err := bson.MarshalExtJSON(filter, false, false)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"$in":[]`, "must be an empty array, not null: %s", raw)
	})

	t.Run("es empty terms becomes match_none", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`id<in>[]`))
		f.Build(ElasticsearchAdapter{})
		q, err := BuildElasticsearchQuery(f)
		require.NoError(t, err)
		js, _ := json.Marshal(q)
		assert.Contains(t, string(js), "match_none")
		assert.NotContains(t, string(js), "null")
	})
}

// A literal '*' in a LIKE value must not become a wildcard on Elasticsearch.
func TestESWildcardEscapesLiteralStars(t *testing.T) {
	assert.Equal(t, `100*\*off`, sqlLikeToESWildcard("100%*off"))
	assert.Equal(t, `a\?b?c`, sqlLikeToESWildcard("a?b_c"))
}

// conditionType "LIMIT" must not leak OFFSET; "LIMIT","OFFSET" must not
// duplicate it.
func TestRawLimitOffsetSegments(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.SetPage(10, 5)
	f.Build(RawAdapter{})

	limitOnly := f.GetSqlString(RawContext{Table: "t"}, "SELECT", "FROM", "LIMIT")
	assert.Contains(t, limitOnly, "LIMIT 5")
	assert.NotContains(t, limitOnly, "OFFSET")

	both := f.GetSqlString(RawContext{Table: "t"}, "SELECT", "FROM", "LIMIT", "OFFSET")
	assert.Equal(t, 1, strings.Count(both, "OFFSET"), "OFFSET duplicated: %s", both)
}

// A custom naming func must be honored by adapter-side column normalization,
// not undone by re-applying the snake_case strategy.
func TestNamingFuncHonoredByAdapters(t *testing.T) {
	f := New()
	f.SetNamingFunc(func(s string) string { return s }) // identity: preserve camelCase
	require.NoError(t, f.AddFiltersFromString(`userName="x"`))
	f.Build(RawAdapter{})

	assert.Equal(t, "userName", normalizeColumnName(f, "userName"))
	sql, _ := BuildRawWhere(f)
	assert.Contains(t, sql, "`userName`")
}

// take=0 means "no limit" on every adapter; GORM must not emit LIMIT 0.
func TestGormTakeZeroMeansNoLimit(t *testing.T) {
	db := newGormRegDB(t)
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name="x"`))
	f.SetPage(0, 0)
	f.Build(GormAdapter{})

	sql := f.GetSqlString(db.Model(&gormRegModel{}))
	assert.NotContains(t, sql, "LIMIT 0", "take=0 must not become LIMIT 0: %s", sql)
}

// The batch concurrency cap must hold even when operations time out.
func TestBatchTimeoutKeepsConcurrencyCap(t *testing.T) {
	var inFlight, maxInFlight int64
	blocker := &slowAdapter{
		onCall: func() {
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				max := atomic.LoadInt64(&maxInFlight)
				if cur <= max || atomic.CompareAndSwapInt64(&maxInFlight, max, cur) {
					break
				}
			}
			time.Sleep(120 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
		},
	}

	ops := make([]BatchOperation, 3)
	for i := range ops {
		f := New()
		f.AddFilter(EqExpr{Field: "a", Value: int64(1)})
		f.SetAdapterObject(blocker)
		ops[i] = BatchOperation{ID: "op", Type: "sql", Query: f, Context: "t"}
	}

	bp := NewInMemoryBatchProcessor(1, 20*time.Millisecond)
	results := bp.Process(ops)

	for _, r := range results {
		assert.Error(t, r.Error, "each op should time out")
	}
	// Give stragglers a moment to finish, then check the observed peak.
	time.Sleep(500 * time.Millisecond)
	assert.LessOrEqual(t, atomic.LoadInt64(&maxInFlight), int64(1),
		"maxConcurrency=1 must hold even under timeouts")
}

type slowAdapter struct {
	onCall func()
}

func (s *slowAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
	s.onCall()
	return "SELECT 1", true
}

func (s *slowAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	s.onCall()
	return SQLQuery{SQL: "SELECT 1"}, true
}

// AddFilter must respect the whitelist and ignore-fields, including for the
// advanced expression types.
func TestAddFilterRespectsWhitelistAndIgnores(t *testing.T) {
	f := New()
	f.SetAllowedFields("name")
	f.EnableFieldWhitelist()
	f.AddFilter(EqExpr{Field: "secret", Value: 1})
	f.AddFilter(GeoDistanceExpr{Field: "hidden_location", Latitude: 1, Longitude: 2, Distance: 3})
	f.AddFilter(EqExpr{Field: "name", Value: "x"})
	require.Len(t, f.GetClauses(), 1)
	assert.Equal(t, "name", exprField(f.GetClauses()[0]))

	f2 := New()
	f2.AddIgnoreFields("secret")
	f2.AddFilter(NotExpr{Operands: []Expr{EqExpr{Field: "secret", Value: 1}}})
	assert.Empty(t, f2.GetClauses(), "ignored field must not enter via AddFilter")
}
