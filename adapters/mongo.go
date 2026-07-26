package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoJoin describes how to join a related collection for a preload using $lookup
type MongoJoin struct {
	From         string // related collection name
	LocalField   string // field in base documents
	ForeignField string // field in related collection
	As           string // output array field name
}

// earthRadiusKm is the equatorial radius MongoDB documents for converting a
// $centerSphere radius from distance to radians.
const earthRadiusKm = 6378.1

// geoDistanceKm normalizes a GeoDistanceExpr distance to kilometers. An empty
// unit defaults to kilometers.
//
// A non-finite or negative distance is refused: it would render a NaN,
// Infinity or negative $centerSphere radius, all of which MongoDB rejects at
// query time, and the build reported success for a query that can never run.
// The Elasticsearch adapter already validates a non-finite Distance; this is
// the same control. Latitude/Longitude RANGES are deliberately not checked —
// a 2d index can be created with custom min/max bounds, so out-of-range values
// are not necessarily wrong — but a non-finite coordinate always is.
func geoDistanceKm(distance float64, unit string) (float64, error) {
	if math.IsNaN(distance) || math.IsInf(distance, 0) {
		return 0, fmt.Errorf("figo: geo distance must be a finite number, got %v", distance)
	}
	if distance < 0 {
		return 0, fmt.Errorf("figo: geo distance must not be negative, got %v", distance)
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "km", "kilometers":
		return distance, nil
	case "m", "meters":
		return distance / 1000, nil
	case "mi", "miles":
		return distance * 1.609344, nil
	default:
		return 0, fmt.Errorf("figo: unsupported geo distance unit %q", unit)
	}
}

// BuildMongoFilter converts the built figo expressions into a MongoDB filter.
// It returns an error if a clause uses an expression type the MongoDB adapter
// does not support (rather than silently dropping the condition), or if the
// instance carries preload predicates this path cannot express.
func BuildMongoFilter(f figo.Figo) (bson.M, error) {
	if err := mongoFindPreloadError(f); err != nil {
		return nil, err
	}
	return buildMongoFilterFromExprs(f.GetClauses(), mongoAdapterOf(f).render())
}

// mongoFindPreloadError rejects a Find-path render whose instance carries
// preload predicates. A Find has no $lookup, so those predicates were simply
// dropped: `a=1 load=[Orders:tenant_id=7]` rendered `{"a":1}`, silently
// WIDENING the query the caller wrote. Every other adapter honors load= (GORM
// Preload, raw JOIN, Mongo's own aggregate path) and Elasticsearch fails
// closed for exactly this case, so this one must not be the one that widens.
// A preload carrying no predicates cannot widen anything, so it is still
// accepted — it simply has no Find-path equivalent.
func mongoFindPreloadError(f figo.Figo) error {
	preloads := f.GetPreloads()
	if len(preloads) == 0 {
		return nil
	}
	for _, relation := range sortedRelations(preloads) {
		for _, e := range preloads[relation] {
			if e != nil {
				return fmt.Errorf("figo: the MongoDB Find path cannot apply the %q preload's filters; render the aggregate path instead (GetQuery(joins, \"AGG\") or BuildMongoAggregatePipeline)", relation)
			}
		}
	}
	return nil
}

// sortedRelations returns the preload relation names in a deterministic order.
func sortedRelations(preloads map[string][]figo.Expr) []string {
	relations := make([]string, 0, len(preloads))
	for relation := range preloads {
		relations = append(relations, relation)
	}
	sort.Strings(relations)
	return relations
}

// BuildMongoFindOptions produces FindOptions including sort, limit/skip.
// Sort and projection keys that would render in MongoDB operator position are
// dropped here (this signature has no error channel); the callers that do have
// one — AdapterMongoGetFind and MongoAdapter.GetQuery — reject the render.
func BuildMongoFindOptions(f figo.Figo) *options.FindOptions {
	opts, _ := buildMongoFindOptions(f)
	return opts
}

func buildMongoFindOptions(f figo.Figo) (*options.FindOptions, error) {
	opts := options.Find()
	var firstErr error
	// pagination
	p := f.GetPage()
	if p.Take > 0 {
		limit := int64(p.Take)
		opts.SetLimit(limit)
	}
	if p.Skip > 0 {
		skip := int64(p.Skip)
		opts.SetSkip(skip)
	}
	// sort
	sd, err := mongoSortDoc(f)
	if err != nil {
		firstErr = err
	}
	if len(sd) > 0 {
		opts.SetSort(sd)
	}
	// projection: honor AddSelectFields the way GORM (Select) and Elasticsearch
	// (_source) do, instead of silently returning full documents.
	names, err := mongoProjectionFields(f)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if len(names) > 0 {
		opts.SetProjection(mongoProjectionDoc(names))
	}
	return opts, firstErr
}

// mongoProjectionDoc renders projection field names as an ordered document.
// bson.D rather than bson.M: a multi-key map marshals its keys in random
// order, so the same figo state produced different wire bytes run to run.
func mongoProjectionDoc(names []string) bson.D {
	doc := make(bson.D, 0, len(names))
	for _, n := range names {
		doc = append(doc, bson.E{Key: n, Value: 1})
	}
	return doc
}

// mongoProjectionFields resolves AddSelectFields into normalized, de-duplicated
// and sorted projection keys. A '$'-prefixed key is refused for the same reason
// mongoExpr refuses a '$'-prefixed clause field: it lands in operator position,
// where the server rejects the whole query ("FieldPath field names may not
// start with '$'", error 16410).
func mongoProjectionFields(f figo.Figo) ([]string, error) {
	sel := f.GetSelectFields()
	if len(sel) == 0 {
		return nil, nil
	}
	var firstErr error
	seen := make(map[string]bool, len(sel))
	names := make([]string, 0, len(sel))
	for name := range sel {
		n := normalizeColumnName(f, name)
		if n == "" || seen[n] {
			continue
		}
		if strings.HasPrefix(n, "$") {
			if firstErr == nil {
				firstErr = fmt.Errorf("figo: select field %q would render as a MongoDB operator and was rejected", n)
			}
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return mongoProjectionPaths(names), firstErr
}

// mongoProjectionPaths sorts a projection key set (deterministic projection
// document) and prunes it so it can never contain BOTH a path and a prefix of
// that path. MongoDB rejects such a specification outright ("Invalid $project
// :: caused by :: Path collision at Orders.total", error 31249/31250, legacy
// 40176) and the ENTIRE aggregate — or Find — fails at the server while figo
// reported a successful build. Dropping the descendant is information
// preserving: an inclusion projection of "a" already includes "a.b", and
// MongoDB has no way to express "a plus a.b" other than "a".
func mongoProjectionPaths(names []string) []string {
	sort.Strings(names)
	// An ancestor is a strict prefix, so it always sorts before its
	// descendants: tracking the last KEPT key is enough to spot one.
	out := make([]string, 0, len(names))
	last := ""
	for _, n := range names {
		if n == last || (last != "" && strings.HasPrefix(n, last+".")) {
			continue
		}
		out = append(out, n)
		last = n
	}
	return out
}

// projectionHasDescendantOf reports whether some projection key addresses a
// field strictly UNDER the dotted path `path`.
func projectionHasDescendantOf(names []string, path string) bool {
	prefix := path + "."
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// mongoSortDoc renders the instance's sort as a bson.D (nil when unset),
// shared by BuildMongoFindOptions and the aggregation pipeline. Empty column
// names are skipped defensively — they would render {"": 1} — and '$'-prefixed
// ones are refused: {"$natural": -1} silently replaces the caller's ordering
// with a collection scan and {"$x": -1} makes the server reject the query.
// The returned document never contains a rejected key, so a caller without an
// error channel still cannot ship one.
func mongoSortDoc(f figo.Figo) (bson.D, error) {
	sortSpec := f.GetSort()
	if sortSpec == nil {
		return nil, nil
	}
	var sd bson.D
	var firstErr error
	for _, c := range sortSpec.Columns {
		if c.Name == "" {
			continue
		}
		if strings.HasPrefix(c.Name, "$") {
			if firstErr == nil {
				firstErr = fmt.Errorf("figo: sort key %q would render as a MongoDB operator and was rejected", c.Name)
			}
			continue
		}
		order := 1
		if c.Desc {
			order = -1
		}
		sd = append(sd, bson.E{Key: c.Name, Value: order})
	}
	return sd, firstErr
}

// BuildMongoAggregatePipeline builds an aggregation pipeline for preloads using $lookup.
// The pipeline begins with an optional $match for root filters, followed by $lookup for each preload,
// and optional $match stages to filter the joined arrays.
func BuildMongoAggregatePipeline(f figo.Figo, joins map[string]MongoJoin) (mongo.Pipeline, error) {
	return mongoAggregatePipeline(f, joins, mongoAdapterOf(f))
}

// mongoLookupLocalVar is the $lookup `let` variable carrying the parent's
// local join key into the correlated sub-pipeline. MongoDB requires a variable
// name that starts with a lowercase ASCII letter.
const mongoLookupLocalVar = "figoLocal"

// resolveMongoJoin looks up (and validates) the join description for a preload.
//
// The old fallback invented MongoJoin{From: relation} with EMPTY localField and
// foreignField, and returned err=nil. MongoDB parses both as FieldPaths and
// rejects the empty string ("FieldPath cannot be constructed with empty
// string", error 40352), so the whole aggregate failed at the server with no
// hint about which relation was unconfigured. The joins map is also keyed by
// the DSL relation name verbatim, so `load=[Orders:...]` against a map keyed
// "orders" landed on exactly this silent path — fail closed and name both.
func resolveMongoJoin(relation string, joins map[string]MongoJoin) (MongoJoin, error) {
	if strings.HasPrefix(relation, "$") {
		// Same control as the clause-field guard: the relation name becomes the
		// lookup output field, which a later stage addresses as a field path.
		return MongoJoin{}, fmt.Errorf("figo: preload relation %q would render as a MongoDB operator and was rejected", relation)
	}
	j, ok := joins[relation]
	if !ok {
		configured := make([]string, 0, len(joins))
		for k := range joins {
			configured = append(configured, k)
		}
		sort.Strings(configured)
		return MongoJoin{}, fmt.Errorf("figo: no MongoJoin configured for preload %q (the joins map is keyed by the relation name exactly as it appears in load=; configured keys: %v)", relation, configured)
	}
	if j.As == "" {
		j.As = relation
	}
	if strings.HasPrefix(j.As, "$") {
		return MongoJoin{}, fmt.Errorf("figo: MongoJoin.As %q for preload %q would render as a MongoDB operator and was rejected", j.As, relation)
	}
	if j.From == "" || j.LocalField == "" || j.ForeignField == "" {
		return MongoJoin{}, fmt.Errorf("figo: MongoJoin for preload %q needs From, LocalField and ForeignField (got From=%q LocalField=%q ForeignField=%q); MongoDB rejects an empty FieldPath (error 40352)", relation, j.From, j.LocalField, j.ForeignField)
	}
	// Both join keys are rendered as aggregation field paths ("$"+name), so a
	// name that already starts with '$' produces "$$name" — a VARIABLE
	// reference, not the field the caller named.
	for _, kv := range [...][2]string{{"LocalField", j.LocalField}, {"ForeignField", j.ForeignField}} {
		if strings.HasPrefix(kv[1], "$") {
			return MongoJoin{}, fmt.Errorf("figo: MongoJoin.%s %q for preload %q would render as a MongoDB variable reference and was rejected", kv[0], kv[1], relation)
		}
	}
	return j, nil
}

func mongoAggregatePipeline(f figo.Figo, joins map[string]MongoJoin, a MongoAdapter) (mongo.Pipeline, error) {
	pipeline := mongo.Pipeline{}

	// preloads, in a deterministic order: ranging the map emitted the $lookup
	// (and its $match) stages in a different order on every build, so the same
	// figo state produced a different pipeline document each time — defeating
	// golden tests, plan caching and any diffing of generated queries. The raw
	// adapter sorts its JOIN tables for the same reason.
	preloadExprs := f.GetPreloads()
	relations := sortedRelations(preloadExprs)

	// Resolve every join before emitting a stage, so a mis-keyed joins map
	// fails the whole build rather than half of it.
	resolved := make([]MongoJoin, len(relations))
	aliases := make(map[string]bool, len(relations))
	for i, relation := range relations {
		j, err := resolveMongoJoin(relation, joins)
		if err != nil {
			return nil, err
		}
		// Two relations writing the same output field would make the second
		// $lookup overwrite the first's array — a silently lost preload.
		if aliases[j.As] {
			return nil, fmt.Errorf("figo: two preloads share the MongoJoin.As output field %q; the second $lookup would overwrite the first", j.As)
		}
		resolved[i] = j
		aliases[j.As] = true
	}

	// root filter
	rootMatch, err := buildMongoFilterFromExprs(f.GetClauses(), a.render())
	if err != nil {
		return nil, err
	}

	// A root clause that addresses a lookup alias ("Orders.total>5") can only be
	// evaluated once the $lookup has created that field; matched first it saw a
	// missing field and returned zero rows. $text is the exception: MongoDB
	// requires a $match carrying $text to be the FIRST pipeline stage, so the
	// two demands are mutually exclusive and the build fails loudly instead of
	// emitting a pipeline the server rejects.
	rootAfterLookups := len(rootMatch) > 0 && len(relations) > 0 && exprsReferenceAlias(f.GetClauses(), aliases)
	if rootAfterLookups && anyContainsFullText(f.GetClauses()) {
		return nil, fmt.Errorf("figo: a root clause referencing a preloaded relation cannot be combined with full-text search ($text) on the MongoDB adapter — $text must be the first pipeline stage")
	}
	if len(rootMatch) > 0 && !rootAfterLookups {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: rootMatch}})
	}

	for i, relation := range relations {
		j := resolved[i]

		// The preload predicates belong to the JOINED documents, so they are
		// rendered inside a correlated $lookup sub-pipeline rather than as a
		// parent-level $match on the produced array. The old form was wrong
		// three ways at once: on an array path MongoDB satisfies each predicate
		// with a possibly DIFFERENT element ("combination of elements"), so
		// `status="paid" amount>100` matched a parent with a cheap paid order
		// and an expensive unpaid one; $ne/$nin/$nor negated the existential
		// ("no element is paid" instead of "some element isn't"); and the array
		// itself was never filtered, so `load=[Orders:tenant_id=7]` handed back
		// every parent's COMPLETE orders array, other tenants included.
		// The join condition is the correlated `let` + `pipeline` form, which
		// MongoDB has supported since 3.6. Note the one semantic difference from
		// the localField/foreignField form it replaces: that form matches
		// element-wise when either join key holds an ARRAY, while $expr/$eq
		// compares the values whole. Scalar join keys — the overwhelmingly
		// common case, and the only one the empty-fallback ever worked for —
		// behave identically.
		sub := mongo.Pipeline{
			bson.D{{Key: "$match", Value: bson.M{"$expr": bson.M{
				"$eq": []any{"$" + j.ForeignField, "$$" + mongoLookupLocalVar},
			}}}},
		}
		childMatch, err := buildMongoFilterFromExprs(preloadExprs[relation], a.renderPreload(relation))
		if err != nil {
			return nil, err
		}
		if len(childMatch) > 0 {
			sub = append(sub, bson.D{{Key: "$match", Value: childMatch}})
		}
		pipeline = append(pipeline, bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: j.From},
			{Key: "let", Value: bson.D{{Key: mongoLookupLocalVar, Value: "$" + j.LocalField}}},
			{Key: "pipeline", Value: sub},
			{Key: "as", Value: j.As},
		}}})

		// INNER-JOIN parent semantics: the parent survives only if at least one
		// child passed the filter. Now that the children are filtered first this
		// is a correct existential quantifier.
		//
		// CAUTION — this is the one place where the adapters DISAGREE, and the
		// justification originally written here ("the raw adapter emits JOIN ...
		// ON <preload filters>") is no longer true: hunt #6's H9/H10 removed
		// that JOIN, and its regression test now pins the opposite contract for
		// raw ("a preload must neither multiply nor annihilate the primary
		// rows", raw_hunt67_regression_test.go). GORM's Preload runs a second
		// query and likewise leaves the parent set alone; Elasticsearch fails
		// the build for load= outright. So `load=[Orders:total>60]` keeps every
		// parent on raw and GORM and keeps only the matching parents here.
		// mongo_semantic_oracle_test.go pins that divergence so it cannot change
		// silently — do not "align" it in either direction without deciding
		// which contract the library means, because both directions change
		// somebody's row set.
		if len(childMatch) > 0 {
			pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{
				j.As + ".0": bson.M{"$exists": true},
			}}})
		}
	}

	if rootAfterLookups {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: rootMatch}})
	}

	// sort= and page= must survive the aggregation path with the same
	// semantics BuildMongoFindOptions gives Find: Take/Skip <= 0 mean
	// "no limit"/"no offset", so those stages are simply omitted.
	sd, err := mongoSortDoc(f)
	if err != nil {
		return nil, err
	}
	if len(sd) > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$sort", Value: sd}})
	}
	p := f.GetPage()
	if p.Skip > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$skip", Value: int64(p.Skip)}})
	}
	if p.Take > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$limit", Value: int64(p.Take)}})
	}

	// AddSelectFields was honored on the Find path and silently ignored here,
	// so merely adding load= to a query turned a narrowed projection back into
	// "return whole documents". $project goes LAST so it cannot strip a field
	// $match/$sort still need, and the lookup outputs are carried through —
	// dropping them would defeat the load= that selected this path.
	names, err := mongoProjectionFields(f)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		for _, j := range resolved {
			// An alias the caller already addressed must NOT be added on top of
			// the caller's own key: {"Orders":1,"Orders.total":1} names both a
			// path and its prefix, which MongoDB refuses (path collision, error
			// 31249/31250) — the whole aggregate failed at the server while the
			// build reported success. "Orders.total" already carries the lookup
			// output through, narrowed exactly as the caller asked, so the load=
			// this path was selected for is not defeated.
			if projectionHasDescendantOf(names, j.As) {
				continue
			}
			names = append(names, j.As)
		}
		// mongoProjectionPaths re-sorts and enforces the no-path-and-its-prefix
		// invariant for the merged set as well (a caller key can be an ANCESTOR
		// of an alias when MongoJoin.As is itself dotted).
		pipeline = append(pipeline, bson.D{{Key: "$project", Value: mongoProjectionDoc(mongoProjectionPaths(names))}})
	}

	return pipeline, nil
}

// exprsReferenceAlias reports whether any leaf in exprs filters on a field
// whose first dotted segment is one of the $lookup output aliases.
func exprsReferenceAlias(exprs []figo.Expr, aliases map[string]bool) bool {
	for _, e := range exprs {
		if e == nil {
			continue
		}
		switch x := e.(type) {
		case figo.AndExpr:
			if exprsReferenceAlias(x.Operands, aliases) {
				return true
			}
		case figo.OrExpr:
			if exprsReferenceAlias(x.Operands, aliases) {
				return true
			}
		case figo.NotExpr:
			if exprsReferenceAlias(x.Operands, aliases) {
				return true
			}
		default:
			field := figo.ExprField(e)
			if i := strings.IndexByte(field, '.'); i > 0 && aliases[field[:i]] {
				return true
			}
		}
	}
	return false
}

// mongoRender carries per-build rendering context: the preload relation whose
// $lookup sub-pipeline is being rendered (empty at the root), whether the node
// is being rendered underneath a NOT, and the set of fields whose hex-string
// values convert to ObjectIDs.
type mongoRender struct {
	preload   string
	negated   bool
	oidFields map[string]bool
}

// under returns the context for the operands of a NotExpr. Tracking negation
// downward replaces the guard that rescanned the whole remaining subtree at
// every NotExpr to answer the same question — that made rendering QUADRATIC in
// the NOT nesting depth (a 32 KB `not not not ... a=1` chain took 260ms, four
// times longer for every doubling, while gorm and Elasticsearch stayed linear).
func (rc mongoRender) under() mongoRender {
	rc.negated = true
	return rc
}

// mongoValueDepthLimit bounds mongoCheckValue's descent into a nested value.
// It also terminates the scan on a self-referential value instead of looping.
const mongoValueDepthLimit = 64

// typeByte and typeByteSlice separate the ONE byte-slice type the BSON encoder
// writes as opaque binary (plain []byte) from every other slice-of-byte type,
// which may carry its own codec. See the reflect.Slice arm of mongoCheckValue.
var (
	typeByte      = reflect.TypeOf(byte(0))
	typeByteSlice = reflect.TypeOf([]byte(nil))
)

// mongoCheckValue rejects a bound value that would be read as query STRUCTURE
// rather than as data. A value is not escaped or parameterized on the way into
// a BSON filter document, so EqExpr{Field: "password", Value: map[string]any
// {"$ne": nil}} rendered {"password":{"$ne":null}} — the textbook NoSQL
// auth-bypass payload — where the SQL adapters bind the same value inertly.
// Any '$'-prefixed key reachable from the value is refused; this is the value
// half of the '$'-prefixed field-name guard in mongoExpr.
func mongoCheckValue(field string, v any, depth int) error {
	if v == nil {
		return nil
	}
	switch v.(type) {
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, time.Time, primitive.ObjectID, primitive.Regex, primitive.DateTime:
		return nil
	}
	if depth > mongoValueDepthLimit {
		return fmt.Errorf("figo: value for field %q nests deeper than %d levels and was rejected by the MongoDB adapter", field, mongoValueDepthLimit)
	}
	if d, ok := v.(bson.D); ok {
		for _, e := range d {
			if strings.HasPrefix(e.Key, "$") {
				return mongoValueOperatorError(field, e.Key)
			}
			if err := mongoCheckValue(field, e.Value, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	// A bson.RawValue is pre-encoded BSON. Its Value field is a plain []byte,
	// so neither the byte-slice arm nor the struct arm below can see the keys
	// inside it — it has to be decoded as BSON.
	if rv, ok := v.(bson.RawValue); ok {
		return mongoCheckRawValue(field, rv, depth)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		iter := rv.MapRange()
		for iter.Next() {
			if k := iter.Key(); k.Kind() == reflect.String && strings.HasPrefix(k.String(), "$") {
				return mongoValueOperatorError(field, k.String())
			}
			if err := mongoCheckValue(field, iter.Value().Interface(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if rv.Type().Elem() == typeByte {
			if rv.Type() == typeByteSlice {
				return nil // a plain []byte is BSON binary: opaque data, no keys
			}
			// A NAMED byte-slice type is not necessarily binary: the driver
			// registers document encoders for bson.Raw and bsoncore.Document,
			// both `type X []byte`, which write an embedded DOCUMENT — so the
			// bytes ARE query structure. Exempting on the element KIND let a
			// bson.Raw of {"$ne":null} (or {"$where":"…"}) straight through the
			// guard this function exists to be. Anything that does not parse as
			// BSON is treated as binary, exactly as before.
			return mongoCheckRawDocument(field, rv.Bytes(), depth)
		}
		for i := 0; i < rv.Len(); i++ {
			if err := mongoCheckValue(field, rv.Index(i).Interface(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		// A struct is encoded as a document keyed by its `bson` tags, so a field
		// tagged `bson:"$ne"` reaches the wire as an OPERATOR just like the
		// map key this guard already refuses. Unexported fields are skipped
		// because the default StructCodec skips them too (struct_codec.go:527,
		// AllowUnexportedFields off).
		t := rv.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" {
				continue
			}
			key, encoded, inlined := mongoStructFieldKey(sf)
			if !encoded {
				continue
			}
			// An inlined field contributes its own keys, not this one.
			if !inlined && strings.HasPrefix(key, "$") {
				return mongoValueOperatorError(field, key)
			}
			if err := mongoCheckValue(field, rv.Field(i).Interface(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return mongoCheckValue(field, rv.Elem().Interface(), depth+1)
	}
	return nil
}

// mongoStructFieldKey resolves the BSON key a struct field is encoded under,
// mirroring the driver's DefaultStructTagParser: the first comma-separated part
// of the `bson` tag, falling back to the lower-cased field name; a whole tag of
// "-" means the field is not encoded at all. `encoded` is false for a field the
// encoder skips, `inlined` true when the field's own key is not emitted.
func mongoStructFieldKey(sf reflect.StructField) (key string, encoded, inlined bool) {
	tag, ok := sf.Tag.Lookup("bson")
	if !ok && len(sf.Tag) > 0 && !strings.Contains(string(sf.Tag), ":") {
		tag = string(sf.Tag) // legacy un-keyed tag form, which the driver honors
	}
	if tag == "-" {
		return "", false, false
	}
	key = strings.ToLower(sf.Name)
	for i, part := range strings.Split(tag, ",") {
		if i == 0 && part != "" {
			key = part
		}
		if part == "inline" {
			inlined = true
		}
	}
	return key, true, inlined
}

// mongoCheckRawDocument applies the operator-key test to pre-encoded BSON. Bytes
// that are not a valid BSON document are accepted as binary data.
func mongoCheckRawDocument(field string, raw []byte, depth int) error {
	if depth > mongoValueDepthLimit {
		return fmt.Errorf("figo: value for field %q nests deeper than %d levels and was rejected by the MongoDB adapter", field, mongoValueDepthLimit)
	}
	elems, err := bson.Raw(raw).Elements()
	if err != nil {
		return nil
	}
	for _, e := range elems {
		key, err := e.KeyErr()
		if err != nil {
			return nil
		}
		if strings.HasPrefix(key, "$") {
			return mongoValueOperatorError(field, key)
		}
		ev, err := e.ValueErr()
		if err != nil {
			return nil
		}
		if err := mongoCheckRawValue(field, ev, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// mongoCheckRawValue descends into a pre-encoded BSON value. Only embedded
// documents and arrays can carry keys; every other BSON type is a leaf.
func mongoCheckRawValue(field string, v bson.RawValue, depth int) error {
	switch v.Type {
	case bsontype.EmbeddedDocument:
		doc, ok := v.DocumentOK()
		if !ok {
			return nil
		}
		return mongoCheckRawDocument(field, doc, depth)
	case bsontype.Array:
		arr, ok := v.ArrayOK()
		if !ok {
			return nil
		}
		// Array element keys are the indices "0","1",… — only the values can
		// hold an operator document.
		vals, err := arr.Values()
		if err != nil {
			return nil
		}
		if depth > mongoValueDepthLimit {
			return fmt.Errorf("figo: value for field %q nests deeper than %d levels and was rejected by the MongoDB adapter", field, mongoValueDepthLimit)
		}
		for _, ev := range vals {
			if err := mongoCheckRawValue(field, ev, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func mongoValueOperatorError(field, key string) error {
	return fmt.Errorf("figo: the value for field %q carries the MongoDB operator key %q and was rejected — a filter value is data, not query structure", field, key)
}

// value converts a hex-string value to primitive.ObjectID for configured
// reference fields. Anything that is not a valid ObjectID hex string passes
// through unchanged, so mistyped ids simply match nothing instead of erroring.
func (rc mongoRender) value(field string, v any) (any, error) {
	if err := mongoCheckValue(field, v, 0); err != nil {
		return nil, err
	}
	if !rc.oidFields[field] {
		return v, nil
	}
	if s, ok := v.(string); ok {
		if oid, err := primitive.ObjectIDFromHex(s); err == nil {
			return oid, nil
		}
	}
	return v, nil
}

// values renders an $in/$nin operand list. A nil slice must become a real
// (possibly empty) array: nil marshals to BSON null and MongoDB rejects the
// query at runtime ("$in needs an array"); an empty array is valid and matches
// nothing ($in) / everything ($nin).
func (rc mongoRender) values(field string, vals []any) ([]any, error) {
	if vals == nil {
		return []any{}, nil
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		converted, err := rc.value(field, v)
		if err != nil {
			return nil, err
		}
		out[i] = converted
	}
	return out, nil
}

// mongoAdapterOf returns the MongoAdapter configured on the figo instance (via
// Build or SetAdapterObject). A figo built with a different or nil adapter
// gets the zero-value MongoAdapter defaults.
func mongoAdapterOf(f figo.Figo) MongoAdapter {
	if f != nil {
		switch t := f.GetAdapterObject().(type) {
		case MongoAdapter:
			return t
		case *MongoAdapter:
			if t != nil {
				return *t
			}
		}
	}
	return MongoAdapter{}
}

// Helper: convert a list of figo.Expr to a bson.M filter
func buildMongoFilterFromExprs(exprs []figo.Expr, rc mongoRender) (bson.M, error) {
	if len(exprs) == 0 {
		return bson.M{}, nil
	}
	// If multiple top-level expressions exist, combine with $and
	parts, err := mongoOperands(exprs, rc)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return bson.M{}, nil
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return bson.M{"$and": parts}, nil
}

// mongoOperands renders a set of operands, propagating the first error.
func mongoOperands(ops []figo.Expr, rc mongoRender) ([]bson.M, error) {
	var parts []bson.M
	for _, op := range ops {
		if op == nil {
			continue
		}
		m, err := mongoExpr(op, rc)
		if err != nil {
			return nil, err
		}
		if len(m) > 0 {
			parts = append(parts, m)
		}
	}
	return parts, nil
}

// Translate figo.Expr to MongoDB filter fragment
func mongoExpr(e figo.Expr, rc mongoRender) (bson.M, error) {
	// A field name beginning with '$' would land in operator position in the
	// filter document — {"$where": "..."} EXECUTES as an operator, not a field
	// match. The default snake_case naming can't produce one, but a permissive
	// NamingFunc (NoChangeNaming) plus attacker-influenced field names would
	// inject straight into the query engine. Rejecting here is the analogue of
	// the SQL adapters' identifier quoting. ('$' later in a dotted path stays
	// legal: "a.$b" is a path component, not an operator.)
	if field := figo.ExprField(e); strings.HasPrefix(field, "$") {
		return nil, fmt.Errorf("figo: field name %q would render as a MongoDB operator and was rejected", field)
	}
	switch x := e.(type) {
	case figo.EqExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: v}, nil
	case figo.GteExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$gte": v}}, nil
	case figo.GtExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$gt": v}}, nil
	case figo.LtExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$lt": v}}, nil
	case figo.LteExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$lte": v}}, nil
	case figo.NeqExpr:
		v, err := rc.value(x.Field, x.Value)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$ne": v}}, nil
	case figo.LikeExpr:
		return bson.M{x.Field: primitive.Regex{Pattern: likeToRegexPattern(x.Value), Options: likeRegexOptions}}, nil
	case figo.RegexExpr:
		re, ok := mongoRawRegex(x.Value)
		if !ok {
			return nil, fmt.Errorf("figo: invalid regex value %v for field %q", x.Value, x.Field)
		}
		return bson.M{x.Field: re}, nil
	case figo.ILikeExpr:
		return bson.M{x.Field: primitive.Regex{Pattern: likeToRegexPattern(x.Value), Options: "i" + likeRegexOptions}}, nil
	case figo.IsNullExpr:
		// Match both explicit null and missing (SQL IS NULL semantics), not just
		// missing as {$exists:false} would.
		return bson.M{x.Field: nil}, nil
	case figo.NotNullExpr:
		return bson.M{x.Field: bson.M{"$ne": nil}}, nil
	case figo.InExpr:
		vals, err := rc.values(x.Field, x.Values)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$in": vals}}, nil
	case figo.NotInExpr:
		vals, err := rc.values(x.Field, x.Values)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$nin": vals}}, nil
	case figo.BetweenExpr:
		low, err := rc.value(x.Field, x.Low)
		if err != nil {
			return nil, err
		}
		high, err := rc.value(x.Field, x.High)
		if err != nil {
			return nil, err
		}
		// bson.D, not bson.M: a two-key map marshals its keys in random order,
		// so identical figo state produced different wire bytes run to run.
		return bson.M{x.Field: bson.D{{Key: "$gte", Value: low}, {Key: "$lte", Value: high}}}, nil
	case figo.JsonPathExpr:
		return mongoJSONPath(x, rc)
	case figo.ArrayContainsExpr:
		if len(x.Values) == 0 {
			// Requiring no elements is vacuously true. Mongo's {$all: []} matches
			// NOTHING, so an empty predicate is the correct rendering.
			return bson.M{}, nil
		}
		vals, err := rc.values(x.Field, x.Values)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$all": vals}}, nil
	case figo.ArrayOverlapsExpr:
		// intersect-ANY: the array field shares at least one element with Values.
		vals, err := rc.values(x.Field, x.Values)
		if err != nil {
			return nil, err
		}
		return bson.M{x.Field: bson.M{"$in": vals}}, nil
	case figo.FullTextSearchExpr:
		// $text is illegal under $nor at any depth — MongoDB rejects the query
		// server-side — so fail the build with a clear error instead.
		if rc.negated {
			return nil, fmt.Errorf("figo: full-text search ($text) cannot be negated ($nor) on the MongoDB adapter")
		}
		// $text is only legal at the top level of a query; inside a $lookup
		// sub-pipeline it would produce a pipeline MongoDB rejects at runtime.
		if rc.preload != "" {
			return nil, fmt.Errorf("figo: full-text search ($text) is not supported inside the %q preload match for the MongoDB adapter", rc.preload)
		}
		// bson.D, not bson.M: see BetweenExpr — $search + $language is the other
		// multi-key sub-document in this renderer.
		txt := bson.D{{Key: "$search", Value: x.Query}}
		if x.Language != "" {
			txt = append(txt, bson.E{Key: "$language", Value: x.Language})
		}
		return bson.M{"$text": txt}, nil
	case figo.GeoDistanceExpr:
		if math.IsNaN(x.Latitude) || math.IsInf(x.Latitude, 0) || math.IsNaN(x.Longitude) || math.IsInf(x.Longitude, 0) {
			return nil, fmt.Errorf("figo: geo coordinates for field %q must be finite, got lat=%v lng=%v", x.Field, x.Latitude, x.Longitude)
		}
		km, err := geoDistanceKm(x.Distance, x.Unit)
		if err != nil {
			return nil, err
		}
		// $centerSphere takes [lng, lat] (longitude FIRST) and a radius in
		// radians (distance / Earth radius).
		return bson.M{x.Field: bson.M{"$geoWithin": bson.M{
			"$centerSphere": []any{[]float64{x.Longitude, x.Latitude}, km / earthRadiusKm},
		}}}, nil
	case figo.AndExpr:
		parts, err := mongoOperands(x.Operands, rc)
		if err != nil {
			return nil, err
		}
		return logicalBSON("$and", parts), nil
	case figo.OrExpr:
		parts, err := mongoOperands(x.Operands, rc)
		if err != nil {
			return nil, err
		}
		return logicalBSON("$or", parts), nil
	case figo.NotExpr:
		nrc := rc.under()
		parts := make([]bson.M, 0, len(x.Operands))
		sawOperand := false
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			sawOperand = true
			m, err := mongoExpr(op, nrc)
			if err != nil {
				return nil, err
			}
			if len(m) == 0 {
				// The operand rendered a match-all predicate ({}), so its
				// negation matches NOTHING. mongoOperands dropped it, leaving
				// {} — match everything: fail open (raw/ES: NOT(TRUE)=FALSE).
				return bson.M{"$nor": []bson.M{{}}}, nil
			}
			parts = append(parts, m)
		}
		if !sawOperand {
			// NOT of no operands stays the vacuous-true identity, like the
			// raw adapter's "" and logicalBSON's empty $nor.
			return bson.M{}, nil
		}
		return bson.M{"$nor": parts}, nil
	case figo.OrderBy:
		return bson.M{}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported expression type %T for the MongoDB adapter", e)
	}
}

// mongoJSONPath renders a JSON path predicate as a dotted-path match: Mongo
// addresses nested documents natively, so path $.user.name on field "data"
// becomes "data.user.name".
func mongoJSONPath(x figo.JsonPathExpr, rc mongoRender) (bson.M, error) {
	if err := mongoCheckValue(x.Field, x.Value, 0); err != nil {
		return nil, err
	}
	path := x.Field + "." + strings.TrimPrefix(x.Path, "$.")
	switch x.Op {
	case "", "=", "==", "contains":
		// Mongo equality already has contains semantics on array fields.
		return bson.M{path: x.Value}, nil
	case "!=":
		return bson.M{path: bson.M{"$ne": x.Value}}, nil
	case ">":
		return bson.M{path: bson.M{"$gt": x.Value}}, nil
	case ">=":
		return bson.M{path: bson.M{"$gte": x.Value}}, nil
	case "<":
		return bson.M{path: bson.M{"$lt": x.Value}}, nil
	case "<=":
		return bson.M{path: bson.M{"$lte": x.Value}}, nil
	case "exists":
		return bson.M{path: bson.M{"$exists": true}}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported JSON path op %q for the MongoDB adapter", x.Op)
	}
}

// exprContainsFullText reports whether the expression tree contains a
// FullTextSearchExpr at any depth. It is a ONE-SHOT check on the root clause
// list (the $text-must-be-the-first-stage conflict in mongoAggregatePipeline);
// negation is tracked downward by mongoRender.negated instead, because calling
// this from every NotExpr made rendering quadratic in the nesting depth.
func exprContainsFullText(e figo.Expr) bool {
	switch x := e.(type) {
	case figo.FullTextSearchExpr:
		return true
	case figo.AndExpr:
		return anyContainsFullText(x.Operands)
	case figo.OrExpr:
		return anyContainsFullText(x.Operands)
	case figo.NotExpr:
		return anyContainsFullText(x.Operands)
	}
	return false
}

func anyContainsFullText(ops []figo.Expr) bool {
	for _, op := range ops {
		if op != nil && exprContainsFullText(op) {
			return true
		}
	}
	return false
}

// logicalBSON wraps rendered operands under a logical operator, avoiding an
// invalid "{$and: null}" (Mongo rejects an empty/nil operand array).
func logicalBSON(op string, parts []bson.M) bson.M {
	if len(parts) == 0 {
		// Empty $or is a false disjunction and must match NOTHING; returning {}
		// (match everything) over-exposes the whole collection. $nor:[{}] negates
		// a match-all, so it reliably matches nothing without an empty $or array
		// (which Mongo rejects). Empty $and (true) and empty $nor (¬false = true)
		// correctly stay match-all as {}.
		if op == "$or" {
			return bson.M{"$nor": []bson.M{{}}}
		}
		return bson.M{}
	}
	return bson.M{op: parts}
}

// likeRegexOptions is the option string every LIKE-derived regex carries. SQL's
// '%' and '_' match ANY character, newlines included; a PCRE '.' does not match
// a newline unless the DOTALL ('s') flag is set, so without it LIKE "a%b" quietly
// failed to match "a\nb" — a false negative no other adapter has. 'm' must NOT
// be added: it would make '^'/'$' match at line boundaries and unanchor the
// pattern.
const likeRegexOptions = "s"

// likeToRegexPattern converts a SQL LIKE pattern into an anchored regex pattern
// string: '%' -> '.*', '_' -> '.', every other character is regex-escaped, and
// the result is anchored with ^...$. Anchoring matters because Mongo $regex is
// unanchored — without it, LIKE "abc" would match "xabcx" instead of exactly
// "abc", and the '_' single-char wildcard would not match at all.
func likeToRegexPattern(v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range s {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	return b.String()
}

// mongoRawRegex builds a BSON regex value from a figo.RegexExpr's raw pattern. The
// pattern MUST be handed to Mongo as a string (primitive.Regex), not a compiled
// *regexp.Regexp — the BSON encoder serializes *regexp.Regexp (all-unexported
// fields) as an empty document, so "$regex": {} matches nothing.
func mongoRawRegex(v any) (primitive.Regex, bool) {
	switch t := v.(type) {
	case *regexp.Regexp:
		return primitive.Regex{Pattern: t.String()}, true
	case string:
		if _, err := regexp.Compile(t); err != nil {
			return primitive.Regex{}, false
		}
		return primitive.Regex{Pattern: t}, true
	default:
		return primitive.Regex{}, false
	}
}

// AdapterMongoGetFind returns filter and find options for a simple Find operation
func AdapterMongoGetFind(f figo.Figo) (bson.M, *options.FindOptions, error) {
	filter, err := BuildMongoFilter(f)
	if err != nil {
		return nil, nil, err
	}
	opts, err := buildMongoFindOptions(f)
	if err != nil {
		return nil, nil, err
	}
	return filter, opts, nil
}

// AdapterMongoGetAggregate returns an aggregation pipeline and options based on joins
func AdapterMongoGetAggregate(f figo.Figo, joins map[string]MongoJoin) (mongo.Pipeline, *options.AggregateOptions, error) {
	pipeline, err := BuildMongoAggregatePipeline(f, joins)
	if err != nil {
		return nil, nil, err
	}
	opts := options.Aggregate()
	return pipeline, opts, nil
}

// MongoAdapter renders figo queries as MongoDB filters and pipelines; it
// doesn't render SQL strings.
//
// ObjectIDFields lists the fields whose valid 24-character hex string values
// are converted to primitive.ObjectID at render time — without it, the common
// DSL lookup `_id="507f..."` compares a string against real ObjectIDs and
// never matches. nil (the zero value) converts just "_id"; an explicit empty
// slice disables conversion entirely.
type MongoAdapter struct {
	ObjectIDFields []string
}

// objectIDFieldSet resolves the ObjectIDFields config into a lookup set,
// applying the nil-means-default-"_id" rule.
func (a MongoAdapter) objectIDFieldSet() map[string]bool {
	if a.ObjectIDFields == nil {
		return map[string]bool{"_id": true}
	}
	set := make(map[string]bool, len(a.ObjectIDFields))
	for _, f := range a.ObjectIDFields {
		set[f] = true
	}
	return set
}

// render builds the root rendering context for this adapter's configuration.
func (a MongoAdapter) render() mongoRender {
	return mongoRender{oidFields: a.objectIDFieldSet()}
}

// renderPreload builds the rendering context for a preload's $lookup
// sub-pipeline. Field names are NOT qualified there: the sub-pipeline runs
// against the joined collection, so "status" means the child's own field.
func (a MongoAdapter) renderPreload(relation string) mongoRender {
	return mongoRender{preload: relation, oidFields: a.objectIDFieldSet()}
}

func (MongoAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	// Mongo adapter doesn't produce SQL strings; return false
	if f == nil {
		return "", false
	}
	return "", false
}

// MongoFindQuery and MongoAggregateQuery are typed results for GetQuery
// They satisfy the figo.Query interface

type MongoFindQuery struct {
	Filter  bson.M
	Options *options.FindOptions
}

func (MongoFindQuery) IsQuery() {}

type MongoAggregateQuery struct {
	Pipeline mongo.Pipeline
	Options  *options.AggregateOptions
}

func (MongoAggregateQuery) IsQuery() {}

func (a MongoAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return nil, false
	}
	// Decide between Find and Aggregate based on conditionType hint
	isAgg := false
	for _, ct := range conditionType {
		up := strings.ToUpper(strings.TrimSpace(ct))
		if up == "AGG" || up == "AGGREGATE" || up == "PIPELINE" {
			isAgg = true
			break
		}
	}
	if isAgg {
		var joins map[string]MongoJoin
		if j, ok := ctx.(map[string]MongoJoin); ok {
			joins = j
		}
		// Render with the receiver's configuration, so a directly-invoked
		// adapter wins over whatever f has stored.
		pipe, err := mongoAggregatePipeline(f, joins, a)
		if err != nil {
			// An unsupported expression must not be silently dropped — fail the
			// query build rather than returning a partial pipeline.
			return nil, false
		}
		return MongoAggregateQuery{Pipeline: pipe, Options: options.Aggregate()}, true
	}
	// Fail closed the way the aggregate branch does: preload predicates the
	// Find path cannot express, and sort/projection keys that would render in
	// operator position, must not be silently dropped.
	if err := mongoFindPreloadError(f); err != nil {
		return nil, false
	}
	filter, err := buildMongoFilterFromExprs(f.GetClauses(), a.render())
	if err != nil {
		return nil, false
	}
	opts, err := buildMongoFindOptions(f)
	if err != nil {
		return nil, false
	}
	return MongoFindQuery{Filter: filter, Options: opts}, true
}
