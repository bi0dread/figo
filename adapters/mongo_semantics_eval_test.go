package adapters

// A MongoDB MATCHING-SEMANTICS evaluator, used by mongo_semantic_oracle_test.go
// to answer the question eight bug hunts never asked of this adapter: does the
// emitted pipeline MEAN what the SQL means?
//
// Every Mongo check before hunt #8 compared rendered document SHAPES between
// commits. A shape differential cannot see a semantic inversion — a
// legitimate-looking document that says the opposite of what was asked. That is
// exactly how the Elasticsearch match_none/match_all sentinel inversion (hunt
// #8 D1) survived eight hunts on the sister adapter, and Mongo has the same
// sentinel shapes ($nor:[{}] for match-nothing, {} for match-everything).
//
// Two rules make this an oracle rather than a second opinion:
//
//  1. Anything the evaluator does not model is an ERROR, never a false. An
//     evaluator that silently answers "no match" for a construct it does not
//     understand reproduces the very blind spot it exists to close.
//  2. The evaluator is itself validated against MongoDB's documented rules by
//     TestMongoEvaluator_MatchesDocumentedMongoDBSemantics, with each case
//     citing the rule it pins. Do not change an expectation there without a
//     documentation reference.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// ---------------------------------------------------------------------------
// document helpers
// ---------------------------------------------------------------------------

// docEntries returns the ordered key/value pairs of a BSON document value, or
// ok=false when the value is not a document.
func docEntries(v any) ([]bson.E, bool) {
	switch t := v.(type) {
	case bson.M:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]bson.E, 0, len(keys))
		for _, k := range keys {
			out = append(out, bson.E{Key: k, Value: t[k]})
		}
		return out, true
	case map[string]any:
		return docEntries(bson.M(t))
	case bson.D:
		return []bson.E(t), true
	}
	return nil, false
}

// arrEntries returns the elements of a BSON array value.
func arrEntries(v any) ([]any, bool) {
	switch t := v.(type) {
	case bson.A:
		return []any(t), true
	case []any:
		return t, true
	case []bson.M:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	case []bson.D:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	case []float64:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

func isDocValue(v any) bool { _, ok := docEntries(v); return ok }

// isOperatorDoc reports whether a query value is an operator document
// ({"$gt": 1}) rather than a literal to compare for equality. MongoDB decides
// this on the FIRST key: a document whose first key starts with '$' is an
// operator expression.
func isOperatorDoc(v any) bool {
	entries, ok := docEntries(v)
	if !ok || len(entries) == 0 {
		return false
	}
	return strings.HasPrefix(entries[0].Key, "$")
}

// ---------------------------------------------------------------------------
// BSON comparison
// ---------------------------------------------------------------------------

// canonicalType is MongoDB's comparison/sort type bracket. $gt/$gte/$lt/$lte
// only match a value in the SAME bracket as the operand (numeric types share
// one bracket).
func canonicalType(v any) int {
	switch t := v.(type) {
	case nil:
		return 1
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return 2
	case string:
		return 3
	case bool:
		return 8
	case time.Time, primitive.DateTime:
		return 9
	case primitive.Regex:
		return 11
	case primitive.ObjectID:
		return 7
	default:
		if _, ok := docEntries(t); ok {
			return 4
		}
		if _, ok := arrEntries(t); ok {
			return 5
		}
		return -1
	}
}

func numeric(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

func timeOf(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case primitive.DateTime:
		return t.Time(), true
	}
	return time.Time{}, false
}

// bsonCompare orders two values. ok=false means "different type bracket", which
// makes every range comparison false rather than an error.
func bsonCompare(a, b any) (int, bool) {
	if canonicalType(a) != canonicalType(b) || canonicalType(a) < 0 {
		return 0, false
	}
	if a == nil {
		return 0, true // null == null, the only value in its bracket
	}
	if x, ok := numeric(a); ok {
		y, _ := numeric(b)
		switch {
		case math.IsNaN(x) || math.IsNaN(y):
			// BSON sorts NaN below every other number and it is never equal to
			// itself under a range comparison.
			if math.IsNaN(x) && math.IsNaN(y) {
				return 0, true
			}
			if math.IsNaN(x) {
				return -1, true
			}
			return 1, true
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
		return 0, true
	}
	if x, ok := a.(string); ok {
		return strings.Compare(x, b.(string)), true
	}
	if x, ok := a.(bool); ok {
		y := b.(bool)
		switch {
		case x == y:
			return 0, true
		case !x:
			return -1, true
		}
		return 1, true
	}
	if x, ok := timeOf(a); ok {
		y, _ := timeOf(b)
		switch {
		case x.Before(y):
			return -1, true
		case x.After(y):
			return 1, true
		}
		return 0, true
	}
	if x, ok := a.(primitive.ObjectID); ok {
		return strings.Compare(x.Hex(), b.(primitive.ObjectID).Hex()), true
	}
	// Documents and arrays: only equality is used by the corpus.
	if av, aok := arrEntries(a); aok {
		bv, _ := arrEntries(b)
		if len(av) != len(bv) {
			return 0, false
		}
		for i := range av {
			if c, ok := bsonCompare(av[i], bv[i]); !ok || c != 0 {
				return 0, false
			}
		}
		return 0, true
	}
	if ad, aok := docEntries(a); aok {
		bd, _ := docEntries(b)
		if len(ad) != len(bd) {
			return 0, false
		}
		for i := range ad {
			if ad[i].Key != bd[i].Key {
				return 0, false
			}
			if c, ok := bsonCompare(ad[i].Value, bd[i].Value); !ok || c != 0 {
				return 0, false
			}
		}
		return 0, true
	}
	return 0, false
}

func bsonEqual(a, b any) bool {
	c, ok := bsonCompare(a, b)
	return ok && c == 0
}

// ---------------------------------------------------------------------------
// the evaluator
// ---------------------------------------------------------------------------

type mongoEval struct {
	// collections feeds $lookup in the pipeline evaluator.
	collections map[string][]bson.M
	// vars carries $lookup `let` bindings for $expr.
	vars map[string]any
}

func (e *mongoEval) unsupported(what string, v any) error {
	return fmt.Errorf("mongo evaluator: unmodelled construct %s (%T %v) — the oracle must FAIL here, never answer false", what, v, v)
}

// pathCandidates resolves a dotted field path, applying MongoDB's implicit
// array traversal: at every segment an array is descended into element-wise for
// each element that is a document, AND a numeric segment also indexes the array
// directly (which is how {"orders.0": {$exists: true}} works). An empty result
// means the path is MISSING, which is distinct from present-and-null.
func pathCandidates(v any, segs []string) []any {
	if len(segs) == 0 {
		return []any{v}
	}
	seg := segs[0]
	var out []any
	if entries, ok := docEntries(v); ok {
		for _, e := range entries {
			if e.Key == seg {
				out = append(out, pathCandidates(e.Value, segs[1:])...)
			}
		}
		return out
	}
	if elems, ok := arrEntries(v); ok {
		if idx, err := strconv.Atoi(seg); err == nil && idx >= 0 && idx < len(elems) {
			out = append(out, pathCandidates(elems[idx], segs[1:])...)
		}
		for _, el := range elems {
			if isDocValue(el) {
				out = append(out, pathCandidates(el, segs)...)
			}
		}
	}
	return out
}

func (e *mongoEval) candidates(doc any, path string) []any {
	return pathCandidates(doc, strings.Split(path, "."))
}

// leaves expands a candidate into the values a scalar predicate may match: the
// candidate itself and, when it is an array, each of its elements. This is the
// documented "a query on an array field matches if any element matches" rule.
func leaves(c any) []any {
	out := []any{c}
	if elems, ok := arrEntries(c); ok {
		out = append(out, elems...)
	}
	return out
}

// matchFilter evaluates a whole query document against one document. Multiple
// top-level keys are an implicit AND.
func (e *mongoEval) matchFilter(filter any, doc bson.M) (bool, error) {
	entries, ok := docEntries(filter)
	if !ok {
		return false, e.unsupported("query filter is not a document", filter)
	}
	for _, en := range entries {
		m, err := e.matchTop(en.Key, en.Value, doc)
		if err != nil {
			return false, err
		}
		if !m {
			return false, nil
		}
	}
	return true, nil
}

func (e *mongoEval) matchTop(key string, val any, doc bson.M) (bool, error) {
	switch key {
	case "$and", "$or", "$nor":
		ops, ok := arrEntries(val)
		if !ok {
			return false, e.unsupported(key+" operand list", val)
		}
		if len(ops) == 0 {
			// MongoDB rejects an empty $and/$or/$nor array outright; figo never
			// emits one (logicalBSON exists to prevent it), so seeing one here
			// is a defect, not a semantic question.
			return false, fmt.Errorf("mongo evaluator: %s with an EMPTY operand array is rejected by MongoDB (error 2, BadValue)", key)
		}
		results := make([]bool, 0, len(ops))
		for _, op := range ops {
			m, err := e.matchFilter(op, doc)
			if err != nil {
				return false, err
			}
			results = append(results, m)
		}
		switch key {
		case "$and":
			for _, r := range results {
				if !r {
					return false, nil
				}
			}
			return true, nil
		case "$or":
			for _, r := range results {
				if r {
					return true, nil
				}
			}
			return false, nil
		default: // $nor
			for _, r := range results {
				if r {
					return false, nil
				}
			}
			return true, nil
		}
	case "$expr":
		v, err := e.evalExpr(val, doc)
		if err != nil {
			return false, err
		}
		return truthy(v), nil
	case "$text", "$where", "$comment", "$jsonSchema":
		return false, e.unsupported("top-level operator "+key, val)
	}
	if strings.HasPrefix(key, "$") {
		return false, e.unsupported("top-level operator "+key, val)
	}
	return e.matchField(key, val, doc)
}

// matchField evaluates a single field predicate.
func (e *mongoEval) matchField(path string, spec any, doc bson.M) (bool, error) {
	if re, ok := spec.(primitive.Regex); ok {
		return e.matchRegex(path, re, doc)
	}
	if !isOperatorDoc(spec) {
		return e.matchEq(path, spec, doc), nil
	}
	entries, _ := docEntries(spec)
	for _, en := range entries {
		m, err := e.matchOperator(path, en.Key, en.Value, doc)
		if err != nil {
			return false, err
		}
		if !m {
			return false, nil
		}
	}
	return true, nil
}

func (e *mongoEval) matchOperator(path, op string, want any, doc bson.M) (bool, error) {
	switch op {
	case "$eq":
		return e.matchEq(path, want, doc), nil
	case "$ne":
		return !e.matchEq(path, want, doc), nil
	case "$gt", "$gte", "$lt", "$lte":
		for _, c := range e.candidates(doc, path) {
			for _, leaf := range leaves(c) {
				cmp, ok := bsonCompare(leaf, want)
				if !ok {
					continue // different type bracket: no match, per MongoDB
				}
				switch op {
				case "$gt":
					if cmp > 0 {
						return true, nil
					}
				case "$gte":
					if cmp >= 0 {
						return true, nil
					}
				case "$lt":
					if cmp < 0 {
						return true, nil
					}
				case "$lte":
					if cmp <= 0 {
						return true, nil
					}
				}
			}
		}
		return false, nil
	case "$in", "$nin":
		wants, ok := arrEntries(want)
		if !ok {
			return false, fmt.Errorf("mongo evaluator: %s needs an array, got %T — MongoDB rejects this query", op, want)
		}
		hit := false
		for _, w := range wants {
			if re, isRe := w.(primitive.Regex); isRe {
				m, err := e.matchRegex(path, re, doc)
				if err != nil {
					return false, err
				}
				if m {
					hit = true
					break
				}
				continue
			}
			if e.matchEq(path, w, doc) {
				hit = true
				break
			}
		}
		if op == "$in" {
			return hit, nil
		}
		return !hit, nil
	case "$exists":
		found := len(e.candidates(doc, path)) > 0
		return found == truthy(want), nil
	case "$all":
		wants, ok := arrEntries(want)
		if !ok {
			return false, e.unsupported("$all operand", want)
		}
		if len(wants) == 0 {
			return false, nil // {$all: []} matches nothing, documented
		}
		for _, w := range wants {
			if !e.matchEq(path, w, doc) {
				return false, nil
			}
		}
		return true, nil
	case "$not":
		m, err := e.matchField(path, want, doc)
		if err != nil {
			return false, err
		}
		return !m, nil
	case "$regex":
		switch t := want.(type) {
		case primitive.Regex:
			return e.matchRegex(path, t, doc)
		case string:
			return e.matchRegex(path, primitive.Regex{Pattern: t}, doc)
		}
		return false, e.unsupported("$regex operand", want)
	case "$elemMatch":
		for _, c := range e.candidates(doc, path) {
			elems, ok := arrEntries(c)
			if !ok {
				continue
			}
			for _, el := range elems {
				sub, isDoc := el.(bson.M)
				if !isDoc {
					sub = bson.M{"__elem": el}
					m, err := e.matchFilter(rekeyElemMatch(want), sub)
					if err != nil {
						return false, err
					}
					if m {
						return true, nil
					}
					continue
				}
				m, err := e.matchFilter(want, sub)
				if err != nil {
					return false, err
				}
				if m {
					return true, nil
				}
			}
		}
		return false, nil
	case "$size":
		for _, c := range e.candidates(doc, path) {
			if elems, ok := arrEntries(c); ok {
				if bsonEqual(len(elems), want) {
					return true, nil
				}
			}
		}
		return false, nil
	case "$geoWithin", "$near", "$nearSphere", "$geoIntersects", "$mod", "$bitsAllSet",
		"$bitsAnySet", "$bitsAllClear", "$bitsAnyClear", "$type", "$options":
		return false, e.unsupported("field operator "+op, want)
	}
	return false, e.unsupported("field operator "+op, want)
}

// rekeyElemMatch turns {$gt: 1} into {__elem: {$gt: 1}} for scalar-array
// $elemMatch. figo does not emit $elemMatch; this exists so the evaluator never
// silently answers false for one.
func rekeyElemMatch(spec any) bson.M { return bson.M{"__elem": spec} }

// matchEq is MongoDB equality. Two documented rules matter:
//   - {f: null} matches a document where f is null OR f is MISSING (and an
//     array containing null).
//   - a predicate on an array field matches when any ELEMENT matches.
func (e *mongoEval) matchEq(path string, want any, doc bson.M) bool {
	cands := e.candidates(doc, path)
	if want == nil && len(cands) == 0 {
		return true
	}
	for _, c := range cands {
		for _, leaf := range leaves(c) {
			if bsonEqual(leaf, want) {
				return true
			}
		}
	}
	return false
}

func (e *mongoEval) matchRegex(path string, re primitive.Regex, doc bson.M) (bool, error) {
	flags := ""
	for _, o := range re.Options {
		switch o {
		case 'i':
			flags += "i"
		case 's':
			flags += "s"
		case 'm':
			flags += "m"
		case 'x', 'u':
			return false, fmt.Errorf("mongo evaluator: regex option %q is not modelled", string(o))
		}
	}
	pattern := re.Pattern
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("mongo evaluator: regex %q does not compile: %w", pattern, err)
	}
	for _, c := range e.candidates(doc, path) {
		for _, leaf := range leaves(c) {
			if s, ok := leaf.(string); ok && rx.MatchString(s) {
				return true, nil
			}
		}
	}
	return false, nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	}
	if n, ok := numeric(v); ok {
		return n != 0
	}
	return true
}

// evalExpr evaluates the tiny aggregation-expression subset the adapter emits:
// field paths ("$x"), let variables ("$$figoLocal") and {$eq: [a, b]}.
func (e *mongoEval) evalExpr(x any, doc bson.M) (any, error) {
	if s, ok := x.(string); ok {
		switch {
		case strings.HasPrefix(s, "$$"):
			name := strings.TrimPrefix(s, "$$")
			v, ok := e.vars[name]
			if !ok {
				return nil, fmt.Errorf("mongo evaluator: $expr references undefined variable %q", name)
			}
			return v, nil
		case strings.HasPrefix(s, "$"):
			cands := e.candidates(doc, strings.TrimPrefix(s, "$"))
			if len(cands) == 0 {
				return nil, nil // a missing field path evaluates to missing/null
			}
			return cands[0], nil
		}
		return s, nil
	}
	entries, ok := docEntries(x)
	if !ok {
		return x, nil // literal
	}
	if len(entries) != 1 {
		return nil, e.unsupported("$expr document with multiple keys", x)
	}
	en := entries[0]
	args, _ := arrEntries(en.Value)
	switch en.Key {
	case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte":
		if len(args) != 2 {
			return nil, e.unsupported("$expr "+en.Key+" arity", en.Value)
		}
		a, err := e.evalExpr(args[0], doc)
		if err != nil {
			return nil, err
		}
		b, err := e.evalExpr(args[1], doc)
		if err != nil {
			return nil, err
		}
		cmp, cmpOK := bsonCompare(a, b)
		if !cmpOK {
			return en.Key == "$ne", nil
		}
		switch en.Key {
		case "$eq":
			return cmp == 0, nil
		case "$ne":
			return cmp != 0, nil
		case "$gt":
			return cmp > 0, nil
		case "$gte":
			return cmp >= 0, nil
		case "$lt":
			return cmp < 0, nil
		default:
			return cmp <= 0, nil
		}
	}
	return nil, e.unsupported("$expr operator "+en.Key, en.Value)
}

// ---------------------------------------------------------------------------
// pipeline evaluator
// ---------------------------------------------------------------------------

// runPipeline executes the aggregation-pipeline subset the Mongo adapter emits.
// An unmodelled stage or an ILLEGAL stage specification is an error: the second
// half matters, because "the build reported success but the server rejects the
// pipeline" is its own defect class (A5-3 is exactly that).
func (e *mongoEval) runPipeline(pipe mongo.Pipeline, docs []bson.M) ([]bson.M, error) {
	cur := append([]bson.M(nil), docs...)
	for i, stage := range pipe {
		entries := []bson.E(stage)
		if len(entries) != 1 {
			return nil, fmt.Errorf("mongo evaluator: pipeline stage %d has %d keys; MongoDB requires exactly one", i, len(entries))
		}
		name, spec := entries[0].Key, entries[0].Value
		var err error
		switch name {
		case "$match":
			cur, err = e.stageMatch(cur, spec)
		case "$lookup":
			cur, err = e.stageLookup(cur, spec)
		case "$sort":
			cur, err = e.stageSort(cur, spec)
		case "$skip":
			cur, err = e.stageSkipLimit(cur, spec, true)
		case "$limit":
			cur, err = e.stageSkipLimit(cur, spec, false)
		case "$project":
			cur, err = e.stageProject(cur, spec)
		default:
			return nil, e.unsupported("pipeline stage "+name, spec)
		}
		if err != nil {
			return nil, fmt.Errorf("stage %d (%s): %w", i, name, err)
		}
	}
	return cur, nil
}

func (e *mongoEval) stageMatch(docs []bson.M, spec any) ([]bson.M, error) {
	out := make([]bson.M, 0, len(docs))
	for _, d := range docs {
		m, err := e.matchFilter(spec, d)
		if err != nil {
			return nil, err
		}
		if m {
			out = append(out, d)
		}
	}
	return out, nil
}

func (e *mongoEval) stageLookup(docs []bson.M, spec any) ([]bson.M, error) {
	entries, ok := docEntries(spec)
	if !ok {
		return nil, e.unsupported("$lookup spec", spec)
	}
	var from, as, localField, foreignField string
	var letDoc any
	var sub mongo.Pipeline
	for _, en := range entries {
		switch en.Key {
		case "from":
			from, _ = en.Value.(string)
		case "as":
			as, _ = en.Value.(string)
		case "localField":
			localField, _ = en.Value.(string)
		case "foreignField":
			foreignField, _ = en.Value.(string)
		case "let":
			letDoc = en.Value
		case "pipeline":
			switch p := en.Value.(type) {
			case mongo.Pipeline:
				sub = p
			case []bson.D:
				sub = mongo.Pipeline(p)
			default:
				return nil, e.unsupported("$lookup pipeline", en.Value)
			}
		default:
			return nil, e.unsupported("$lookup field "+en.Key, en.Value)
		}
	}
	if from == "" || as == "" {
		return nil, fmt.Errorf("$lookup needs a non-empty from and as (got from=%q as=%q); MongoDB rejects an empty FieldPath (error 40352)", from, as)
	}
	foreign, ok := e.collections[from]
	if !ok {
		return nil, fmt.Errorf("mongo evaluator: no collection %q seeded for $lookup", from)
	}
	out := make([]bson.M, 0, len(docs))
	for _, d := range docs {
		joined := []any{}
		if letDoc != nil {
			letEntries, ok := docEntries(letDoc)
			if !ok {
				return nil, e.unsupported("$lookup let", letDoc)
			}
			vars := map[string]any{}
			for _, le := range letEntries {
				v, err := e.evalExpr(le.Value, d)
				if err != nil {
					return nil, err
				}
				vars[le.Key] = v
			}
			child := &mongoEval{collections: e.collections, vars: vars}
			res, err := child.runPipeline(sub, foreign)
			if err != nil {
				return nil, err
			}
			for _, r := range res {
				joined = append(joined, r)
			}
		} else {
			// The localField/foreignField form: MongoDB matches ELEMENT-WISE
			// when either key holds an array.
			locals := leaves(firstOrNil(e.candidates(d, localField)))
			for _, fd := range foreign {
				remotes := leaves(firstOrNil(e.candidates(fd, foreignField)))
				if anyEqual(locals, remotes) {
					joined = append(joined, fd)
				}
			}
		}
		nd := cloneDoc(d)
		nd[as] = bson.A(joined)
		out = append(out, nd)
	}
	return out, nil
}

func firstOrNil(vals []any) any {
	if len(vals) == 0 {
		return nil
	}
	return vals[0]
}

func anyEqual(a, b []any) bool {
	for _, x := range a {
		for _, y := range b {
			if bsonEqual(x, y) {
				return true
			}
		}
	}
	return false
}

func cloneDoc(d bson.M) bson.M {
	out := make(bson.M, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	return out
}

func (e *mongoEval) stageSort(docs []bson.M, spec any) ([]bson.M, error) {
	entries, ok := docEntries(spec)
	if !ok {
		return nil, e.unsupported("$sort spec", spec)
	}
	out := append([]bson.M(nil), docs...)
	sort.SliceStable(out, func(i, j int) bool {
		for _, en := range entries {
			dir, _ := numeric(en.Value)
			a := firstOrNil(e.candidates(out[i], en.Key))
			b := firstOrNil(e.candidates(out[j], en.Key))
			cmp, ok := bsonCompare(a, b)
			if !ok {
				cmp = canonicalType(a) - canonicalType(b)
			}
			if cmp != 0 {
				if dir < 0 {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
	return out, nil
}

func (e *mongoEval) stageSkipLimit(docs []bson.M, spec any, skip bool) ([]bson.M, error) {
	n, ok := numeric(spec)
	if !ok {
		return nil, e.unsupported("$skip/$limit operand", spec)
	}
	if skip {
		if int(n) >= len(docs) {
			return nil, nil
		}
		return docs[int(n):], nil
	}
	if n <= 0 {
		return nil, fmt.Errorf("$limit must be positive; MongoDB rejects %v (error 15958)", spec)
	}
	if int(n) >= len(docs) {
		return docs, nil
	}
	return docs[:int(n)], nil
}

// stageProject models an INCLUSION projection, including MongoDB's path
// collision rule: a specification that names both a path and a prefix of that
// path is rejected outright ("Invalid $project :: caused by :: Path collision",
// errors 31249/31250, legacy 40176) and the WHOLE aggregate fails.
func (e *mongoEval) stageProject(docs []bson.M, spec any) ([]bson.M, error) {
	entries, ok := docEntries(spec)
	if !ok {
		return nil, e.unsupported("$project spec", spec)
	}
	paths := make([]string, 0, len(entries))
	for _, en := range entries {
		if n, ok := numeric(en.Value); !ok || n == 0 {
			return nil, e.unsupported("$project value (only inclusion is modelled)", en.Value)
		}
		paths = append(paths, en.Key)
	}
	for i := range paths {
		for j := range paths {
			if i == j {
				continue
			}
			if strings.HasPrefix(paths[j], paths[i]+".") {
				return nil, fmt.Errorf("Invalid $project :: caused by :: Path collision at %s (specification also names its prefix %s) — MongoDB rejects the whole aggregate (error 31249/31250)", paths[j], paths[i])
			}
		}
	}
	out := make([]bson.M, 0, len(docs))
	for _, d := range docs {
		nd := bson.M{}
		if _, ok := d["_id"]; ok {
			nd["_id"] = d["_id"]
		}
		for _, p := range paths {
			projectInto(nd, d, strings.Split(p, "."))
		}
		out = append(out, nd)
	}
	return out, nil
}

// projectInto copies the value at segs from src into dst, creating the
// intermediate documents (and mapping over arrays) the way $project does.
func projectInto(dst, src bson.M, segs []string) {
	if len(segs) == 0 {
		return
	}
	head := segs[0]
	v, ok := src[head]
	if !ok {
		return
	}
	if len(segs) == 1 {
		dst[head] = v
		return
	}
	if elems, ok := arrEntries(v); ok {
		outArr := bson.A{}
		for _, el := range elems {
			if sub, ok := el.(bson.M); ok {
				nd := bson.M{}
				projectInto(nd, sub, segs[1:])
				outArr = append(outArr, nd)
			}
		}
		dst[head] = outArr
		return
	}
	if sub, ok := v.(bson.M); ok {
		nd, _ := dst[head].(bson.M)
		if nd == nil {
			nd = bson.M{}
		}
		projectInto(nd, sub, segs[1:])
		dst[head] = nd
	}
}
