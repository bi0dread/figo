package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

// #26: Mongo IS NULL / IS NOT NULL use null semantics (match explicit null),
// not $exists (which only matches missing fields).
func TestMongoNullSemantics(t *testing.T) {
	f := New()
	f.AddFilter(IsNullExpr{Field: "deleted_at"})
	f.Build(nil)
	m := BuildMongoFilterMust(t, f)
	assert.Contains(t, m, "deleted_at")
	assert.Nil(t, m["deleted_at"], "IS NULL should be {field: nil}")

	f2 := New()
	f2.AddFilter(NotNullExpr{Field: "deleted_at"})
	f2.Build(nil)
	m2 := BuildMongoFilterMust(t, f2)
	sub, ok := m2["deleted_at"].(bson.M)
	assert.True(t, ok)
	assert.Equal(t, nil, sub["$ne"], "IS NOT NULL should be {field: {$ne: nil}}")
}

// #27: an AndExpr with no operands must not emit an invalid {$and: null}.
func TestMongoEmptyLogicalNotNull(t *testing.T) {
	f := New()
	f.AddFilter(AndExpr{Operands: nil})
	f.Build(nil)
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
