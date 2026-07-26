package adapters

// Regression tests for the hunt #8 findings in the MongoDB adapter area.
// Each test is named for the finding id it pins.

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// ---------------------------------------------------------------------------
// A5-2
// ---------------------------------------------------------------------------

type a52TaggedOperator struct {
	Ne any `bson:"$ne"`
}

type a52NestedOperator struct {
	Inner a52TaggedOperator `bson:"inner"`
}

type a52InlineOperator struct {
	M bson.M `bson:",inline"`
}

type a52Clean struct {
	Name string `bson:"name"`
	Age  int    `bson:"age"`
	Skip any    `bson:"-"`
	priv string
}

type a52NamedBytes []byte

// A5-2: mongoCheckValue exists to stop a filter VALUE from becoming query
// STRUCTURE ({"password":{"$ne":null}} is the textbook NoSQL auth bypass). Two
// carriers walked straight past it: bson.Raw, whose underlying type is []byte
// but whose BSON encoding is an embedded DOCUMENT, was waved through by the
// []byte fast path; and a struct with a '$'-prefixed bson tag had no arm at all.
func TestA5_2_OperatorKeyGuardSeesThroughBsonRawAndStructTags(t *testing.T) {
	mustRaw := func(v any) bson.Raw {
		b, err := bson.Marshal(v)
		require.NoError(t, err)
		return bson.Raw(b)
	}

	rejected := []struct {
		name string
		v    any
		key  string
	}{
		{"bson.Raw carrying $ne", mustRaw(bson.M{"$ne": nil}), "$ne"},
		{"bson.Raw carrying $where", mustRaw(bson.M{"$where": "sleep(60000)||true"}), "$where"},
		{"bson.Raw with a nested operator", mustRaw(bson.M{"a": bson.M{"$gt": 1}}), "$gt"},
		{"bson.Raw with an operator inside an array", mustRaw(bson.M{"a": bson.A{bson.M{"$ne": nil}}}), "$ne"},
		{"bson.RawValue embedded document", bson.RawValue{Type: bsontype.EmbeddedDocument, Value: mustRaw(bson.M{"$ne": nil})}, "$ne"},
		{"struct with a $-prefixed bson tag", a52TaggedOperator{}, "$ne"},
		{"pointer to that struct", &a52TaggedOperator{}, "$ne"},
		{"the tag one level down", a52NestedOperator{}, "$ne"},
		{"a struct inside a slice", []a52TaggedOperator{{}}, "$ne"},
		{"a struct inside a map", map[string]any{"k": a52TaggedOperator{}}, "$ne"},
		{"an inlined map carrying the operator", a52InlineOperator{M: bson.M{"$ne": nil}}, "$ne"},
	}
	for _, c := range rejected {
		t.Run("rejected/"+c.name, func(t *testing.T) {
			f := figo.New()
			f.AddFilter(figo.EqExpr{Field: "password", Value: c.v})
			f.Build(MongoAdapter{})
			_, err := BuildMongoFilter(f)
			require.Error(t, err, "the operator key reached the wire")
			assert.Contains(t, err.Error(), c.key)
			assert.Contains(t, err.Error(), "a filter value is data, not query structure")
		})
	}

	// The controls matter as much as the rejections: this guard runs on every
	// bound value, so a false positive would break ordinary filters.
	accepted := []struct {
		name string
		v    any
	}{
		{"bson.Raw with no operator key", mustRaw(bson.M{"name": "bob"})},
		{"bson.RawValue holding a plain document", bson.RawValue{Type: bsontype.EmbeddedDocument, Value: mustRaw(bson.M{"name": "bob"})}},
		{"bson.RawValue holding a string", mustRaw(bson.M{"k": "x"}).Lookup("k")},
		{"a plain []byte is still binary data", []byte{1, 2, 3}},
		{"a nil bson.Raw", bson.Raw(nil)},
		{"a bson.Raw too short to be a document", bson.Raw{1, 2}},
		{"an empty bson.RawValue", bson.RawValue{}},
		{"a named byte slice the driver encodes as binary", a52NamedBytes{1, 2, 3}},
		{"a struct with ordinary tags, a skipped field and a private field", a52Clean{Name: "bob", Age: 3, Skip: bson.M{"$ne": nil}, priv: "x"}},
		{"an operator-free nested document", bson.M{"a": bson.M{"b": 1}}},
		{"a scalar", 7},
	}
	for _, c := range accepted {
		t.Run("accepted/"+c.name, func(t *testing.T) {
			f := figo.New()
			f.AddFilter(figo.EqExpr{Field: "password", Value: c.v})
			f.Build(MongoAdapter{})
			_, err := BuildMongoFilter(f)
			require.NoError(t, err)
		})
	}

	// A `bson:"-"` field is not encoded, so its contents can never become an
	// operator — refusing it would be a false positive. Pinned explicitly
	// because a52Clean above carries one.
	t.Run("a bson:\"-\" field cannot carry an operator to the wire", func(t *testing.T) {
		f := figo.New()
		f.AddFilter(figo.EqExpr{Field: "password", Value: a52Clean{Skip: bson.M{"$where": "x"}}})
		f.Build(MongoAdapter{})
		filter, err := BuildMongoFilter(f)
		require.NoError(t, err)
		b, err := bson.MarshalExtJSON(filter, false, false)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "$where")
	})
}

// ---------------------------------------------------------------------------
// A5-3
// ---------------------------------------------------------------------------

// A5-3: M6 made the aggregate path emit a $project and carry every $lookup
// output alias through it. When the caller's own projection already names a
// field UNDER one of those aliases, the stage named both a path and its prefix —
// which MongoDB rejects outright ("Path collision", error 31249/31250) — while
// BuildMongoAggregatePipeline reported success. The invariant is that the
// emitted $project can never contain both a path and a prefix of that path.
func TestA5_3_AggregateProjectNeverCollidesWithALookupAlias(t *testing.T) {
	joins := map[string]MongoJoin{
		"Orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "Orders"},
		"orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "orders"},
	}
	cases := []struct {
		name     string
		naming   figo.NamingFunc
		relation string // load= relation name; defaults to "Orders"
		sel      []string
		want     []string
	}{
		{
			name:   "a projected sub-field of the alias replaces the alias",
			naming: figo.NoChangeNaming,
			sel:    []string{"a", "Orders.total"},
			want:   []string{"Orders.total", "a"},
		},
		{
			name:   "a deep projected sub-field of the alias replaces the alias",
			naming: figo.NoChangeNaming,
			sel:    []string{"a", "Orders.x.y"},
			want:   []string{"Orders.x.y", "a"},
		},
		{
			name:   "the caller's own path/prefix pair is collapsed too",
			naming: figo.NoChangeNaming,
			sel:    []string{"m", "m.k", "m.k.deep"},
			want:   []string{"Orders", "m"},
		},
		{
			name:   "an unrelated projection still carries the alias through",
			naming: figo.NoChangeNaming,
			sel:    []string{"a", "b"},
			want:   []string{"Orders", "a", "b"},
		},
		{
			name:   "a key that merely shares a prefix is not a path collision",
			naming: figo.NoChangeNaming,
			sel:    []string{"a", "OrdersX"},
			want:   []string{"Orders", "OrdersX", "a"},
		},
		{
			// SnakeCaseNaming converts per '.'-separated segment (hunt #8 CORE-2),
			// so a qualified name stays qualified: Orders.total -> orders.total.
			// That does NOT collide with the "Orders" alias, because the alias
			// keeps its capital and MongoDB paths are case-sensitive.
			name:   "per-segment naming keeps a qualified key qualified without colliding",
			naming: nil,
			sel:    []string{"a", "Orders.total"},
			want:   []string{"Orders", "a", "orders.total"},
		},
		{
			// The sharp case that per-segment naming makes reachable: a lowercase
			// relation means the converted projection key really does share the
			// alias's path, so the alias must be dropped rather than emitted
			// alongside it — the A5-3 guard, exercised through naming.
			name:     "a converted key on the alias path drops the alias",
			naming:   nil,
			relation: "orders",
			sel:      []string{"a", "orders.total"},
			want:     []string{"a", "orders.total"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := figo.New()
			if c.naming != nil {
				f.SetNamingFunc(c.naming)
			}
			rel := c.relation
			if rel == "" {
				rel = "Orders"
			}
			require.NoError(t, f.AddFiltersFromString(`a=1 load=[`+rel+`:total>1]`))
			f.AddSelectFields(c.sel...)
			f.Build(MongoAdapter{})
			pipe, err := BuildMongoAggregatePipeline(f, joins)
			require.NoError(t, err)

			var project bson.D
			for _, stage := range pipe {
				if stage[0].Key == "$project" {
					project = stage[0].Value.(bson.D)
				}
			}
			require.NotNil(t, project, "the aggregate must still emit a $project (M6)")
			keys := make([]string, 0, len(project))
			for _, e := range project {
				keys = append(keys, e.Key)
			}
			assert.Equal(t, c.want, keys)

			// The invariant, stated independently of the expected key list.
			for _, outer := range keys {
				for _, inner := range keys {
					require.False(t, outer != inner && strings.HasPrefix(inner, outer+"."),
						"$project names both %q and its prefix %q — MongoDB rejects the whole aggregate", inner, outer)
				}
			}

			// And the pipeline must actually be executable: the evaluator
			// enforces MongoDB's path-collision rule, so a regression here
			// fails as a rejected pipeline rather than as a changed shape.
			e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
			_, err = e.runPipeline(pipe, oracleDocs())
			require.NoError(t, err, "pipeline is not executable")
		})
	}
}

// A5-3 (second half): the point of carrying the alias through was that adding a
// projection must not defeat the load= that selected the aggregate path. A
// caller-supplied sub-path of the alias keeps the relation in the output, just
// narrowed — which is what the caller asked for.
func TestA5_3_ProjectingASubFieldStillReturnsTheRelation(t *testing.T) {
	joins := map[string]MongoJoin{
		"Orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "Orders"},
	}
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	require.NoError(t, f.AddFiltersFromString(`a=1 load=[Orders:total>60]`))
	f.AddSelectFields("a", "Orders.total")
	f.Build(MongoAdapter{})
	pipe, err := BuildMongoAggregatePipeline(f, joins)
	require.NoError(t, err)

	e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
	docs, err := e.runPipeline(pipe, oracleDocs())
	require.NoError(t, err)
	require.Len(t, docs, 1, "item 1 is the only parent with an order over 60")
	orders, ok := arrEntries(docs[0]["Orders"])
	require.True(t, ok, "the relation must survive the projection")
	require.Len(t, orders, 1)
	child := orders[0].(bson.M)
	assert.Equal(t, 100, child["total"], "the projected sub-field is present")
	assert.NotContains(t, child, "status", "and only the projected sub-field is present")
}
