package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// ordersJoin is the join description used by the preload regressions below.
func ordersJoin() map[string]MongoJoin {
	return map[string]MongoJoin{
		"Orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "Orders"},
	}
}

// lookupSubPipeline returns the `pipeline` of the first $lookup stage.
func lookupSubPipeline(t *testing.T, pipe mongo.Pipeline) mongo.Pipeline {
	t.Helper()
	for _, stage := range pipe {
		for _, e := range stage {
			if e.Key != "$lookup" {
				continue
			}
			doc, ok := e.Value.(bson.D)
			require.True(t, ok, "$lookup value should be a bson.D, got %T", e.Value)
			sub, ok := doc.Map()["pipeline"].(mongo.Pipeline)
			require.True(t, ok, "$lookup must carry a correlated sub-pipeline, got %#v", doc.Map()["pipeline"])
			return sub
		}
	}
	t.Fatalf("no $lookup stage in %v", pipe)
	return nil
}

// rootMatchKeys returns the field keys of every top-level $match stage.
func rootMatchKeys(t *testing.T, pipe mongo.Pipeline) []string {
	t.Helper()
	var keys []string
	for _, stage := range pipe {
		for _, e := range stage {
			if e.Key != "$match" {
				continue
			}
			m, ok := e.Value.(bson.M)
			require.True(t, ok, "$match value should be a bson.M, got %T", e.Value)
			for k := range m {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// H12: preload filters were a parent-level $match on the produced lookup ARRAY.
// They must be rendered inside a correlated $lookup sub-pipeline instead, so
// each predicate is evaluated against a single child document and the array
// itself is filtered.
func TestH12_PreloadFiltersRenderInsideLookupSubPipeline(t *testing.T) {
	t.Run("a: predicates apply to ONE child, not a combination of elements", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:status="paid" amount>100]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)

		sub := lookupSubPipeline(t, pipe)
		require.Len(t, sub, 2, "join condition + child filter: %v", sub)
		childMatch, ok := sub[1].Map()["$match"].(bson.M)
		require.True(t, ok, "got %#v", sub[1])
		and, ok := childMatch["$and"].([]bson.M)
		require.True(t, ok, "got %#v", childMatch)
		require.Len(t, and, 2)
		// Unqualified: inside the sub-pipeline "status" is the CHILD's field.
		assert.Equal(t, bson.M{"status": "paid"}, and[0])
		assert.Equal(t, bson.M{"amount": bson.M{"$gt": int64(100)}}, and[1])

		for _, k := range rootMatchKeys(t, pipe) {
			assert.NotEqual(t, "Orders.status", k, "predicate must not be a parent-level match on the array")
			assert.NotEqual(t, "Orders.amount", k, "predicate must not be a parent-level match on the array")
		}
	})

	t.Run("b: != keeps the existential quantifier instead of inverting it", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:status!="paid"]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)

		sub := lookupSubPipeline(t, pipe)
		require.Len(t, sub, 2)
		// {"Orders.status": {$ne: "paid"}} on an ARRAY path means "NO element is
		// paid" — the opposite quantifier. Per child it means what it says.
		assert.Equal(t, bson.M{"status": bson.M{"$ne": "paid"}}, sub[1].Map()["$match"])
		assert.NotContains(t, rootMatchKeys(t, pipe), "Orders.status")
	})

	t.Run("c: the relation array itself is filtered", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:tenant_id=7]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)

		sub := lookupSubPipeline(t, pipe)
		require.Len(t, sub, 2, "the tenant predicate must run inside the $lookup so other tenants' orders never enter the array: %v", sub)
		assert.Equal(t, bson.M{"tenant_id": int64(7)}, sub[1].Map()["$match"])

		// The correlated join condition must still be present and first.
		assert.Equal(t, bson.M{"$expr": bson.M{"$eq": []any{"$user_id", "$$figoLocal"}}}, sub[0].Map()["$match"])
	})

	t.Run("parent still needs at least one matching child", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:tenant_id=7]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)
		assert.Contains(t, rootMatchKeys(t, pipe), "Orders.0",
			"the raw adapter's INNER JOIN semantics: a parent with no matching child drops out")
	})

	t.Run("a preload with no predicates adds no parent restriction", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)
		assert.NotContains(t, rootMatchKeys(t, pipe), "Orders.0")
		assert.Len(t, lookupSubPipeline(t, pipe), 1, "join condition only")
	})
}

// H13: a missing or mis-keyed joins map produced a $lookup with EMPTY
// localField/foreignField and err=nil. MongoDB parses both as FieldPaths and
// rejects the empty string (error 40352), so the whole aggregate failed at the
// server with no hint about which relation was unconfigured.
func TestH13_MissingOrMiskeyedJoinFailsClosed(t *testing.T) {
	build := func(t *testing.T, joins map[string]MongoJoin) (mongo.Pipeline, error) {
		t.Helper()
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:id=1]`))
		f.Build(MongoAdapter{})
		return BuildMongoAggregatePipeline(f, joins)
	}

	t.Run("nil joins map", func(t *testing.T) {
		pipe, err := build(t, nil)
		require.Error(t, err)
		assert.Nil(t, pipe)
		assert.Contains(t, err.Error(), `"Orders"`)
	})

	t.Run("mis-keyed joins map names the configured keys", func(t *testing.T) {
		pipe, err := build(t, map[string]MongoJoin{
			"orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "orders"},
		})
		require.Error(t, err)
		assert.Nil(t, pipe)
		assert.Contains(t, err.Error(), "orders", "the diagnostic must show the key that IS configured")
	})

	t.Run("incomplete join is rejected too", func(t *testing.T) {
		_, err := build(t, map[string]MongoJoin{"Orders": {From: "orders", LocalField: "_id"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ForeignField")
	})

	t.Run("As defaults to the relation name", func(t *testing.T) {
		pipe, err := build(t, map[string]MongoJoin{
			"Orders": {From: "orders", LocalField: "_id", ForeignField: "user_id"},
		})
		require.NoError(t, err)
		assert.Contains(t, rootMatchKeys(t, pipe), "Orders.0")
	})

	t.Run("GetQuery aggregate path fails closed too", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:id=1]`))
		f.Build(MongoAdapter{})
		_, ok := MongoAdapter{}.GetQuery(f, nil, "AGG")
		assert.False(t, ok, "an unconfigured join must not yield a pipeline MongoDB rejects")
	})
}

// M5: the '$'-operator guard covered clause fields only — not relation names,
// sort keys or projection keys, all of which land in operator position too.
func TestM5_OperatorNamesRejectedOutsideClauseFields(t *testing.T) {
	t.Run("preload relation name", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`load=[$where:id=1]`))
		f.Build(MongoAdapter{})
		_, err := BuildMongoAggregatePipeline(f, map[string]MongoJoin{
			"$where": {From: "x", LocalField: "a", ForeignField: "b", As: "$where"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$where")
	})

	t.Run("MongoJoin.As", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:id=1]`))
		f.Build(MongoAdapter{})
		_, err := BuildMongoAggregatePipeline(f, map[string]MongoJoin{
			"Orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "$where"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$where")
	})

	t.Run("sort key", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`a=1 sort=$natural:desc`))
		f.Build(MongoAdapter{})

		_, ok := MongoAdapter{}.GetQuery(f, nil)
		assert.False(t, ok, "GetQuery has a channel for the rejection and must use it")

		// The no-error-channel helper must still never emit the key.
		assert.Nil(t, BuildMongoFindOptions(f).Sort)

		_, _, err := AdapterMongoGetFind(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$natural")
	})

	t.Run("projection key", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		f.AddSelectFields("$where")
		f.Build(MongoAdapter{})

		_, ok := MongoAdapter{}.GetQuery(f, nil)
		assert.False(t, ok)
		assert.Nil(t, BuildMongoFindOptions(f).Projection)

		_, _, err := AdapterMongoGetFind(f)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "$where")
	})
}

// M6: AddSelectFields was honored on the Find path and silently ignored on the
// aggregation path, so merely adding load= turned a narrowed projection back
// into "return whole documents".
func TestM6_AggregatePathHonorsSelectFields(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:id=2]`))
	f.AddSelectFields("a", "b")
	f.Build(MongoAdapter{})
	pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
	require.NoError(t, err)

	last := pipe[len(pipe)-1]
	proj, ok := last.Map()["$project"].(bson.D)
	require.True(t, ok, "the pipeline must end with a $project, got %v", pipe)
	assert.Equal(t, bson.D{{Key: "Orders", Value: 1}, {Key: "a", Value: 1}, {Key: "b", Value: 1}}, proj,
		"selected fields plus the lookup outputs, in a fixed order")
}

// M7: preload predicates vanished on the Find path, so `a=1 load=[Orders:
// tenant_id=7]` rendered {"a":1} — a silent WIDENING. Elasticsearch fails
// closed for exactly this case.
func TestM7_FindPathFailsClosedOnPreloadPredicates(t *testing.T) {
	t.Run("BuildMongoFilter", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:tenant_id=7]`))
		f.Build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.Error(t, err, "rendering {\"a\":1} drops the tenant restriction")
		assert.Nil(t, filter)
		assert.Contains(t, err.Error(), "Orders")
	})

	t.Run("GetQuery without the AGG hint", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:tenant_id=7]`))
		f.Build(MongoAdapter{})
		_, ok := MongoAdapter{}.GetQuery(f, nil)
		assert.False(t, ok)
	})

	t.Run("a preload with no predicates cannot widen and is accepted", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:]`))
		f.Build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Equal(t, bson.M{"a": int64(1)}, filter)
	})
}

// L7: identical figo state marshalled to different WIRE bytes, because the
// two multi-key sub-documents in the renderer were bson.M maps. The old
// determinism test used encoding/json, which sorts keys and cannot see it.
func TestL7_FilterMarshalsToStableWireBytes(t *testing.T) {
	stable := func(t *testing.T, build func() Figo) {
		t.Helper()
		var first []byte
		for i := 0; i < 200; i++ {
			m, err := BuildMongoFilter(build())
			require.NoError(t, err)
			raw, err := bson.Marshal(m)
			require.NoError(t, err)
			if i == 0 {
				first = raw
				continue
			}
			require.Equal(t, first, raw, "wire bytes differ between renders of identical state")
		}
	}

	t.Run("between", func(t *testing.T) {
		stable(t, func() Figo {
			f := New()
			_ = f.AddFiltersFromString(`price<bet>(1..10)`)
			f.Build(MongoAdapter{})
			return f
		})
	})

	t.Run("text with language", func(t *testing.T) {
		stable(t, func() Figo {
			f := New()
			f.AddFilter(FullTextSearchExpr{Query: "hello", Language: "en"})
			f.Build(MongoAdapter{})
			return f
		})
	})

	t.Run("projection", func(t *testing.T) {
		var first []byte
		for i := 0; i < 200; i++ {
			f := New()
			require.NoError(t, f.AddFiltersFromString(`a=1`))
			f.AddSelectFields("z", "y", "x", "w", "v")
			f.Build(MongoAdapter{})
			raw, err := bson.Marshal(BuildMongoFindOptions(f).Projection)
			require.NoError(t, err)
			if i == 0 {
				first = raw
				continue
			}
			require.Equal(t, first, raw)
		}
	})
}

// L8: a root clause addressing a preloaded relation was matched BEFORE the
// $lookup that creates that field, so it saw a missing field and returned zero
// rows.
func TestL8_RootMatchOnRelationRunsAfterTheLookup(t *testing.T) {
	t.Run("root match moves after the lookup", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`Orders.total>5 load=[Orders:id=2]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)

		lookupIdx, rootIdx := -1, -1
		for i, stage := range pipe {
			for _, e := range stage {
				switch e.Key {
				case "$lookup":
					lookupIdx = i
				case "$match":
					if m, ok := e.Value.(bson.M); ok {
						if _, hit := m["Orders.total"]; hit {
							rootIdx = i
						}
					}
				}
			}
		}
		require.NotEqual(t, -1, lookupIdx)
		require.NotEqual(t, -1, rootIdx, "root clause lost: %v", pipe)
		assert.Greater(t, rootIdx, lookupIdx, "the field does not exist until the $lookup ran: %v", pipe)
	})

	t.Run("a root match with no relation reference stays first", func(t *testing.T) {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:id=2]`))
		f.Build(MongoAdapter{})
		pipe, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.NoError(t, err)
		assert.Equal(t, bson.M{"a": int64(1)}, pipe[0].Map()["$match"], "filter first is the whole point: %v", pipe)
	})

	t.Run("$text cannot be moved off the first stage", func(t *testing.T) {
		f := New()
		f.SetNamingFunc(NoChangeNaming)
		require.NoError(t, f.AddFiltersFromString(`Orders.total>5 load=[Orders:id=2]`))
		f.Build(MongoAdapter{})
		f.AddFilter(FullTextSearchExpr{Query: "hi"}) // after Build: a DSL set discards pre-Build AddFilter
		_, err := BuildMongoAggregatePipeline(f, ordersJoin())
		require.Error(t, err, "MongoDB requires a $match carrying $text to be the first stage")
		assert.Contains(t, err.Error(), "$text")
	})
}

// L9: LIKE became an anchored regex without the DOTALL flag, so '%' and '_'
// stopped matching a newline the way SQL LIKE does.
func TestL9_LikeRegexMatchesNewlinesLikeSQL(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=^"a%b"`))
	f.Build(MongoAdapter{})
	m, err := BuildMongoFilter(f)
	require.NoError(t, err)
	re := m["name"].(primitive.Regex)
	assert.Equal(t, `^a.*b$`, re.Pattern)
	assert.Contains(t, re.Options, "s", "'%' must match a newline, as SQL LIKE does")
	assert.NotContains(t, re.Options, "m", "'m' would unanchor ^ and $")
}

// L10: a non-scalar value became query STRUCTURE instead of data —
// EqExpr{Field: "password", Value: map[string]any{"$ne": nil}} rendered
// {"password":{"$ne":null}}, the textbook NoSQL auth-bypass payload, where the
// SQL adapters bind the same value inertly.
func TestL10_OperatorBearingValuesAreRejected(t *testing.T) {
	render := func(e Expr) error {
		f := New()
		f.AddFilter(e)
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		return err
	}

	cases := []struct {
		name string
		expr Expr
	}{
		{"eq map", EqExpr{Field: "password", Value: map[string]any{"$ne": nil}}},
		{"eq bson.M", EqExpr{Field: "password", Value: bson.M{"$gt": ""}}},
		{"eq bson.D", EqExpr{Field: "password", Value: bson.D{{Key: "$gt", Value: ""}}}},
		{"eq nested", EqExpr{Field: "password", Value: map[string]any{"ok": []any{bson.M{"$where": "1"}}}}},
		{"neq", NeqExpr{Field: "password", Value: map[string]any{"$ne": nil}}},
		{"in", InExpr{Field: "password", Values: []any{map[string]any{"$ne": nil}}}},
		{"between low", BetweenExpr{Field: "p", Low: bson.M{"$ne": nil}, High: 1}},
		{"json path", JsonPathExpr{Field: "data", Path: "$.a", Op: "=", Value: bson.M{"$ne": nil}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := render(tc.expr)
			require.Error(t, err, "a filter value must be data, not query structure")
			assert.Contains(t, err.Error(), "$")
		})
	}

	t.Run("an operator-free document value is still legal data", func(t *testing.T) {
		f := New()
		f.AddFilter(EqExpr{Field: "profile", Value: map[string]any{"city": "berlin"}})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"city": "berlin"}, m["profile"])
	})
}

// A10/A7-3: the $text-under-$nor guard rescanned the whole remaining subtree at
// every NotExpr, making the render QUADRATIC in the NOT nesting depth (a 32 KB
// `not not not ... a=1` chain took 260 ms and 4x longer for every doubling,
// while gorm and Elasticsearch rendered the same AST linearly).
func TestA10_DeepNotNestingRendersLinearly(t *testing.T) {
	const n = 40000
	f := New()
	require.NoError(t, f.AddFiltersFromString(strings.Repeat("not ", n)+"a=1"))
	f.Build(MongoAdapter{})

	start := time.Now()
	m, err := BuildMongoFilter(f)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotEmpty(t, m)

	// Linear render is ~20ms here; the quadratic one extrapolates to ~6s.
	assert.Less(t, elapsed, 2*time.Second, "render is quadratic in the NOT depth (took %v)", elapsed)
}

// The guard the quadratic scan implemented must still hold: $text is illegal
// under $nor at ANY depth, and MongoDB rejects it server-side.
func TestA10_FullTextUnderNotStillRejectedAtDepth(t *testing.T) {
	for _, depth := range []int{1, 2, 5, 40} {
		var e Expr = FullTextSearchExpr{Query: "banned"}
		for i := 0; i < depth; i++ {
			e = AndExpr{Operands: []Expr{EqExpr{Field: "a", Value: 1}, e}}
		}
		f := New()
		f.AddFilter(NotExpr{Operands: []Expr{e}})
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		require.Error(t, err, "depth %d", depth)
		assert.Contains(t, err.Error(), "$text")
	}

	t.Run("a NOT elsewhere in the tree does not poison a sibling $text", func(t *testing.T) {
		f := New()
		f.AddFilter(AndExpr{Operands: []Expr{
			NotExpr{Operands: []Expr{EqExpr{Field: "a", Value: 1}}},
			FullTextSearchExpr{Query: "fine"},
		}})
		f.Build(MongoAdapter{})
		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		require.NotEmpty(t, m)
	})
}

// N1 (new in this pass): a non-finite or negative geo distance, and a
// non-finite coordinate, rendered a $centerSphere MongoDB rejects at query
// time while the build reported success. The Elasticsearch adapter already
// validates a non-finite Distance; this is the same control.
func TestN1_GeoNonFiniteAndNegativeRejected(t *testing.T) {
	render := func(e Expr) error {
		f := New()
		f.AddFilter(e)
		f.Build(MongoAdapter{})
		_, err := BuildMongoFilter(f)
		return err
	}
	cases := []struct {
		name string
		expr Expr
	}{
		{"NaN distance", GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: math.NaN()}},
		{"Inf distance", GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: math.Inf(1)}},
		{"negative distance", GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: -5}},
		{"NaN latitude", GeoDistanceExpr{Field: "loc", Latitude: math.NaN(), Longitude: 2, Distance: 5}},
		{"Inf longitude", GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: math.Inf(-1), Distance: 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, render(tc.expr), "the build reported success for a query MongoDB can never run")
		})
	}

	t.Run("a valid geo predicate still renders", func(t *testing.T) {
		require.NoError(t, render(GeoDistanceExpr{Field: "loc", Latitude: 35.7, Longitude: 51.4, Distance: 10, Unit: "km"}))
	})

	t.Run("zero distance is legal", func(t *testing.T) {
		require.NoError(t, render(GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 0}))
	})
}

// N2 (new in this pass): MongoJoin.LocalField/ForeignField are rendered as
// aggregation field paths ("$"+name), so a name already starting with '$'
// silently became "$$name" — a VARIABLE reference rather than the field the
// caller named. Same class as M5's relation/sort/projection guard.
func TestN2_JoinFieldPathsRejectOperatorPrefix(t *testing.T) {
	build := func(j MongoJoin) error {
		f := New()
		require.NoError(t, f.AddFiltersFromString(`load=[Orders:id=1]`))
		f.Build(MongoAdapter{})
		_, err := BuildMongoAggregatePipeline(f, map[string]MongoJoin{"Orders": j})
		return err
	}
	t.Run("ForeignField", func(t *testing.T) {
		err := build(MongoJoin{From: "orders", LocalField: "_id", ForeignField: "$where", As: "Orders"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ForeignField")
	})
	t.Run("LocalField", func(t *testing.T) {
		err := build(MongoJoin{From: "orders", LocalField: "$x", ForeignField: "user_id", As: "Orders"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LocalField")
	})
	t.Run("ordinary dotted field paths are unaffected", func(t *testing.T) {
		require.NoError(t, build(MongoJoin{From: "orders", LocalField: "a.b", ForeignField: "c.d", As: "Orders"}))
	})
}

// N3 (new in this pass): two preloads configured with the same MongoJoin.As
// made the second $lookup overwrite the first's output array — a preload lost
// with no diagnostic.
func TestN3_DuplicateLookupOutputFieldRejected(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:id=1|Items:id=2]`))
	f.Build(MongoAdapter{})
	_, err := BuildMongoAggregatePipeline(f, map[string]MongoJoin{
		"Orders": {From: "orders", LocalField: "_id", ForeignField: "user_id", As: "rel"},
		"Items":  {From: "items", LocalField: "_id", ForeignField: "user_id", As: "rel"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rel")
}
