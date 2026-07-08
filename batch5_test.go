package figo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

// #22: a zero timeout must not fail an operation that completed successfully.
func TestBatchZeroTimeoutSucceeds(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`id=1`)
	f.Build(RawAdapter{})

	bp := NewInMemoryBatchProcessor(2, 0) // 0 => no timeout
	results := bp.Process([]BatchOperation{
		{ID: "1", Query: f, Context: "t", Type: "sql"},
	})
	assert.Len(t, results, 1)
	assert.True(t, results[0].Success, "zero timeout must not fail a completed op")
	assert.NoError(t, results[0].Error)
	assert.NotEmpty(t, results[0].Result)
}

// #26: Mongo IS NULL / IS NOT NULL use null semantics (match explicit null),
// not $exists (which only matches missing fields).
func TestMongoNullSemantics(t *testing.T) {
	f := New()
	f.AddFilter(IsNullExpr{Field: "deleted_at"})
	f.Build()
	m := BuildMongoFilterMust(t, f)
	assert.Contains(t, m, "deleted_at")
	assert.Nil(t, m["deleted_at"], "IS NULL should be {field: nil}")

	f2 := New()
	f2.AddFilter(NotNullExpr{Field: "deleted_at"})
	f2.Build()
	m2 := BuildMongoFilterMust(t, f2)
	sub, ok := m2["deleted_at"].(bson.M)
	assert.True(t, ok)
	assert.Equal(t, nil, sub["$ne"], "IS NOT NULL should be {field: {$ne: nil}}")
}

// #27: an AndExpr with no operands must not emit an invalid {$and: null}.
func TestMongoEmptyLogicalNotNull(t *testing.T) {
	f := New()
	f.AddFilter(AndExpr{Operands: nil})
	f.Build()
	m := BuildMongoFilterMust(t, f)
	_, has := m["$and"]
	assert.False(t, has, "empty AND must not produce {$and: null}")
}

func BuildMongoFilterMust(t *testing.T, f Figo) bson.M {
	t.Helper()
	m, err := BuildMongoFilter(f)
	assert.NoError(t, err)
	return m
}

// #28: OFFSET without LIMIT must include a LIMIT (MySQL/SQLite reject bare OFFSET).
func TestOffsetWithoutLimitIncludesLimit(t *testing.T) {
	f := New()
	f.SetPage(5, 0) // skip 5, take 0
	f.Build(RawAdapter{})
	sql, _ := BuildRawSelect(f, "t")
	assert.Contains(t, sql, "OFFSET 5")
	assert.Contains(t, sql, "LIMIT", "bare OFFSET is invalid on MySQL/SQLite")
}

// #29: column and JOIN ordering is deterministic across builds.
func TestDeterministicColumnAndJoinOrder(t *testing.T) {
	build := func() string {
		f := New()
		f.AddSelectFields("zeta", "alpha", "mu")
		f.Build(RawAdapter{})
		sql, _ := BuildRawSelect(f, "t")
		return sql
	}
	first := build()
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, build(), "column order must be stable")
	}
	// Columns come out sorted.
	assert.Contains(t, first, "`alpha`, `mu`, `zeta`")
}

// #30: an expired entry is removed on access and no longer counted in Size.
func TestExpiredEntryDeletedOnGet(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 10}) // no cleanup goroutine
	defer c.Close()
	c.Set("k", "v", time.Nanosecond)
	time.Sleep(time.Millisecond)

	_, ok := c.Get("k")
	assert.False(t, ok, "expired entry must be a miss")
	assert.Equal(t, 0, c.Stats().Size, "expired entry must be deleted, not linger")
}
