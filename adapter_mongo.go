package figo

import (
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
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

// BuildMongoFilter converts the built figo expressions into a MongoDB filter
func BuildMongoFilter(f Figo) bson.M {
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
func BuildMongoAggregatePipeline(f Figo, joins map[string]MongoJoin) mongo.Pipeline {
	pipeline := mongo.Pipeline{}

	// root filter
	rootMatch := BuildMongoFilter(f)
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
			m := buildMongoFilterFromExprsQualified(exprs, j.As)
			if len(m) > 0 {
				pipeline = append(pipeline, bson.D{{Key: "$match", Value: m}})
			}
		}
	}

	return pipeline
}

// Helper: convert a list of Expr to a bson.M filter
func buildMongoFilterFromExprs(exprs []Expr) bson.M {
	if len(exprs) == 0 {
		return bson.M{}
	}
	// If multiple top-level expressions exist, combine with $and
	var parts []bson.M
	for _, e := range exprs {
		if e == nil {
			continue
		}
		if m := mongoExpr(e); len(m) > 0 {
			parts = append(parts, m)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return bson.M{"$and": parts}
}

func buildMongoFilterFromExprsQualified(exprs []Expr, qualifier string) bson.M {
	if len(exprs) == 0 {
		return bson.M{}
	}
	var parts []bson.M
	for _, e := range exprs {
		if e == nil {
			continue
		}
		if m := mongoExprQualified(e, qualifier); len(m) > 0 {
			parts = append(parts, m)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return bson.M{"$and": parts}
}

// Translate Expr to MongoDB filter fragment
func mongoExpr(e Expr) bson.M {
	switch x := e.(type) {
	case EqExpr:
		return bson.M{x.Field: x.Value}
	case GteExpr:
		return bson.M{x.Field: bson.M{"$gte": x.Value}}
	case GtExpr:
		return bson.M{x.Field: bson.M{"$gt": x.Value}}
	case LtExpr:
		return bson.M{x.Field: bson.M{"$lt": x.Value}}
	case LteExpr:
		return bson.M{x.Field: bson.M{"$lte": x.Value}}
	case NeqExpr:
		return bson.M{x.Field: bson.M{"$ne": x.Value}}
	case LikeExpr:
		return bson.M{x.Field: bson.M{"$regex": likeToRegex(x.Value)}}
	case RegexExpr:
		// Accept raw regex string or *regexp.Regexp
		switch v := x.Value.(type) {
		case *regexp.Regexp:
			return bson.M{x.Field: bson.M{"$regex": v}}
		case string:
			// do not escape: treat as raw pattern
			if compiled, err := regexp.Compile(v); err == nil {
				return bson.M{x.Field: bson.M{"$regex": compiled}}
			}
			// If regex compilation fails, return empty filter
			return bson.M{}
		default:
			return bson.M{}
		}
	case ILikeExpr:
		return bson.M{x.Field: bson.M{"$regex": likeToRegex(x.Value), "$options": "i"}}
	case IsNullExpr:
		return bson.M{x.Field: bson.M{"$exists": false}}
	case NotNullExpr:
		return bson.M{x.Field: bson.M{"$exists": true}}
	case InExpr:
		return bson.M{x.Field: bson.M{"$in": x.Values}}
	case NotInExpr:
		return bson.M{x.Field: bson.M{"$nin": x.Values}}
	case BetweenExpr:
		return bson.M{x.Field: bson.M{"$gte": x.Low, "$lte": x.High}}
	case AndExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExpr(op); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$and": parts}
	case OrExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExpr(op); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$or": parts}
	case NotExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExpr(op); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$nor": parts}
	case OrderBy:
		return bson.M{}
	default:
		return bson.M{}
	}
}

func mongoExprQualified(e Expr, qualifier string) bson.M {
	q := func(field string) string { return qualifier + "." + field }
	switch x := e.(type) {
	case EqExpr:
		return bson.M{q(x.Field): x.Value}
	case GteExpr:
		return bson.M{q(x.Field): bson.M{"$gte": x.Value}}
	case GtExpr:
		return bson.M{q(x.Field): bson.M{"$gt": x.Value}}
	case LtExpr:
		return bson.M{q(x.Field): bson.M{"$lt": x.Value}}
	case LteExpr:
		return bson.M{q(x.Field): bson.M{"$lte": x.Value}}
	case NeqExpr:
		return bson.M{q(x.Field): bson.M{"$ne": x.Value}}
	case LikeExpr:
		return bson.M{q(x.Field): bson.M{"$regex": likeToRegex(x.Value)}}
	case RegexExpr:
		switch v := x.Value.(type) {
		case *regexp.Regexp:
			return bson.M{q(x.Field): bson.M{"$regex": v}}
		case string:
			if compiled, err := regexp.Compile(v); err == nil {
				return bson.M{q(x.Field): bson.M{"$regex": compiled}}
			}
			// If regex compilation fails, return empty filter
			return bson.M{}
		default:
			return bson.M{}
		}
	case ILikeExpr:
		return bson.M{q(x.Field): bson.M{"$regex": likeToRegex(x.Value), "$options": "i"}}
	case IsNullExpr:
		return bson.M{q(x.Field): bson.M{"$exists": false}}
	case NotNullExpr:
		return bson.M{q(x.Field): bson.M{"$exists": true}}
	case InExpr:
		return bson.M{q(x.Field): bson.M{"$in": x.Values}}
	case NotInExpr:
		return bson.M{q(x.Field): bson.M{"$nin": x.Values}}
	case BetweenExpr:
		return bson.M{q(x.Field): bson.M{"$gte": x.Low, "$lte": x.High}}
	case AndExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExprQualified(op, qualifier); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$and": parts}
	case OrExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExprQualified(op, qualifier); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$or": parts}
	case NotExpr:
		var parts []bson.M
		for _, op := range x.Operands {
			if m := mongoExprQualified(op, qualifier); len(m) > 0 {
				parts = append(parts, m)
			}
		}
		return bson.M{"$nor": parts}
	case OrderBy:
		return bson.M{}
	default:
		return bson.M{}
	}
}

func likeToRegex(v any) *regexp.Regexp {
	// Convert SQL-like %pattern% into a Go regex. Non-string inputs are stringified.
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	// strip quotes if present
	s = strings.Trim(s, "\"")
	// escape regex meta and then convert % wildcards to .*
	pattern := regexp.QuoteMeta(s)
	pattern = strings.ReplaceAll(pattern, "%", ".*")
	return regexp.MustCompile(pattern)
}

// AdapterMongoGetFind returns filter and find options for a simple Find operation
func AdapterMongoGetFind(f Figo) (bson.M, *options.FindOptions) {
	return BuildMongoFilter(f), BuildMongoFindOptions(f)
}

// AdapterMongoGetAggregate returns an aggregation pipeline and options based on joins
func AdapterMongoGetAggregate(f Figo, joins map[string]MongoJoin) (mongo.Pipeline, *options.AggregateOptions) {
	pipeline := BuildMongoAggregatePipeline(f, joins)
	opts := options.Aggregate()
	return pipeline, opts
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
		pipe, opts := AdapterMongoGetAggregate(f, joins)
		return MongoAggregateQuery{Pipeline: pipe, Options: opts}, true
	}
	filter, opts := AdapterMongoGetFind(f)
	return MongoFindQuery{Filter: filter, Options: opts}, true
}
