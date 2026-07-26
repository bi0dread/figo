package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// esRender builds one clause and returns its rendered query body.
func esRender(t *testing.T, e Expr) map[string]interface{} {
	t.Helper()
	f := New()
	f.AddFilter(e)
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	return q.Query
}

// esRenderDSL builds a DSL string and returns its rendered query body.
func esRenderDSL(t *testing.T, dsl string) map[string]interface{} {
	t.Helper()
	f := New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	return q.Query
}

func esJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// H1: OrExpr with no renderable operands is figo's library-wide match-NOTHING
// sentinel (raw/GORM `1=0`, Mongo `{"$nor":[{}]}`) and is what guardPluginPanic
// leaves behind after a recovered plugin panic. It rendered as
// {"bool":{"minimum_should_match":1,"should":[]}} — a bool query with no
// clauses is answered with MatchAllDocsQuery *before* minimum_should_match
// applies, so the never-true guard returned the whole index.
func TestH1_ESEmptyOrExprMatchesNothing(t *testing.T) {
	for name, e := range map[string]Expr{
		"bare":         OrExpr{},
		"nil operands": OrExpr{Operands: []Expr{nil, nil}},
	} {
		t.Run(name, func(t *testing.T) {
			q := esRender(t, e)
			assert.Contains(t, q, "match_none", "got %s", esJSON(t, q))
			assert.NotContains(t, esJSON(t, q), "should")
		})
	}

	t.Run("nested beside a tenant scope", func(t *testing.T) {
		f := New()
		f.AddFilter(EqExpr{Field: "tenant_id", Value: "acme"})
		f.AddFilter(OrExpr{})
		f.Build(ElasticsearchAdapter{})
		q, err := BuildElasticsearchQuery(f)
		require.NoError(t, err)
		assert.Contains(t, esJSON(t, q.Query), `{"match_none":{}}`,
			"the sentinel must survive nesting: %s", esJSON(t, q.Query))
	})

	// A single-operand OR still renders as a should/1 disjunction.
	t.Run("non-empty OR unchanged", func(t *testing.T) {
		q := esRender(t, OrExpr{Operands: []Expr{EqExpr{Field: "a", Value: 1}}})
		b := q["bool"].(map[string]interface{})
		assert.Equal(t, 1, b["minimum_should_match"])
		assert.Len(t, b["should"], 1)
	})
}

// H14: Elasticsearch validates from + size against index.max_result_window, so
// rendering the flat window default next to any offset made every
// `skip>=1,take:0` an HTTP 400 for the whole search.
func TestH14_ESFromPlusSizeStaysInResultWindow(t *testing.T) {
	for _, tc := range []struct {
		dsl              string
		wantFrom, wantSz int
	}{
		{`a=1 page=skip:1,take:0`, 1, 9999},
		{`a=1 page=skip:30,take:0`, 30, 9970},
		{`a=1 page=skip:0,take:0`, 0, 10000},
		{`a=1 page=skip:0,take:5`, 0, 5},
	} {
		t.Run(tc.dsl, func(t *testing.T) {
			f := New()
			require.NoError(t, f.AddFiltersFromString(tc.dsl))
			f.Build(ElasticsearchAdapter{})
			q, err := BuildElasticsearchQuery(f)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFrom, q.From)
			assert.Equal(t, tc.wantSz, q.Size)
			assert.LessOrEqual(t, q.From+q.Size, esMaxResultWindow,
				"from + size must not exceed the result window")
		})
	}

	// An offset already past the window cannot be satisfied; the size must not
	// silently fall back to the ES default of 10 hits either.
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1 page=skip:20000,take:0`))
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	assert.Equal(t, 0, q.Size)
	assert.Contains(t, esJSON(t, q), `"size":0`, "an explicit zero size must be rendered")
}

// M8: a null element inside a `terms` query is a ParsingException — HTTP 400
// for the whole search. Dropping it from an any-match set matches SQL
// (`IN (1, NULL)` selects the same rows as `IN (1)`); the all-must-match and
// negated forms fail closed instead, because dropping would widen them.
func TestM8_ESNullInsideTermsLists(t *testing.T) {
	assert.Equal(t, map[string]interface{}{"terms": map[string]interface{}{"id": []any{int64(1)}}},
		esRenderDSL(t, `id<in>[1,null]`))
	assert.Contains(t, esRenderDSL(t, `id<in>[null]`), "match_none")

	// NOT IN with a NULL is never true in SQL, and null is invalid in terms.
	assert.Contains(t, esRenderDSL(t, `id<nin>[1,null]`), "match_none")

	assert.Contains(t, esRender(t, ArrayOverlapsExpr{Field: "tags", Values: []any{nil}}), "match_none")
	assert.Equal(t, map[string]interface{}{"terms": map[string]interface{}{"tags": []any{"a"}}},
		esRender(t, ArrayOverlapsExpr{Field: "tags", Values: []any{"a", nil}}))
	assert.Contains(t, esRender(t, ArrayContainsExpr{Field: "tags", Values: []any{"a", nil}}), "match_none")

	// No null survives into any rendered document.
	for _, dsl := range []string{`id<in>[1,null]`, `id<nin>[1,null]`, `id<in>[null]`} {
		assert.NotContains(t, esJSON(t, esRenderDSL(t, dsl)), "null", "dsl=%s", dsl)
	}
}

// M9: ES documents a null range bound as UNBOUNDED, so a BETWEEN with null
// bounds matched every document having the field, while SQL's BETWEEN with a
// NULL bound is UNKNOWN and matches none. A widening of the predicate itself.
func TestM9_ESNullBetweenBoundFailsClosed(t *testing.T) {
	assert.Contains(t, esRenderDSL(t, `price<bet>(null..null)`), "match_none")
	assert.Contains(t, esRender(t, BetweenExpr{Field: "price", Low: 1, High: nil}), "match_none")
	assert.Contains(t, esRender(t, BetweenExpr{Field: "price", Low: nil, High: 10}), "match_none")

	// A fully bounded range is untouched.
	q := esRenderDSL(t, `price<bet>(1..10)`)
	assert.Equal(t, `{"range":{"price":{"gte":1,"lte":10}}}`, esJSON(t, q))
}

// A7-6: the four range operators passed a nil operand straight into the range
// body, where ES reads it as unbounded ("field exists") — SQL's `a > NULL` is
// UNKNOWN and returns nothing. EqExpr/NeqExpr already canonicalised nil.
func TestA7_6_ESNullRangeOperandFailsClosed(t *testing.T) {
	for _, dsl := range []string{`a>null`, `a>=null`, `a<null`, `a<=null`} {
		t.Run(dsl, func(t *testing.T) {
			q := esRenderDSL(t, dsl)
			assert.Contains(t, q, "match_none", "got %s", esJSON(t, q))
			assert.NotContains(t, esJSON(t, q), "range")
		})
	}

	// The guarded equality forms keep their existing canonicalisation.
	assert.Contains(t, esJSON(t, esRenderDSL(t, `a=null`)), "must_not")
	assert.Contains(t, esJSON(t, esRenderDSL(t, `a!=null`)), "exists")
}

// M10: a non-string LIKE value skipped both the escaping and the %/_
// translation, so a fmt.Stringer rendering '*' became an unescaped ES wildcard
// matching every document — on this adapter alone.
func TestM10_ESNonStringLikeIsEscapedAndTranslated(t *testing.T) {
	q := esRender(t, LikeExpr{Field: "n", Value: esWildcardStringer{}})
	assert.Equal(t, `{"wildcard":{"n":"a\\*b?c"}}`, esJSON(t, q),
		"'*' must be escaped and '_' translated for a non-string value too")

	qi := esRender(t, ILikeExpr{Field: "n", Value: esWildcardStringer{}})
	assert.Contains(t, esJSON(t, qi), `a\\*b?c`)

	// Strings keep behaving exactly as before.
	assert.Equal(t, `{"wildcard":{"n":"a\\*b?c"}}`, esJSON(t, esRender(t, LikeExpr{Field: "n", Value: "a*b_c"})))
}

type esWildcardStringer struct{}

func (esWildcardStringer) String() string { return "a*b_c" }

// A7-4: Lucene's regexp query defaults flags to ALL, which leaves @ ANYSTRING,
// ~ COMPLEMENT, & INTERSECTION and <n-m> INTERVAL live. `.*(@).*` therefore
// matched every value of the field, so `email=~"@"` returned the whole index
// while Mongo and SQL treat the character literally.
func TestA7_4_ESRegexpDisablesLuceneOptionalOperators(t *testing.T) {
	q := esRenderDSL(t, `email=~"@"`)
	body := q["regexp"].(map[string]interface{})["email"].(map[string]interface{})
	assert.Equal(t, "NONE", body["flags"], "optional Lucene operators must be disabled")
	assert.Equal(t, ".*(@).*", body["value"], "contains semantics must be preserved")
}

// L11: the fluent builder dropped size:0 through omitempty, so ES applied its
// default of 10 hits — the exact hazard the adapter path guards — and a
// count-only search was inexpressible.
func TestL11_ESBuilderRendersExplicitSizeZero(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "id", Value: 1})
	f.Build(ElasticsearchAdapter{})

	s, err := NewElasticsearchQueryBuilder().FromFigo(f).SetPagination(0, 0).ToJSONCompact()
	require.NoError(t, err)
	assert.Contains(t, s, `"size":0`, "an explicitly requested count-only size must survive: %s", s)

	// A builder that never sets pagination still omits "size" (ES default).
	s2, err := NewElasticsearchQueryBuilder().ToJSONCompact()
	require.NoError(t, err)
	assert.NotContains(t, s2, "size")
}

// L12: the builder emitted {"":{"order":"asc"}} for an empty field name, an
// invalid sort key that the adapter path already skips defensively.
func TestL12_ESBuilderSkipsEmptySortField(t *testing.T) {
	s, err := NewElasticsearchQueryBuilder().AddSort("", true).AddSort("id", false).ToJSONCompact()
	require.NoError(t, err)
	assert.NotContains(t, s, `""`, "empty sort key leaked: %s", s)
	assert.Contains(t, s, `{"id":{"order":"desc"}}`)
}

// L13: a non-finite Latitude/Longitude made the query unmarshalable while
// Build/BuildE/Err all reported success — Distance was validated, the
// coordinates were not.
func TestL13_ESGeoNonFiniteCoordinateErrors(t *testing.T) {
	for name, e := range map[string]GeoDistanceExpr{
		"lat NaN": {Field: "loc", Latitude: math.NaN(), Longitude: 2, Distance: 5},
		"lon Inf": {Field: "loc", Latitude: 1, Longitude: math.Inf(1), Distance: 5},
	} {
		t.Run(name, func(t *testing.T) {
			f := New()
			f.AddFilter(e)
			f.Build(ElasticsearchAdapter{})
			_, err := BuildElasticsearchQuery(f)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "non-finite")
		})
	}

	f := New()
	f.AddFilter(GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 5})
	f.Build(ElasticsearchAdapter{})
	_, err := BuildElasticsearchQuery(f)
	assert.NoError(t, err, "finite coordinates keep working")
}

// A7-1: the adapter's fail-closed refusal used to reach the caller as ("",
// false); figo.GetSqlString has no error channel and returns "" for ok=false,
// and an empty Elasticsearch request body is match_all — so a DSL containing
// the documented `load=` directive dumped the whole index, tenant scope and
// all. BuildE stays nil for this input, so the documented validation gate does
// not catch it.
func TestA7_1_ESFailClosedNeverRendersAnEmptyBody(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 load=[Orders:id=2]`))
	f.AddFilter(EqExpr{Field: "tenant_id", Value: "acme"})
	require.NoError(t, f.BuildE(ElasticsearchAdapter{}), "BuildE does not diagnose this input")

	body := f.GetSqlString(nil)
	require.NotEmpty(t, body, "an empty ES body is match_all")
	assert.JSONEq(t, `{"query":{"match_none":{}}}`, body)

	// Every other rendering entry point agrees, and the error stays observable
	// wherever there is an error to return.
	q, err := BuildElasticsearchQuery(f)
	require.Error(t, err)
	assert.NotContains(t, esJSON(t, q), `"query":null`, "the error value must not marshal to a null query")
	assert.Contains(t, esJSON(t, q), "match_none")

	s, err := GetElasticsearchQueryStringCompact(f)
	assert.Error(t, err)
	assert.JSONEq(t, `{"query":{"match_none":{}}}`, s)

	// A renderable instance is unaffected.
	ok := New()
	require.NoError(t, ok.AddFiltersFromString(`id=1`))
	ok.Build(ElasticsearchAdapter{})
	assert.Contains(t, ok.GetSqlString(nil), `"term":{"id":1}`)
}

// A7-2: figo.GetQuery returns a nil interface when the adapter reports
// ok=false, so README's documented retrieval pattern
// (`f.GetQuery(nil).(adapters.ElasticsearchQueryWrapper)`) panicked the
// handler goroutine for any `?filter=` containing `load=`.
func TestA7_2_ESGetQueryNeverNilForDocumentedAssertion(t *testing.T) {
	for _, dsl := range []string{`id=1 load=[Orders:id=2]`, `load=[X:a=1]`, `id=1`} {
		t.Run(dsl, func(t *testing.T) {
			f := New()
			require.NoError(t, f.AddFiltersFromString(dsl))
			f.Build(ElasticsearchAdapter{})

			var w ElasticsearchQueryWrapper
			require.NotPanics(t, func() {
				w = f.GetQuery(nil).(ElasticsearchQueryWrapper) // README's verbatim pattern
			})
			assert.NotEmpty(t, w.GetSQL())
			if strings.Contains(dsl, "load=") {
				assert.JSONEq(t, `{"query":{"match_none":{}}}`, w.GetSQL())
			}
		})
	}

	// The same holds for a programmatically unsupported expression.
	f := New()
	f.AddFilter(CustomExpr{Field: "x", Operator: "op"})
	f.Build(ElasticsearchAdapter{})
	require.NotPanics(t, func() { _ = f.GetQuery(nil).(ElasticsearchQueryWrapper) })
}

// A7-3: FromFigo replaced b.query on the success path but never cleared b.err,
// so a reused builder stayed pinned to match_none forever while holding a
// perfectly good query — and reported an error naming an expression type that
// was no longer present.
func TestA7_3_ESBuilderErrorIsNotSticky(t *testing.T) {
	bad := New()
	bad.AddFilter(CustomExpr{Field: "x", Operator: "op"})
	bad.Build(ElasticsearchAdapter{})

	good := New()
	good.AddFilter(EqExpr{Field: "id", Value: 1})
	good.Build(ElasticsearchAdapter{})

	b := NewElasticsearchQueryBuilder()
	b.FromFigo(bad)
	require.Error(t, b.Err())

	b.FromFigo(good)
	assert.NoError(t, b.Err(), "a successful FromFigo must clear the deferred error")

	s, err := b.ToJSONCompact()
	require.NoError(t, err)
	assert.Contains(t, s, `"term":{"id":1}`, "the good query must actually be emitted: %s", s)
	assert.NotContains(t, s, "match_none")
}

// A7-7: the rendered document handed the caller the figo instance's own
// Values slice, so editing the emitted query (README documents the struct as
// something you marshal into a request body) rewrote the instance's clause
// tree for every later render, on every adapter. SetSource retained the
// caller's variadic backing array the same way.
func TestA7_7_ESRenderedDocumentDoesNotAliasInstance(t *testing.T) {
	f := New()
	f.AddFilter(InExpr{Field: "id", Values: []any{1, 2, 3}})
	f.Build(ElasticsearchAdapter{})

	q1, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	q1.Query["terms"].(map[string]interface{})["id"].([]any)[0] = 999

	q2, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	assert.Equal(t, `{"terms":{"id":[1,2,3]}}`, esJSON(t, q2.Query), "the instance must be unchanged")

	sql, _, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`id` IN (?,?,?)", sql)
	_, args, err := BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, []any{1, 2, 3}, args, "the corruption must not cross adapters")

	// NotInExpr renders through the same path.
	fn := New()
	fn.AddFilter(NotInExpr{Field: "id", Values: []any{1, 2}})
	fn.Build(ElasticsearchAdapter{})
	n1, err := BuildElasticsearchQuery(fn)
	require.NoError(t, err)
	n1.Query["bool"].(map[string]interface{})["must_not"].(map[string]interface{})["terms"].(map[string]interface{})["id"].([]any)[0] = 777
	n2, err := BuildElasticsearchQuery(fn)
	require.NoError(t, err)
	assert.Contains(t, esJSON(t, n2.Query), "[1,2]")

	// SetSource must copy the caller's slice.
	names := []string{"name", "email"}
	b := NewElasticsearchQueryBuilder().SetSource(names...)
	names[0] = "password_hash"
	s, err := b.ToJSONCompact()
	require.NoError(t, err)
	assert.Contains(t, s, `["name","email"]`, "the projection must not follow the caller's slice: %s", s)
}

// A7-8: SetPagination wrote negative from/size straight into the body, which
// Elasticsearch rejects with "[from]/[size] parameter cannot be negative"
// while Build/BuildE/Err all reported success. The identical values fed
// through the adapter path are normalised to a legal body.
func TestA7_8_ESBuilderRejectsNegativePagination(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "id", Value: 1})
	f.Build(ElasticsearchAdapter{})

	for _, tc := range [][2]int{{-5, -10}, {-1, 5}, {0, -1}} {
		b := NewElasticsearchQueryBuilder().FromFigo(f).SetPagination(tc[0], tc[1])
		require.Error(t, b.Err(), "from=%d size=%d must be diagnosed", tc[0], tc[1])

		q, err := b.BuildE()
		assert.Error(t, err)
		assert.Contains(t, q.Query, "match_none", "an invalid page must fail closed")

		s, err := b.ToJSONCompact()
		assert.Error(t, err)
		assert.NotContains(t, s, "-", "no negative value may reach the body: %s", s)
	}

	// Valid pagination is untouched.
	b := NewElasticsearchQueryBuilder().FromFigo(f).SetPagination(5, 10)
	require.NoError(t, b.Err())
	s, err := b.ToJSONCompact()
	require.NoError(t, err)
	assert.Contains(t, s, `"from":5`)
	assert.Contains(t, s, `"size":10`)
}
