package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// An OrExpr with no renderable operands is a false disjunction and must match
// NOTHING on every adapter. The SQL adapters used to render it as "" — the
// predicate vanished, so a top-level empty OR matched every row (Mongo and ES
// already matched nothing; pinned in bughunt_round2_regression_test.go).
func TestEmptyOrExprFailsClosedOnSQLAdapters(t *testing.T) {
	t.Run("raw top-level", func(t *testing.T) {
		f := New()
		f.AddFilter(OrExpr{})
		f.Build(RawAdapter{})
		sql, args, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "1=0", sql)
		assert.Empty(t, args)
	})

	t.Run("raw nested inside AND", func(t *testing.T) {
		f := New()
		f.AddFilter(AndExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: int64(1)},
			OrExpr{},
		}})
		f.Build(RawAdapter{})
		sql, args, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "(`a` = ? AND 1=0)", sql)
		assert.Equal(t, []any{int64(1)}, args)
	})

	t.Run("gorm", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		f := New()
		f.AddFilter(OrExpr{})
		f.Build(GormAdapter{})
		sql := f.GetSqlString(db.Model(&failClosedModel{}))
		assert.Contains(t, sql, "1=0", "empty OR must not vanish from the WHERE clause; got %q", sql)
	})

	// The identity cases stay identities: empty AND and empty NOT render no
	// predicate (match-all), consistent with Mongo's {} for $and/$nor.
	t.Run("raw empty AND and NOT stay match-all", func(t *testing.T) {
		f := New()
		f.AddFilter(AndExpr{})
		f.AddFilter(NotExpr{})
		f.Build(RawAdapter{})
		sql, _, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "", sql)
	})
}

type failClosedModel struct {
	ID   uint
	Name string
}

// The parser applies the NamingFunc when it builds the AST; adapters must use
// field names VERBATIM. The GORM adapter re-applied the func — invisible with
// the idempotent snake_case default, but a prefixing (non-idempotent) func
// rendered t_t_age while raw/Mongo rendered t_age.
func TestNamingFuncAppliedExactlyOnceAcrossAdapters(t *testing.T) {
	prefix := func(s string) string { return "t_" + s }
	build := func(adapter Adapter) Figo {
		f := New()
		f.SetNamingFunc(prefix)
		require.NoError(t, f.AddFiltersFromString(`age>5 sort=age:desc`))
		f.Build(adapter)
		return f
	}

	t.Run("raw", func(t *testing.T) {
		f := build(RawAdapter{})
		sql, _, err := BuildRawSelect(f, "users")
		require.NoError(t, err)
		assert.Contains(t, sql, "`t_age` > ?")
		assert.Contains(t, sql, "ORDER BY `t_age` DESC")
	})

	t.Run("gorm", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		f := build(GormAdapter{})
		sql := f.GetSqlString(db.Model(&failClosedModel{}))
		assert.Contains(t, sql, "t_age")
		assert.NotContains(t, sql, "t_t_age", "naming func must not be applied twice")
	})

	t.Run("mongo", func(t *testing.T) {
		f := build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.NoError(t, err)
		_, ok := filter["t_age"]
		assert.True(t, ok, "expected filter keyed by t_age, got %v", filter)
	})
}

// With a caller *gorm.DB opened with a global DryRun:true, the segment path
// used to append the requested clause to the retained full SELECT — and its
// binds to Vars, duplicating every arg on the GetQuery path.
func TestGormSegmentRenderWithCallerDryRunDB(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true, Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)

	newF := func() Figo {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`name="x"`))
		f.Build(GormAdapter{})
		return f
	}

	t.Run("segment SQL is only the segment", func(t *testing.T) {
		sql := newF().GetSqlString(db.Model(&failClosedModel{}), "WHERE")
		assert.Contains(t, sql, "WHERE")
		assert.NotContains(t, sql, "SELECT", "segment render must not retain the full query: %q", sql)
	})

	t.Run("segment args are not duplicated", func(t *testing.T) {
		q, ok := newF().GetQuery(db.Model(&failClosedModel{}), "WHERE").(SQLQuery)
		require.True(t, ok)
		assert.Equal(t, []any{"x"}, q.Args, "binds must appear exactly once")
	})
}

// A field name in operator position would execute as a MongoDB operator
// ({"$where": ...} runs server-side JS). The default naming can't produce
// one, but NoChangeNaming plus attacker-influenced field names could.
func TestMongoRejectsOperatorFieldNames(t *testing.T) {
	t.Run("$where rejected", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		f.AddFilter(EqExpr{Field: "$where", Value: "sleep(1000)"})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MongoDB operator")
	})

	t.Run("underscore and dotted fields still fine", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		f.AddFilter(EqExpr{Field: "_id", Value: "507f1f77bcf86cd799439011"})
		f.AddFilter(EqExpr{Field: "user.age", Value: int64(3)})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.NoError(t, err)
	})
}

// Dotted field names (set via Walk/SetNodeField to qualify columns, per the
// README recipe) must render as `table`.`column`, not one quoted identifier
// naming a nonexistent dotted column.
func TestDottedFieldNamesQuotedPerSegment(t *testing.T) {
	newDotted := func() Figo {
		f := New()
		f.AddFilter(EqExpr{Field: "users.first_name", Value: "jo"})
		return f
	}

	t.Run("raw mysql", func(t *testing.T) {
		f := newDotted()
		f.Build(RawAdapter{})
		sql, _, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "`users`.`first_name` = ?", sql)
	})

	t.Run("raw postgres", func(t *testing.T) {
		f := newDotted()
		f.Build(RawAdapter{Dialect: PostgresDialect})
		sql, _, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, `"users"."first_name" = $1`, sql)
	})

	t.Run("gorm", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		f := newDotted()
		f.Build(GormAdapter{})
		sql := f.GetSqlString(db.Model(&failClosedModel{}))
		assert.True(t,
			strings.Contains(sql, "`users`.`first_name`") || strings.Contains(sql, `"users"."first_name"`),
			"expected per-segment quoting, got %q", sql)
	})

	// Injection hardening is unchanged: embedded quote runes in each segment
	// are still doubled, so a crafted name cannot break out of the quoting.
	t.Run("quote runes still escaped per segment", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		f.AddFilter(EqExpr{Field: "a`b.c", Value: int64(1)})
		f.Build(RawAdapter{})
		sql, _, err := BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "`a``b`.`c` = ?", sql)
	})
}
