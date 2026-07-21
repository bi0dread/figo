package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
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
func geoDistanceKm(distance float64, unit string) (float64, error) {
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
// does not support (rather than silently dropping the condition).
func BuildMongoFilter(f figo.Figo) (bson.M, error) {
	return buildMongoFilterFromExprs(f.GetClauses(), mongoAdapterOf(f).render(""))
}

// BuildMongoFindOptions produces FindOptions including sort, limit/skip
func BuildMongoFindOptions(f figo.Figo) *options.FindOptions {
	opts := options.Find()
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
	sort := f.GetSort()
	if sort != nil {
		var sd bson.D
		for _, c := range sort.Columns {
			order := 1
			if c.Desc {
				order = -1
			}
			sd = append(sd, bson.E{Key: c.Name, Value: order})
		}
		if len(sd) > 0 {
			opts.SetSort(sd)
		}
	}
	// projection: honor AddSelectFields the way GORM (Select) and Elasticsearch
	// (_source) do, instead of silently returning full documents.
	if sel := f.GetSelectFields(); len(sel) > 0 {
		proj := bson.M{}
		for name := range sel {
			proj[normalizeColumnName(f, name)] = 1
		}
		opts.SetProjection(proj)
	}
	return opts
}

// BuildMongoAggregatePipeline builds an aggregation pipeline for preloads using $lookup.
// The pipeline begins with an optional $match for root filters, followed by $lookup for each preload,
// and optional $match stages to filter the joined arrays.
func BuildMongoAggregatePipeline(f figo.Figo, joins map[string]MongoJoin) (mongo.Pipeline, error) {
	return mongoAggregatePipeline(f, joins, mongoAdapterOf(f))
}

func mongoAggregatePipeline(f figo.Figo, joins map[string]MongoJoin, a MongoAdapter) (mongo.Pipeline, error) {
	pipeline := mongo.Pipeline{}

	// root filter
	rootMatch, err := buildMongoFilterFromExprs(f.GetClauses(), a.render(""))
	if err != nil {
		return nil, err
	}
	if len(rootMatch) > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: rootMatch}})
	}

	// preloads
	for preload, exprs := range f.GetPreloads() {
		j, ok := joins[preload]
		if !ok {
			// fallback: assume From = preload, LocalField = "", ForeignField = "", As = preload
			j = MongoJoin{From: preload, As: preload}
		}
		lookup := bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: j.From},
			{Key: "localField", Value: j.LocalField},
			{Key: "foreignField", Value: j.ForeignField},
			{Key: "as", Value: j.As},
		}}}
		pipeline = append(pipeline, lookup)

		// if there are filters for the joined docs, add a match on as.field
		if len(exprs) > 0 {
			m, err := buildMongoFilterFromExprs(exprs, a.render(j.As))
			if err != nil {
				return nil, err
			}
			if len(m) > 0 {
				pipeline = append(pipeline, bson.D{{Key: "$match", Value: m}})
			}
		}
	}

	return pipeline, nil
}

// mongoRender carries per-build rendering context: an optional field qualifier
// (set for $lookup sub-document matches, where "field" becomes "As.field") and
// the set of fields whose hex-string values convert to ObjectIDs.
type mongoRender struct {
	qualifier string
	oidFields map[string]bool
}

// key qualifies a field name for the current match context.
func (rc mongoRender) key(field string) string {
	if rc.qualifier == "" {
		return field
	}
	return rc.qualifier + "." + field
}

// value converts a hex-string value to primitive.ObjectID for configured
// reference fields (matched on the unqualified field name). Anything that is
// not a valid ObjectID hex string passes through unchanged, so mistyped ids
// simply match nothing instead of erroring.
func (rc mongoRender) value(field string, v any) any {
	if !rc.oidFields[field] {
		return v
	}
	if s, ok := v.(string); ok {
		if oid, err := primitive.ObjectIDFromHex(s); err == nil {
			return oid
		}
	}
	return v
}

// values renders an $in/$nin operand list. A nil slice must become a real
// (possibly empty) array: nil marshals to BSON null and MongoDB rejects the
// query at runtime ("$in needs an array"); an empty array is valid and matches
// nothing ($in) / everything ($nin).
func (rc mongoRender) values(field string, vals []any) []any {
	if vals == nil {
		return []any{}
	}
	if !rc.oidFields[field] {
		return vals
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = rc.value(field, v)
	}
	return out
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
		return bson.M{rc.key(x.Field): rc.value(x.Field, x.Value)}, nil
	case figo.GteExpr:
		return bson.M{rc.key(x.Field): bson.M{"$gte": rc.value(x.Field, x.Value)}}, nil
	case figo.GtExpr:
		return bson.M{rc.key(x.Field): bson.M{"$gt": rc.value(x.Field, x.Value)}}, nil
	case figo.LtExpr:
		return bson.M{rc.key(x.Field): bson.M{"$lt": rc.value(x.Field, x.Value)}}, nil
	case figo.LteExpr:
		return bson.M{rc.key(x.Field): bson.M{"$lte": rc.value(x.Field, x.Value)}}, nil
	case figo.NeqExpr:
		return bson.M{rc.key(x.Field): bson.M{"$ne": rc.value(x.Field, x.Value)}}, nil
	case figo.LikeExpr:
		return bson.M{rc.key(x.Field): primitive.Regex{Pattern: likeToRegexPattern(x.Value)}}, nil
	case figo.RegexExpr:
		re, ok := mongoRawRegex(x.Value)
		if !ok {
			return nil, fmt.Errorf("figo: invalid regex value %v for field %q", x.Value, x.Field)
		}
		return bson.M{rc.key(x.Field): re}, nil
	case figo.ILikeExpr:
		return bson.M{rc.key(x.Field): primitive.Regex{Pattern: likeToRegexPattern(x.Value), Options: "i"}}, nil
	case figo.IsNullExpr:
		// Match both explicit null and missing (SQL IS NULL semantics), not just
		// missing as {$exists:false} would.
		return bson.M{rc.key(x.Field): nil}, nil
	case figo.NotNullExpr:
		return bson.M{rc.key(x.Field): bson.M{"$ne": nil}}, nil
	case figo.InExpr:
		return bson.M{rc.key(x.Field): bson.M{"$in": rc.values(x.Field, x.Values)}}, nil
	case figo.NotInExpr:
		return bson.M{rc.key(x.Field): bson.M{"$nin": rc.values(x.Field, x.Values)}}, nil
	case figo.BetweenExpr:
		return bson.M{rc.key(x.Field): bson.M{"$gte": rc.value(x.Field, x.Low), "$lte": rc.value(x.Field, x.High)}}, nil
	case figo.JsonPathExpr:
		return mongoJSONPath(x, rc)
	case figo.ArrayContainsExpr:
		if len(x.Values) == 0 {
			// Requiring no elements is vacuously true. Mongo's {$all: []} matches
			// NOTHING, so an empty predicate is the correct rendering.
			return bson.M{}, nil
		}
		return bson.M{rc.key(x.Field): bson.M{"$all": rc.values(x.Field, x.Values)}}, nil
	case figo.ArrayOverlapsExpr:
		// intersect-ANY: the array field shares at least one element with Values.
		return bson.M{rc.key(x.Field): bson.M{"$in": rc.values(x.Field, x.Values)}}, nil
	case figo.FullTextSearchExpr:
		// $text is only legal at the top level of a query; inside a $lookup
		// match it would produce a pipeline MongoDB rejects at runtime.
		if rc.qualifier != "" {
			return nil, fmt.Errorf("figo: full-text search ($text) is not supported inside the %q preload match for the MongoDB adapter", rc.qualifier)
		}
		txt := bson.M{"$search": x.Query}
		if x.Language != "" {
			txt["$language"] = x.Language
		}
		return bson.M{"$text": txt}, nil
	case figo.GeoDistanceExpr:
		km, err := geoDistanceKm(x.Distance, x.Unit)
		if err != nil {
			return nil, err
		}
		// $centerSphere takes [lng, lat] (longitude FIRST) and a radius in
		// radians (distance / Earth radius).
		return bson.M{rc.key(x.Field): bson.M{"$geoWithin": bson.M{
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
		parts, err := mongoOperands(x.Operands, rc)
		if err != nil {
			return nil, err
		}
		return logicalBSON("$nor", parts), nil
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
	path := rc.key(x.Field) + "." + strings.TrimPrefix(x.Path, "$.")
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
	return filter, BuildMongoFindOptions(f), nil
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

// render builds the rendering context for this adapter's configuration.
func (a MongoAdapter) render(qualifier string) mongoRender {
	return mongoRender{qualifier: qualifier, oidFields: a.objectIDFieldSet()}
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
	filter, err := buildMongoFilterFromExprs(f.GetClauses(), a.render(""))
	if err != nil {
		return nil, false
	}
	return MongoFindQuery{Filter: filter, Options: BuildMongoFindOptions(f)}, true
}
