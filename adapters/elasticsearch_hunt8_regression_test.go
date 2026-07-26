package adapters

// Regression tests for the Elasticsearch findings of bug hunt #8.
// Each test is named for its finding id. The truth-table instrument that these
// share lives in elasticsearch_oracle_test.go.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// esHunt8Matches evaluates the document the adapter emits for dsl against the
// oracle corpus and returns the ids Elasticsearch would return.
func esHunt8Matches(t *testing.T, dsl string) []int {
	t.Helper()
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	require.NoError(t, f.BuildE(RawAdapter{}))
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	ids := []int{}
	for _, r := range esOracleRows() {
		ok, err := esEvalClause(q.Query, r.vals)
		require.NoError(t, err, "evaluating %q -> %v", dsl, q.Query)
		if ok {
			ids = append(ids, r.id)
		}
	}
	return ids
}

// esHunt8Body renders the compact request body for an already-built instance.
func esHunt8Body(t *testing.T, f figo.Figo) string {
	t.Helper()
	s, err := GetElasticsearchQueryStringCompact(f)
	require.NoError(t, err)
	return s
}

// D1: the match_none sentinel that fixes M8/M9/A7-6 encodes SQL's UNKNOWN as
// FALSE, and the two are not interchangeable under negation — bool.must_not of a
// clause matching zero documents excludes nothing and therefore matches EVERY
// document. `not a<nin>[null]` returned the whole index with BuildE()==nil and
// Err()==nil where SQL returns no rows.
//
// The sentinel can sit at ANY depth below the negation, so guarding only the
// NotExpr's immediate operand is not enough.
func TestD1_ESUnknownSentinelDoesNotInvertUnderNot(t *testing.T) {
	all := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	none := []int{}

	// The corpus rows are a in {1,2,3} x b in {1,2} x c in {1,3}, ids 1..12 in
	// that order; d = (a+b)%3+1. a=1 -> 1..4, a=2 -> 5..8, a=3 -> 9..12.
	for _, tc := range []struct {
		dsl  string
		want []int
		why  string
	}{
		// depth 1 — the finder's cases
		{"not a<nin>[null]", none, "NOT(a NOT IN (NULL)) is UNKNOWN for every row"},
		{"not a<in>[null]", none, "NOT(a IN (NULL)) is UNKNOWN for every row"},
		{"not a>null", none, "NOT(a > NULL) is UNKNOWN for every row"},
		{"not a<bet>(null..null)", none, "both bounds UNKNOWN"},

		// depth 1, but the predicate is FALSE for some rows rather than UNKNOWN
		// for all — a blanket match_none would be wrong in the other direction.
		{"not a<in>[1,null]", none, "a IN (1,NULL) is TRUE or UNKNOWN, never FALSE"},
		{"not a<nin>[2,null]", []int{5, 6, 7, 8}, "a NOT IN (2,NULL) is FALSE exactly where a=2"},
		{"not a<bet>(null..2)", []int{9, 10, 11, 12}, "BETWEEN NULL AND 2 is FALSE exactly where a>2"},
		{"not a<bet>(2..null)", []int{1, 2, 3, 4}, "BETWEEN 2 AND NULL is FALSE exactly where a<2"},

		// depth 2+ — the completeness critic's cases, all fail-open before the fix
		{"not (a=1 or b>null)", none, "NOT(TRUE OR UNKNOWN)=FALSE; NOT(FALSE OR UNKNOWN)=UNKNOWN"},
		{"not (a=1 and b>null)", []int{5, 6, 7, 8, 9, 10, 11, 12}, "FALSE AND UNKNOWN is FALSE, so NOT is TRUE where a<>1"},
		{"not (c>null or d>null)", none, "every disjunct UNKNOWN"},
		{"not not a>null", none, "NOT NOT UNKNOWN is UNKNOWN"},
		{"not not not a>null", none, "still UNKNOWN at depth 3"},
		{"not (not (a=1 or a>null) and b>1)", []int{1, 2, 3, 4, 5, 6, 9, 10}, "sentinel three levels below the outer NOT; TRUE where a=1 or b<=1"},

		// The two carve-outs: a genuinely FALSE clause must keep negating to TRUE.
		{"not a<in>[]", all, "IN () is FALSE, so its negation matches everything"},
		{"not not a<in>[]", none, "and double negation returns to FALSE"},
		{"not (a<in>[] or a>null)", none, "FALSE OR UNKNOWN is UNKNOWN"},
		{"not (a<in>[] and a>null)", all, "FALSE AND UNKNOWN is FALSE, so NOT is TRUE"},

		// Controls the parent got right and that must not move.
		{"not a=1", []int{5, 6, 7, 8, 9, 10, 11, 12}, "ordinary negation is untouched"},
		{"not (a=1 or b>1)", []int{5, 6, 9, 10}, "sentinel-free nesting is untouched"},
		{"not a<nin>[2]", []int{5, 6, 7, 8}, "null-free NOT IN is untouched"},
	} {
		t.Run(tc.dsl, func(t *testing.T) {
			assert.Equal(t, tc.want, esHunt8Matches(t, tc.dsl), tc.why)
		})
	}
}

// D1, second carve-out: the empty figo.OrExpr is the library-wide
// match-NOTHING sentinel (raw/GORM render 1=0) and guardPluginPanic leaves
// exactly that shape behind. It is genuinely FALSE, not UNKNOWN, so it must keep
// rendering match_none AND must keep negating to match-everything.
func TestD1_ESEmptyOrExprSentinelStaysFalseNotUnknown(t *testing.T) {
	f := figo.New()
	f.AddFilter(figo.OrExpr{})
	f.Build(ElasticsearchAdapter{})
	assert.Contains(t, esHunt8Body(t, f), `"match_none"`,
		"the empty-OrExpr sentinel must still render match_none")

	n := figo.New()
	n.AddFilter(figo.NotExpr{Operands: []figo.Expr{figo.OrExpr{}}})
	n.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(n)
	require.NoError(t, err)
	for _, r := range esOracleRows() {
		ok, err := esEvalClause(q.Query, r.vals)
		require.NoError(t, err)
		assert.True(t, ok, "NOT(1=0) is TRUE: doc %d must match %v", r.id, q.Query)
	}
}

// D1, complexity guard: the three-valued negation must stay LINEAR in the
// expression tree.
//
// The obvious shape of the D1 fix — render the operands positively to discover
// the UNKNOWN taint, then re-render them negated — costs 2x the subtree at every
// nested negation, i.e. 2^depth. It made a 30-deep `not (… and b=i)` chain take
// 4.2 seconds and a 60-deep `not` chain never finish, turning a fail-open bug
// into a DSL-reachable CPU exhaustion. esRenderExpr therefore produces the
// positive and negated clauses in ONE pass, and only inside NOT subtrees.
func TestD1_ESNegationRenderStaysLinear(t *testing.T) {
	nest := "a>null"
	for i := 0; i < 30; i++ {
		nest = fmt.Sprintf("not (%s and b=%d)", nest, i)
	}
	for _, dsl := range []string{nest, strings.Repeat("not ", 60) + "a>null"} {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		require.NoError(t, f.BuildE(ElasticsearchAdapter{}))
		start := time.Now()
		_, err := BuildElasticsearchQuery(f)
		elapsed := time.Since(start)
		require.NoError(t, err)
		// The quadratic/exponential shapes take seconds here; a linear one takes
		// microseconds. 250ms leaves three orders of magnitude of slack for a
		// loaded CI box while still failing an exponential rewrite.
		assert.Less(t, elapsed, 250*time.Millisecond,
			"rendering a %d-node negation nest took %v — the negation is no longer linear", len(dsl), elapsed)
	}
}

// ES-3: SetPagination's deferred error was written into the same field FromFigo
// uses and never cleared on the valid path, so one bad ?from= pinned the builder
// to match_none forever — reintroducing, one method below it, exactly the
// stickiness the A7-3 fix removed from FromFigo. The only reset was FromFigo,
// which replaces the whole query, so a pagination-only mistake was unrecoverable.
func TestES3_ESBuilderPaginationErrorIsNotSticky(t *testing.T) {
	b := NewElasticsearchQueryBuilder()
	b.SetPagination(-1, 10)
	require.Error(t, b.Err(), "a negative from must still be diagnosed")

	// The caller validates its input and retries.
	b.SetPagination(0, 10)
	require.NoError(t, b.Err(), "a valid SetPagination must clear its own deferred error")

	s, err := b.ToJSONCompact()
	require.NoError(t, err)
	assert.Contains(t, s, `"match_all"`, "the corrected builder must emit its real query: %s", s)
	assert.Contains(t, s, `"size":10`)
	assert.NotContains(t, s, "match_none")

	// Clearing the pagination error must NOT clear a FromFigo error, which means
	// the builder holds no usable query at all.
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:b=2]`))
	f.Build(ElasticsearchAdapter{})
	b2 := NewElasticsearchQueryBuilder().FromFigo(f)
	require.Error(t, b2.Err())
	b2.SetPagination(3, 7)
	require.Error(t, b2.Err(), "a valid SetPagination must not erase a FromFigo error")
	q, err := b2.BuildE()
	require.Error(t, err)
	assert.Contains(t, q.Query, "match_none")

	// And FromFigo still resets a pagination error, as its doc comment promises.
	good := figo.New()
	require.NoError(t, good.AddFiltersFromString(`a=1`))
	good.Build(ElasticsearchAdapter{})
	b3 := NewElasticsearchQueryBuilder()
	b3.SetPagination(-5, -5)
	require.Error(t, b3.Err())
	b3.FromFigo(good)
	require.NoError(t, b3.Err(), "FromFigo must clear an error deferred by SetPagination")
}

// ES-7: the M10 LIKE fix formats a non-string value with %v before escaping, but
// fmt.Sprintf("%v", []byte("a*b")) is the decimal element list — so a []byte
// value, the one type the fix's own comment names, searched for a string no
// document contains, while the SQL adapters bind the bytes and match the pattern.
func TestES7_ESLikePatternHandlesByteSlice(t *testing.T) {
	assert.Equal(t, `a\*b?c`, sqlLikeToESWildcard([]byte("a*b_c")),
		"a []byte LIKE value must be read as text, not dumped as decimal bytes")
	assert.Equal(t, `.*(a*b).*`, esRegexpContains([]byte("a*b")),
		"the regexp path has the identical %v fallback")

	// The whole-document view, and parity with the raw adapter's bound pattern.
	f := figo.New()
	f.AddFilter(figo.LikeExpr{Field: "a", Value: []byte("a*b_c")})
	f.Build(ElasticsearchAdapter{})
	body := esHunt8Body(t, f)
	assert.NotContains(t, body, "97 42 98", "the decimal byte dump must not reach the wire: %s", body)
	assert.Contains(t, body, `a\\*b?c`, body)

	// The Stringer half of the M10 fix must keep working.
	assert.Equal(t, `a\*b?c`, sqlLikeToESWildcard(esHunt8Star{}))
}

type esHunt8Star struct{}

func (esHunt8Star) String() string { return "a*b_c" }

// ES-6 (the one half the completeness critic did not dismiss): a negative
// GeoDistanceExpr.Distance rendered "-5km", which Elasticsearch's DistanceUnit
// parser rejects, while Build/BuildE/Err all reported success. Same class as the
// non-finite guard beside it, and the Mongo adapter grew the identical check in
// the same commit citing this adapter as its model.
func TestES6_ESRejectsNegativeGeoDistance(t *testing.T) {
	f := figo.New()
	f.AddFilter(figo.GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: -5})
	f.Build(ElasticsearchAdapter{})
	_, err := BuildElasticsearchQuery(f)
	require.Error(t, err, "a negative distance must be diagnosed, not rendered as \"-5km\"")
	assert.Contains(t, fmt.Sprint(err), "negative")

	// Zero and positive distances are untouched.
	for _, d := range []float64{0, 5} {
		ok := figo.New()
		ok.AddFilter(figo.GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: d})
		ok.Build(ElasticsearchAdapter{})
		_, err := BuildElasticsearchQuery(ok)
		require.NoError(t, err, "distance %v must still render", d)
	}
}

// ES-4 (range half only): esJSONPath received none of the nil-operand guards the
// four top-level range operators grew, so a nil Value rendered
// {"range":{"data.score":{"gt":null}}} — an UNBOUNDED range, i.e. every document
// HAVING the field, where SQL matches none. Pre-existing rather than a
// regression, but the same widening class and the same fix.
func TestES4_ESJSONPathNilRangeOperandFailsClosed(t *testing.T) {
	for _, op := range []string{">", ">=", "<", "<="} {
		f := figo.New()
		f.AddFilter(figo.JsonPathExpr{Field: "data", Path: "$.score", Op: op, Value: nil})
		f.Build(ElasticsearchAdapter{})
		body := esHunt8Body(t, f)
		assert.Contains(t, body, `"match_none"`, "op %q must fail closed, got %s", op, body)
		assert.NotContains(t, body, "null", "no unbounded range bound may reach the wire: %s", body)

		// And the sentinel must not invert under a negation either.
		n := figo.New()
		n.AddFilter(figo.NotExpr{Operands: []figo.Expr{
			figo.JsonPathExpr{Field: "data", Path: "$.score", Op: op, Value: nil},
		}})
		n.Build(ElasticsearchAdapter{})
		q, err := BuildElasticsearchQuery(n)
		require.NoError(t, err)
		for _, r := range esOracleRows() {
			ok, err := esEvalClause(q.Query, r.vals)
			require.NoError(t, err)
			assert.False(t, ok, "NOT(UNKNOWN) must match nothing for op %q: %v", op, q.Query)
		}
	}

	// A non-nil operand still renders the range.
	f := figo.New()
	f.AddFilter(figo.JsonPathExpr{Field: "data", Path: "$.score", Op: ">", Value: 3})
	f.Build(ElasticsearchAdapter{})
	assert.Contains(t, esHunt8Body(t, f), `"range":{"data.score":{"gt":3}}`)
}
