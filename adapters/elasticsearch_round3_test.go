package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A3: on a FromFigo error the builder must fail CLOSED — Build() returns a
// match_none query (never the initial match_all with all filters stripped) and
// the error is observable via Err()/BuildE().
func TestESBuilderFailsClosed(t *testing.T) {
	f := New()
	f.AddFilter(CustomExpr{Field: "x", Operator: "op"}) // unsupported on ES
	f.Build(ElasticsearchAdapter{})

	b := NewElasticsearchQueryBuilder().FromFigo(f)
	require.Error(t, b.Err(), "the deferred error must be exposed")

	q := b.Build()
	_, hasMatchNone := q.Query["match_none"]
	assert.True(t, hasMatchNone, "Build must return match_none on error, got %v", q.Query)
	_, hasMatchAll := q.Query["match_all"]
	assert.False(t, hasMatchAll, "the initial match_all must not survive an error")

	qe, err := b.BuildE()
	require.Error(t, err)
	_, hasMatchNone = qe.Query["match_none"]
	assert.True(t, hasMatchNone)

	// Sanity: a clean FromFigo keeps working.
	f2 := New()
	f2.AddFilter(EqExpr{Field: "a", Value: 1})
	f2.Build(ElasticsearchAdapter{})
	b2 := NewElasticsearchQueryBuilder().FromFigo(f2)
	require.NoError(t, b2.Err())
	q2, err := b2.BuildE()
	require.NoError(t, err)
	assert.Contains(t, q2.Query, "term")
}

// A4: figo's take:0 means "no limit"; ES cannot express unlimited and would
// default to 10 hits, so the max_result_window default must be rendered.
func TestESTakeZeroRendersMaxWindowSize(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1 page=skip:30,take:0`))
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	assert.Equal(t, 30, q.From)
	assert.Equal(t, 10000, q.Size, "take:0 must render the max_result_window default")

	// An explicit take still wins.
	f2 := New()
	require.NoError(t, f2.AddFiltersFromString(`a=1 page=skip:0,take:5`))
	f2.Build(ElasticsearchAdapter{})
	q2, err := BuildElasticsearchQuery(f2)
	require.NoError(t, err)
	assert.Equal(t, 5, q2.Size)
}

// A5: ES has no preload path; load= must fail the build loudly instead of
// being silently discarded (the only adapter that dropped it).
func TestESPreloadsFailClosed(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:b=2]`))
	f.Build(ElasticsearchAdapter{})

	_, err := BuildElasticsearchQuery(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preload")

	_, ok := ElasticsearchAdapter{}.GetSqlString(f, nil)
	assert.False(t, ok, "GetSqlString must fail closed on preloads")

	_, ok = ElasticsearchAdapter{}.GetQuery(f, nil)
	assert.False(t, ok, "GetQuery must fail closed on preloads")

	_, err = GetElasticsearchQueryString(f)
	assert.Error(t, err)

	_, err = NewElasticsearchQueryBuilder().FromFigo(f).ToJSON()
	assert.Error(t, err)
}

// A6: a non-finite geo distance would render "NaNkm"/"+Infkm"; fail the build
// like esGeoUnit does for unknown units.
func TestESGeoDistanceNonFiniteErrors(t *testing.T) {
	for name, dist := range map[string]float64{"NaN": math.NaN(), "PosInf": math.Inf(1), "NegInf": math.Inf(-1)} {
		t.Run(name, func(t *testing.T) {
			f := New()
			f.AddFilter(GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: dist, Unit: "km"})
			f.Build(ElasticsearchAdapter{})
			_, err := BuildElasticsearchQuery(f)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "non-finite")
		})
	}

	// Finite distances keep working.
	f := New()
	f.AddFilter(GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 5, Unit: "km"})
	f.Build(ElasticsearchAdapter{})
	_, err := BuildElasticsearchQuery(f)
	assert.NoError(t, err)
}

// A7: a query holding an unmarshalable value (NaN) must not report ok=true and
// then render "" from the wrapper's GetSQL.
func TestESGetQueryFailsOnUnmarshalableValue(t *testing.T) {
	f := New()
	f.AddFilter(EqExpr{Field: "score", Value: math.NaN()})
	f.Build(ElasticsearchAdapter{})

	_, ok := ElasticsearchAdapter{}.GetQuery(f, nil)
	assert.False(t, ok, "GetQuery must fail when the query cannot marshal")

	_, ok = ElasticsearchAdapter{}.GetSqlString(f, nil)
	assert.False(t, ok)
}

// A8: the field-less multi_match branch must carry Language as an analyzer,
// exactly like the field-scoped match branch.
func TestESMultiMatchCarriesLanguage(t *testing.T) {
	f := New()
	f.AddFilter(FullTextSearchExpr{Query: "hola", Language: "spanish"})
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	mm, ok := q.Query["multi_match"].(map[string]interface{})
	require.True(t, ok, "got %v", q.Query)
	assert.Equal(t, "spanish", mm["analyzer"])

	// Language stays omitted when unset.
	f2 := New()
	f2.AddFilter(FullTextSearchExpr{Query: "plain"})
	f2.Build(ElasticsearchAdapter{})
	q2, err := BuildElasticsearchQuery(f2)
	require.NoError(t, err)
	mm2 := q2.Query["multi_match"].(map[string]interface{})
	_, hasAnalyzer := mm2["analyzer"]
	assert.False(t, hasAnalyzer)
}

// A14: empty-string sort columns must be skipped, never {"":{"order":...}}.
func TestESSkipsEmptySortColumns(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(ElasticsearchAdapter{})
	f.SetSort(&OrderBy{Columns: []OrderByColumn{{Name: ""}, {Name: "id", Desc: true}}})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	require.Len(t, q.Sort, 1, "only the real column may render")
	js, err := json.Marshal(q.Sort)
	require.NoError(t, err)
	assert.NotContains(t, string(js), `""`, "empty sort key leaked: %s", js)
}
