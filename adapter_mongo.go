package figo

import (
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

// BuildMongoFilter converts the built figo expressions into a MongoDB filter.
// It returns an error if a clause uses an expression type the MongoDB adapter
// does not support (rather than silently dropping the condition).
func BuildMongoFilter(f Figo) (bson.M, error) {
	return buildMongoFilterFromExprs(f.GetClauses())
}

// BuildMongoFindOptions produces FindOptions including sort, limit/skip
func BuildMongoFindOptions(f Figo) *options.FindOptions {
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
	return opts
}

// BuildMongoAggregatePipeline builds an aggregation pipeline for preloads using $lookup.
// The pipeline begins with an optional $match for root filters, followed by $lookup for each preload,
// and optional $match stages to filter the joined arrays.
func BuildMongoAggregatePipeline(f Figo, joins map[string]MongoJoin) (mongo.Pipeline, error) {
	pipeline := mongo.Pipeline{}

	// root filter
	rootMatch, err := BuildMongoFilter(f)
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
			m, err := buildMongoFilterFromExprsQualified(exprs, j.As)
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

// Helper: convert a list of Expr to a bson.M filter
func buildMongoFilterFromExprs(exprs []Expr) (bson.M, error) {
	if len(exprs) == 0 {
		return bson.M{}, nil
	}
	// If multiple top-level expressions exist, combine with $and
	parts, err := mongoOperands(exprs, "")
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

func buildMongoFilterFromExprsQualified(exprs []Expr, qualifier string) (bson.M, error) {
	if len(exprs) == 0 {
		return bson.M{}, nil
	}
	parts, err := mongoOperands(exprs, qualifier)
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

// mongoOperands renders a set of operands, propagating the first error. An empty
// qualifier selects the unqualified mongoExpr; a non-empty one qualifies fields.
func mongoOperands(ops []Expr, qualifier string) ([]bson.M, error) {
	var parts []bson.M
	for _, op := range ops {
		if op == nil {
			continue
		}
		var (
			m   bson.M
			err error
		)
		if qualifier == "" {
			m, err = mongoExpr(op)
		} else {
			m, err = mongoExprQualified(op, qualifier)
		}
		if err != nil {
			return nil, err
		}
		if len(m) > 0 {
			parts = append(parts, m)
		}
	}
	return parts, nil
}

// Translate Expr to MongoDB filter fragment
func mongoExpr(e Expr) (bson.M, error) {
	switch x := e.(type) {
	case EqExpr:
		return bson.M{x.Field: x.Value}, nil
	case GteExpr:
		return bson.M{x.Field: bson.M{"$gte": x.Value}}, nil
	case GtExpr:
		return bson.M{x.Field: bson.M{"$gt": x.Value}}, nil
	case LtExpr:
		return bson.M{x.Field: bson.M{"$lt": x.Value}}, nil
	case LteExpr:
		return bson.M{x.Field: bson.M{"$lte": x.Value}}, nil
	case NeqExpr:
		return bson.M{x.Field: bson.M{"$ne": x.Value}}, nil
	case LikeExpr:
		return bson.M{x.Field: primitive.Regex{Pattern: likeToRegexPattern(x.Value)}}, nil
	case RegexExpr:
		re, ok := mongoRawRegex(x.Value)
		if !ok {
			return nil, fmt.Errorf("figo: invalid regex value %v for field %q", x.Value, x.Field)
		}
		return bson.M{x.Field: re}, nil
	case ILikeExpr:
		return bson.M{x.Field: primitive.Regex{Pattern: likeToRegexPattern(x.Value), Options: "i"}}, nil
	case IsNullExpr:
		return bson.M{x.Field: bson.M{"$exists": false}}, nil
	case NotNullExpr:
		return bson.M{x.Field: bson.M{"$exists": true}}, nil
	case InExpr:
		return bson.M{x.Field: bson.M{"$in": x.Values}}, nil
	case NotInExpr:
		return bson.M{x.Field: bson.M{"$nin": x.Values}}, nil
	case BetweenExpr:
		return bson.M{x.Field: bson.M{"$gte": x.Low, "$lte": x.High}}, nil
	case AndExpr:
		parts, err := mongoOperands(x.Operands, "")
		if err != nil {
			return nil, err
		}
		return bson.M{"$and": parts}, nil
	case OrExpr:
		parts, err := mongoOperands(x.Operands, "")
		if err != nil {
			return nil, err
		}
		return bson.M{"$or": parts}, nil
	case NotExpr:
		parts, err := mongoOperands(x.Operands, "")
		if err != nil {
			return nil, err
		}
		return bson.M{"$nor": parts}, nil
	case OrderBy:
		return bson.M{}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported expression type %T for the MongoDB adapter", e)
	}
}

func mongoExprQualified(e Expr, qualifier string) (bson.M, error) {
	q := func(field string) string { return qualifier + "." + field }
	switch x := e.(type) {
	case EqExpr:
		return bson.M{q(x.Field): x.Value}, nil
	case GteExpr:
		return bson.M{q(x.Field): bson.M{"$gte": x.Value}}, nil
	case GtExpr:
		return bson.M{q(x.Field): bson.M{"$gt": x.Value}}, nil
	case LtExpr:
		return bson.M{q(x.Field): bson.M{"$lt": x.Value}}, nil
	case LteExpr:
		return bson.M{q(x.Field): bson.M{"$lte": x.Value}}, nil
	case NeqExpr:
		return bson.M{q(x.Field): bson.M{"$ne": x.Value}}, nil
	case LikeExpr:
		return bson.M{q(x.Field): primitive.Regex{Pattern: likeToRegexPattern(x.Value)}}, nil
	case RegexExpr:
		re, ok := mongoRawRegex(x.Value)
		if !ok {
			return nil, fmt.Errorf("figo: invalid regex value %v for field %q", x.Value, x.Field)
		}
		return bson.M{q(x.Field): re}, nil
	case ILikeExpr:
		return bson.M{q(x.Field): primitive.Regex{Pattern: likeToRegexPattern(x.Value), Options: "i"}}, nil
	case IsNullExpr:
		return bson.M{q(x.Field): bson.M{"$exists": false}}, nil
	case NotNullExpr:
		return bson.M{q(x.Field): bson.M{"$exists": true}}, nil
	case InExpr:
		return bson.M{q(x.Field): bson.M{"$in": x.Values}}, nil
	case NotInExpr:
		return bson.M{q(x.Field): bson.M{"$nin": x.Values}}, nil
	case BetweenExpr:
		return bson.M{q(x.Field): bson.M{"$gte": x.Low, "$lte": x.High}}, nil
	case AndExpr:
		parts, err := mongoOperands(x.Operands, qualifier)
		if err != nil {
			return nil, err
		}
		return bson.M{"$and": parts}, nil
	case OrExpr:
		parts, err := mongoOperands(x.Operands, qualifier)
		if err != nil {
			return nil, err
		}
		return bson.M{"$or": parts}, nil
	case NotExpr:
		parts, err := mongoOperands(x.Operands, qualifier)
		if err != nil {
			return nil, err
		}
		return bson.M{"$nor": parts}, nil
	case OrderBy:
		return bson.M{}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported expression type %T for the MongoDB adapter", e)
	}
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
	s = strings.Trim(s, "\"")
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

// mongoRawRegex builds a BSON regex value from a RegexExpr's raw pattern. The
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
func AdapterMongoGetFind(f Figo) (bson.M, *options.FindOptions, error) {
	filter, err := BuildMongoFilter(f)
	if err != nil {
		return nil, nil, err
	}
	return filter, BuildMongoFindOptions(f), nil
}

// AdapterMongoGetAggregate returns an aggregation pipeline and options based on joins
func AdapterMongoGetAggregate(f Figo, joins map[string]MongoJoin) (mongo.Pipeline, *options.AggregateOptions, error) {
	pipeline, err := BuildMongoAggregatePipeline(f, joins)
	if err != nil {
		return nil, nil, err
	}
	opts := options.Aggregate()
	return pipeline, opts, nil
}

// MongoAdapter exists for interface parity; it doesn't render SQL strings.
type MongoAdapter struct{}

func (MongoAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
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

func (MongoFindQuery) isQuery() {}

type MongoAggregateQuery struct {
	Pipeline mongo.Pipeline
	Options  *options.AggregateOptions
}

func (MongoAggregateQuery) isQuery() {}

func (MongoAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
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
		pipe, opts, err := AdapterMongoGetAggregate(f, joins)
		if err != nil {
			// An unsupported expression must not be silently dropped — fail the
			// query build rather than returning a partial pipeline.
			return nil, false
		}
		return MongoAggregateQuery{Pipeline: pipe, Options: opts}, true
	}
	filter, opts, err := AdapterMongoGetFind(f)
	if err != nil {
		return nil, false
	}
	return MongoFindQuery{Filter: filter, Options: opts}, true
}
