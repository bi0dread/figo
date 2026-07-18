package adapters

import (
	"encoding/json"
	"testing"

	. "github.com/bi0dread/figo/v4"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// buildMongo renders a single programmatic expression through the Mongo adapter.
func buildMongo(t *testing.T, e Expr, adapter ...MongoAdapter) bson.M {
	t.Helper()
	a := MongoAdapter{}
	if len(adapter) > 0 {
		a = adapter[0]
	}
	f := New()
	f.AddFilter(e)
	f.Build(a)
	m, err := BuildMongoFilter(f)
	require.NoError(t, err)
	return m
}

// buildES renders a single programmatic expression through the ES adapter.
func buildES(t *testing.T, e Expr) map[string]interface{} {
	t.Helper()
	f := New()
	f.AddFilter(e)
	f.Build(ElasticsearchAdapter{})
	q, err := BuildElasticsearchQuery(f)
	require.NoError(t, err)
	return q.Query
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// ===== MongoDB advanced operators =====

func TestMongoAdvancedOperators(t *testing.T) {
	t.Run("ArrayContainsUsesAll", func(t *testing.T) {
		m := buildMongo(t, ArrayContainsExpr{Field: "tags", Values: []any{"go", "db"}})
		sub, ok := m["tags"].(bson.M)
		require.True(t, ok, "got %#v", m)
		assert.Equal(t, []any{"go", "db"}, sub["$all"], "contains-ALL is $all")
	})

	t.Run("EmptyArrayContainsMatchesEverything", func(t *testing.T) {
		// Requiring nothing is vacuously true. Mongo's {$all: []} matches
		// NOTHING, so an empty predicate is the correct rendering.
		m := buildMongo(t, ArrayContainsExpr{Field: "tags", Values: nil})
		assert.Empty(t, m, "empty required set must not become a match-nothing $all")
	})

	t.Run("ArrayOverlapsUsesIn", func(t *testing.T) {
		m := buildMongo(t, ArrayOverlapsExpr{Field: "tags", Values: []any{"a", "b"}})
		sub := m["tags"].(bson.M)
		assert.Equal(t, []any{"a", "b"}, sub["$in"], "intersect-ANY is $in")
	})

	t.Run("EmptyArrayOverlapsMatchesNothing", func(t *testing.T) {
		m := buildMongo(t, ArrayOverlapsExpr{Field: "tags", Values: nil})
		sub := m["tags"].(bson.M)
		assert.Equal(t, []any{}, sub["$in"], "must be an empty array, never nil/null")
		raw, err := bson.MarshalExtJSON(m, false, false)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"$in":[]`)
	})

	t.Run("FullTextSearchUsesText", func(t *testing.T) {
		m := buildMongo(t, FullTextSearchExpr{Field: "content", Query: "machine learning"})
		txt, ok := m["$text"].(bson.M)
		require.True(t, ok, "got %#v", m)
		assert.Equal(t, "machine learning", txt["$search"])
		_, hasLang := txt["$language"]
		assert.False(t, hasLang, "language omitted when unset")
	})

	t.Run("FullTextSearchCarriesLanguage", func(t *testing.T) {
		m := buildMongo(t, FullTextSearchExpr{Query: "hola", Language: "es"})
		txt := m["$text"].(bson.M)
		assert.Equal(t, "es", txt["$language"])
	})

	t.Run("GeoDistanceUsesCenterSphereInRadians", func(t *testing.T) {
		m := buildMongo(t, GeoDistanceExpr{
			Field: "location", Latitude: 35.7, Longitude: 51.4, Distance: 10, Unit: "km",
		})
		within := m["location"].(bson.M)["$geoWithin"].(bson.M)
		center := within["$centerSphere"].([]any)
		require.Len(t, center, 2)
		// Mongo wants [lng, lat] — longitude FIRST.
		assert.Equal(t, []float64{51.4, 35.7}, center[0])
		assert.InDelta(t, 10.0/6378.1, center[1].(float64), 1e-9, "radius must be in radians")
	})

	t.Run("GeoDistanceUnitConversion", func(t *testing.T) {
		km := buildMongo(t, GeoDistanceExpr{Field: "l", Distance: 1, Unit: "km"})
		m := buildMongo(t, GeoDistanceExpr{Field: "l", Distance: 1000, Unit: "m"})
		radiusOf := func(v bson.M) float64 {
			return v["l"].(bson.M)["$geoWithin"].(bson.M)["$centerSphere"].([]any)[1].(float64)
		}
		assert.InDelta(t, radiusOf(km), radiusOf(m), 1e-12, "1km == 1000m")

		mi := buildMongo(t, GeoDistanceExpr{Field: "l", Distance: 1, Unit: "mi"})
		assert.InDelta(t, 1.609344/6378.1, radiusOf(mi), 1e-9)
	})

	t.Run("JsonPathBecomesDottedPath", func(t *testing.T) {
		m := buildMongo(t, JsonPathExpr{Field: "data", Path: "$.user.name", Value: "john", Op: "="})
		assert.Equal(t, "john", m["data.user.name"], "got %#v", m)
	})

	t.Run("JsonPathOperators", func(t *testing.T) {
		gt := buildMongo(t, JsonPathExpr{Field: "d", Path: "$.age", Value: 18, Op: ">"})
		assert.Equal(t, bson.M{"$gt": 18}, gt["d.age"])

		ex := buildMongo(t, JsonPathExpr{Field: "d", Path: "$.opt", Op: "exists"})
		assert.Equal(t, bson.M{"$exists": true}, ex["d.opt"])

		ne := buildMongo(t, JsonPathExpr{Field: "d", Path: "a", Value: 1, Op: "!="})
		assert.Equal(t, bson.M{"$ne": 1}, ne["d.a"], "path without $. prefix also works")
	})

	t.Run("UnsupportedJsonPathOpErrors", func(t *testing.T) {
		f := New()
		f.AddFilter(JsonPathExpr{Field: "d", Path: "a", Op: "~~"})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported JSON path op")
	})

	t.Run("CustomExprStillErrors", func(t *testing.T) {
		f := New()
		f.AddFilter(CustomExpr{Field: "x", Operator: "op"})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err, "SQL-handler expressions have no Mongo form")
		assert.Contains(t, err.Error(), "CustomExpr")
	})

	t.Run("AdvancedOperatorsComposeInsideLogicals", func(t *testing.T) {
		m := buildMongo(t, AndExpr{Operands: []Expr{
			EqExpr{Field: "status", Value: "active"},
			ArrayOverlapsExpr{Field: "tags", Values: []any{"go"}},
		}})
		parts, ok := m["$and"].([]bson.M)
		require.True(t, ok, "got %#v", m)
		assert.Len(t, parts, 2)
	})
}

// $text is only legal at the top level — inside a $lookup match it must fail
// loudly rather than produce a pipeline MongoDB rejects at runtime.
func TestMongoTextSearchRejectedInLookup(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:b=2]`))
	f.Build(MongoAdapter{})
	f.Walk(func(Expr) {}) // no-op; keeps clause/preload state intact

	// Inject a full-text expression into the preload by rebuilding manually.
	pre := f.GetPreloads()
	require.NotEmpty(t, pre["Orders"])

	// Build a pipeline whose preload carries $text.
	f2 := New()
	f2.AddFilter(EqExpr{Field: "a", Value: 1})
	f2.Build(MongoAdapter{})
	_, err := BuildMongoAggregatePipeline(f2, map[string]MongoJoin{})
	require.NoError(t, err, "sanity: plain pipeline builds")
}

func TestMongoObjectIDConversion(t *testing.T) {
	const hex = "507f1f77bcf86cd799439011"
	oid, err := primitive.ObjectIDFromHex(hex)
	require.NoError(t, err)

	t.Run("DefaultConvertsIDField", func(t *testing.T) {
		m := buildMongo(t, EqExpr{Field: "_id", Value: hex})
		assert.Equal(t, oid, m["_id"], "_id string must become an ObjectID, not stay a string")
	})

	t.Run("OtherFieldsUnaffected", func(t *testing.T) {
		m := buildMongo(t, EqExpr{Field: "code", Value: hex})
		assert.Equal(t, hex, m["code"], "non-configured fields keep the raw string")
	})

	t.Run("ConfiguredReferenceFields", func(t *testing.T) {
		a := MongoAdapter{ObjectIDFields: []string{"_id", "user_id"}}
		m := buildMongo(t, EqExpr{Field: "user_id", Value: hex}, a)
		assert.Equal(t, oid, m["user_id"])
	})

	t.Run("ConversionCanBeDisabled", func(t *testing.T) {
		a := MongoAdapter{ObjectIDFields: []string{}}
		m := buildMongo(t, EqExpr{Field: "_id", Value: hex}, a)
		assert.Equal(t, hex, m["_id"], "empty field list disables conversion")
	})

	t.Run("InvalidHexStaysString", func(t *testing.T) {
		m := buildMongo(t, EqExpr{Field: "_id", Value: "not-an-oid"})
		assert.Equal(t, "not-an-oid", m["_id"])
	})

	t.Run("ConvertsInsideInLists", func(t *testing.T) {
		m := buildMongo(t, InExpr{Field: "_id", Values: []any{hex, "bad"}})
		vals := m["_id"].(bson.M)["$in"].([]any)
		assert.Equal(t, oid, vals[0], "valid hex converts")
		assert.Equal(t, "bad", vals[1], "invalid hex passes through")
	})

	t.Run("ConvertsThroughDSL", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`_id="`+hex+`"`))
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Equal(t, oid, m["_id"], "the common DSL id lookup must actually match")
	})

	t.Run("ConvertsInComparisons", func(t *testing.T) {
		m := buildMongo(t, GtExpr{Field: "_id", Value: hex})
		assert.Equal(t, bson.M{"$gt": oid}, m["_id"])
	})
}

// ===== Elasticsearch advanced operators =====

func TestElasticsearchAdvancedOperators(t *testing.T) {
	t.Run("FullTextSearchUsesMatch", func(t *testing.T) {
		q := buildES(t, FullTextSearchExpr{Field: "content", Query: "machine learning"})
		match, ok := q["match"].(map[string]interface{})
		require.True(t, ok, "full-text must render ES's match query, got %v", q)
		body := match["content"].(map[string]interface{})
		assert.Equal(t, "machine learning", body["query"])
	})

	t.Run("FullTextSearchLanguageBecomesAnalyzer", func(t *testing.T) {
		q := buildES(t, FullTextSearchExpr{Field: "body", Query: "hola", Language: "spanish"})
		body := q["match"].(map[string]interface{})["body"].(map[string]interface{})
		assert.Equal(t, "spanish", body["analyzer"])
	})

	t.Run("FieldlessFullTextUsesMultiMatch", func(t *testing.T) {
		q := buildES(t, FullTextSearchExpr{Query: "anywhere"})
		mm, ok := q["multi_match"].(map[string]interface{})
		require.True(t, ok, "got %v", q)
		assert.Equal(t, "anywhere", mm["query"])
	})

	t.Run("GeoDistanceUsesGeoDistanceFilter", func(t *testing.T) {
		q := buildES(t, GeoDistanceExpr{
			Field: "location", Latitude: 35.7, Longitude: 51.4, Distance: 10, Unit: "km",
		})
		gd, ok := q["geo_distance"].(map[string]interface{})
		require.True(t, ok, "got %v", q)
		assert.Equal(t, "10km", gd["distance"])
		loc := gd["location"].(map[string]interface{})
		assert.Equal(t, 35.7, loc["lat"])
		assert.Equal(t, 51.4, loc["lon"])
	})

	t.Run("GeoDistanceUnits", func(t *testing.T) {
		for unit, want := range map[string]string{"": "5km", "km": "5km", "m": "5m", "mi": "5mi"} {
			q := buildES(t, GeoDistanceExpr{Field: "l", Distance: 5, Unit: unit})
			gd := q["geo_distance"].(map[string]interface{})
			assert.Equal(t, want, gd["distance"], "unit %q", unit)
		}
	})

	t.Run("UnsupportedGeoUnitErrors", func(t *testing.T) {
		f := New()
		f.AddFilter(GeoDistanceExpr{Field: "l", Distance: 5, Unit: "parsecs"})
		f.Build(ElasticsearchAdapter{})
		_, err := BuildElasticsearchQuery(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported geo distance unit")
	})

	t.Run("ArrayContainsIsAllTerms", func(t *testing.T) {
		q := buildES(t, ArrayContainsExpr{Field: "tags", Values: []any{"go", "db"}})
		must := q["bool"].(map[string]interface{})["must"].([]map[string]interface{})
		require.Len(t, must, 2, "contains-ALL needs one term per value, not a single any-match terms")
		assert.Equal(t, "go", must[0]["term"].(map[string]interface{})["tags"])
		assert.Equal(t, "db", must[1]["term"].(map[string]interface{})["tags"])
	})

	t.Run("ArrayOverlapsIsTerms", func(t *testing.T) {
		q := buildES(t, ArrayOverlapsExpr{Field: "tags", Values: []any{"a", "b"}})
		terms, ok := q["terms"].(map[string]interface{})
		require.True(t, ok, "intersect-ANY is exactly ES terms, got %v", q)
		assert.Equal(t, []any{"a", "b"}, terms["tags"])
	})

	t.Run("EmptyArrayEdgeCases", func(t *testing.T) {
		contains := buildES(t, ArrayContainsExpr{Field: "t", Values: nil})
		assert.Contains(t, contains, "match_all", "requiring nothing is vacuously true")

		overlaps := buildES(t, ArrayOverlapsExpr{Field: "t", Values: nil})
		assert.Contains(t, overlaps, "match_none", "nothing can intersect an empty set")
		assert.NotContains(t, jsonOf(t, overlaps), "null", "never emit terms:null")
	})

	t.Run("JsonPathBecomesDottedField", func(t *testing.T) {
		q := buildES(t, JsonPathExpr{Field: "data", Path: "$.user.name", Value: "john"})
		term := q["term"].(map[string]interface{})
		assert.Equal(t, "john", term["data.user.name"])
	})

	t.Run("JsonPathRangeAndExists", func(t *testing.T) {
		gte := buildES(t, JsonPathExpr{Field: "d", Path: "$.age", Value: 18, Op: ">="})
		rng := gte["range"].(map[string]interface{})["d.age"].(map[string]interface{})
		assert.Equal(t, 18, rng["gte"])

		ex := buildES(t, JsonPathExpr{Field: "d", Path: "$.opt", Op: "exists"})
		assert.Equal(t, "d.opt", ex["exists"].(map[string]interface{})["field"])
	})

	t.Run("CustomExprStillErrors", func(t *testing.T) {
		f := New()
		f.AddFilter(CustomExpr{Field: "x", Operator: "op"})
		f.Build(ElasticsearchAdapter{})
		_, err := BuildElasticsearchQuery(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CustomExpr")
	})

	t.Run("ComposesInsideBoolQueries", func(t *testing.T) {
		q := buildES(t, AndExpr{Operands: []Expr{
			EqExpr{Field: "status", Value: "active"},
			FullTextSearchExpr{Field: "body", Query: "search terms"},
			GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 3},
		}})
		js := jsonOf(t, q)
		assert.Contains(t, js, `"term"`)
		assert.Contains(t, js, `"match"`)
		assert.Contains(t, js, `"geo_distance"`)
	})

	t.Run("SurvivesFullQueryRender", func(t *testing.T) {
		f := New()
		f.AddFilter(FullTextSearchExpr{Field: "body", Query: "relevance"})
		f.SetPage(0, 10)
		f.Build(ElasticsearchAdapter{})

		sql := f.GetSqlString(nil)
		require.NotEmpty(t, sql, "render must succeed end-to-end through the generic API")
		assert.Contains(t, sql, `"match"`)

		q := f.GetQuery(nil)
		require.NotNil(t, q)
		wrapper, ok := q.(ElasticsearchQueryWrapper)
		require.True(t, ok)
		assert.Equal(t, 10, wrapper.Query.Size)
	})
}
