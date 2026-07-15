package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// #1: a qualified/kebab/$-suffixed field followed by a SPACED operator must
// parse like its no-space form, not emit a predicate on an empty column.
func TestRegr_SpacedOperatorWithSpecialFieldNames(t *testing.T) {
	cases := []struct {
		dsl      string
		wantCol  string
		wantArgs []any
	}{
		{`user.age > 5`, "`user_age` > ?", []any{int64(5)}},
		{`profile.bio =^ "%dev%"`, "`profile_bio` LIKE ?", []any{"%dev%"}},
		{`user-name = "jo"`, "`user_name` = ?", []any{"jo"}}, // snake_case normalizes '-' to '_'
	}
	for _, tc := range cases {
		f := New()
		require.NoError(t, f.AddFiltersFromString(tc.dsl))
		f.Build(RawAdapter{})
		where, args := BuildRawWhere(f)
		assert.Equal(t, tc.wantCol, where, "dsl=%q", tc.dsl)
		assert.Equal(t, tc.wantArgs, args, "dsl=%q", tc.dsl)
		assert.NotContains(t, where, "`` ", "empty column leaked for %q", tc.dsl)
	}

	// <bet> with a dotted field must not drop the whole filter.
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a.b <bet> (1..2)`))
	f.Build(RawAdapter{})
	where, args := BuildRawWhere(f)
	assert.Equal(t, "`a_b` BETWEEN ? AND ?", where)
	assert.Equal(t, []any{int64(1), int64(2)}, args)
}

func TestRegr_MongoProjectionFromSelectFields(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.AddSelectFields("id", "name")
	f.Build(MongoAdapter{})
	opts := BuildMongoFindOptions(f)
	require.NotNil(t, opts.Projection, "projection must be set when select fields are present")
	proj, _ := json.Marshal(opts.Projection)
	assert.Contains(t, string(proj), `"id":1`)
	assert.Contains(t, string(proj), `"name":1`)
}

// #4: empty NOT IN matches all rows (1=1) and empty IN matches none (1=0) on
// GORM, consistent with the raw adapter — not "IS NOT NULL".
func TestRegr_GormEmptyInNotInSemantics(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)

	fNin := New()
	fNin.AddFilter(NotInExpr{Field: "id", Values: []any{}})
	fNin.Build(GormAdapter{})
	assert.Contains(t, fNin.GetSqlString(db.Table("t"), "WHERE"), "1=1")
	assert.NotContains(t, fNin.GetSqlString(db.Table("t"), "WHERE"), "IS NOT NULL")

	fIn := New()
	fIn.AddFilter(InExpr{Field: "id", Values: []any{}})
	fIn.Build(GormAdapter{})
	assert.Contains(t, fIn.GetSqlString(db.Table("t"), "WHERE"), "1=0")
}

// #5: Elasticsearch =~ must be an unanchored contains match (like Mongo), not
// Lucene's implicit full-value anchor.
func TestRegr_ElasticsearchRegexIsContains(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=~foo`))
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	b, _ := json.Marshal(q.Query)
	assert.Contains(t, string(b), `.*(foo).*`, "ES regexp must be wrapped for contains semantics")
}

// #6: an empty OR must match NOTHING on Mongo (not the whole collection).
func TestRegr_MongoEmptyOrMatchesNothing(t *testing.T) {
	f := New()
	f.AddFilter(OrExpr{Operands: nil})
	f.Build(MongoAdapter{})
	m, err := BuildMongoFilter(f)
	require.NoError(t, err)
	_, isMatchAll := m["$nor"]
	assert.True(t, isMatchAll, "empty OR must render a match-nothing predicate, got %v", m)
	assert.NotEqual(t, 0, len(m), "empty OR must not render {} (match everything)")
}

// #7: requesting a segment and its alias (WHERE+LIKE, SORT+ORDER BY) must emit
// the clause and its args once, not twice.
func TestRegr_RawSegmentAliasNoDuplication(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`age=30 sort=id:desc`))
	f.Build(RawAdapter{})

	q := f.GetQuery("t", "SELECT", "FROM", "WHERE", "LIKE").(SQLQuery)
	assert.Equal(t, 1, strings.Count(q.SQL, "WHERE"), "WHERE must appear once: %q", q.SQL)
	assert.Len(t, q.Args, 1, "args must not be duplicated: %v", q.Args)

	s := f.GetSqlString("t", "SORT", "ORDER BY")
	assert.Equal(t, 1, strings.Count(s, "ORDER BY"), "ORDER BY must appear once: %q", s)
}

// #8: a stray OrderBy in the clause list must not fail the ES build.
func TestRegr_ElasticsearchToleratesOrderByClause(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "a", Value: 1})
	f.AddFilter(OrderBy{Columns: []OrderByColumn{{Name: "a", Desc: true}}})
	f.Build(ElasticsearchAdapter{})
	_, err := BuildElasticsearchQuery(f)
	assert.NoError(t, err, "ES must tolerate an OrderBy clause like the Mongo adapter does")
}

// #9: a '?' inside a backtick identifier must stay in the identifier on the
// display path, and the real placeholder must still be interpolated.
func TestRegr_ExpandPlaceholdersIgnoresQuestionInIdentifier(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "a?b", Value: "v"})
	f.Build(RawAdapter{})
	sql := f.GetSqlString("t")
	assert.Contains(t, sql, "`a?b`", "identifier must stay intact: %q", sql)
	assert.Contains(t, sql, "`a?b` = 'v'", "value must interpolate at the real placeholder: %q", sql)
}
