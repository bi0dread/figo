package adapters

import (
	figo "github.com/bi0dread/figo/v4"

	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// esMaxResultWindow is Elasticsearch's default index.max_result_window. figo's
// Take <= 0 means "no limit", which ES cannot express — omitting "size" would
// silently cap the result at 10 hits, so the window is rendered explicitly
// instead, minus the offset (ES validates from + size against the window).
// Result sets beyond it need search_after/scroll.
const esMaxResultWindow = 10000

// ElasticsearchAdapter provides query building for Elasticsearch
type ElasticsearchAdapter struct{}

// GetSqlString returns the JSON representation of the Elasticsearch query.
//
// It never returns an empty body. An empty request body is Elasticsearch's
// documented "no body" case and is answered with match_all, so the (string,
// bool) contract cannot carry fail-closed intent here: figo.GetSqlString drops
// the bool and returns "" for ok=false, which turned the adapter's refusal to
// render (a load= preload, an unsupported expression, an unmarshalable value)
// into "return the whole index". A render failure therefore yields the
// fail-closed {"query":{"match_none":{}}} document with ok=true — the error
// itself stays observable through BuildElasticsearchQuery /
// GetElasticsearchQueryString / the builder's Err().
func (e ElasticsearchAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		// No instance to render: report failure, but still hand back a body
		// that matches nothing rather than one that matches everything.
		return esMatchNoneJSON(), false
	}
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		query = matchNoneQuery()
	}
	jsonBytes, mErr := json.Marshal(query)
	if mErr != nil {
		// An unmarshalable value (e.g. a NaN float): fail closed too.
		return esMatchNoneJSON(), true
	}
	return string(jsonBytes), true
}

// GetQuery returns the Elasticsearch query for the given figo instance.
//
// Like GetSqlString it fails closed to match_none rather than to a nil Query:
// figo.GetQuery returns a nil interface for ok=false, and the documented
// retrieval pattern (`f.GetQuery(nil).(adapters.ElasticsearchQueryWrapper)`)
// panics on a nil interface, so any DSL containing load= used to crash the
// caller's goroutine.
func (e ElasticsearchAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return ElasticsearchQueryWrapper{Query: matchNoneQuery()}, false
	}
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		query = matchNoneQuery()
	}
	// An unmarshalable value (e.g. a NaN float) would surface only from the
	// wrapper's GetSQL, which has no error return and yielded a silent "" —
	// which is match_all on ES. Substitute the fail-closed query instead.
	if _, err := json.Marshal(query); err != nil {
		query = matchNoneQuery()
	}
	return ElasticsearchQueryWrapper{Query: query}, true
}

// ElasticsearchQueryWrapper wraps ElasticsearchQuery to implement the figo.Query interface
type ElasticsearchQueryWrapper struct {
	Query ElasticsearchQuery
}

// IsQuery implements the figo.Query interface
func (e ElasticsearchQueryWrapper) IsQuery() {}

// GetSQL returns the JSON representation of the Elasticsearch query. An
// unmarshalable query renders as match_none, never as "" (an empty body is
// match_all on Elasticsearch).
func (e ElasticsearchQueryWrapper) GetSQL() string {
	jsonBytes, err := json.Marshal(e.Query)
	if err != nil {
		return esMatchNoneJSON()
	}
	return string(jsonBytes)
}

// GetArgs returns nil for Elasticsearch queries (no parameterized queries)
func (e ElasticsearchQueryWrapper) GetArgs() []any {
	return nil
}

// ElasticsearchQuery represents the structure of an Elasticsearch query
type ElasticsearchQuery struct {
	Query  map[string]interface{}   `json:"query"`
	Sort   []map[string]interface{} `json:"sort,omitempty"`
	From   int                      `json:"from,omitempty"`
	Size   int                      `json:"size,omitempty"`
	Source []string                 `json:"_source,omitempty"`

	// sizeSet records that Size was chosen deliberately, so that a zero Size
	// renders as "size":0 (a count-only search) instead of being dropped by
	// omitempty — Elasticsearch then applies its default of 10 hits, which is
	// the exact hazard the adapter path guards against for take <= 0.
	sizeSet bool
}

// MarshalJSON renders the query. It exists solely so an explicitly requested
// "size":0 survives; the field order matches the struct tags.
func (q ElasticsearchQuery) MarshalJSON() ([]byte, error) {
	type esQueryBody struct {
		Query  map[string]interface{}   `json:"query"`
		Sort   []map[string]interface{} `json:"sort,omitempty"`
		From   int                      `json:"from,omitempty"`
		Size   *int                     `json:"size,omitempty"`
		Source []string                 `json:"_source,omitempty"`
	}
	body := esQueryBody{Query: q.Query, Sort: q.Sort, From: q.From, Source: q.Source}
	if q.Size != 0 || q.sizeSet {
		size := q.Size
		body.Size = &size
	}
	return json.Marshal(body)
}

// BuildElasticsearchQuery converts the built figo expressions into an Elasticsearch query
func BuildElasticsearchQuery(f figo.Figo) (ElasticsearchQuery, error) {
	// ES has no join/preload analogue, so load= cannot be rendered. Every
	// other adapter honors preloads (GORM Preload, Mongo $lookup, raw JOIN);
	// failing the build keeps the file's fail-closed convention instead of
	// silently discarding them.
	if len(f.GetPreloads()) > 0 {
		// The error value is the fail-closed query, never the zero
		// ElasticsearchQuery: that marshals to {"query":null}, which ES either
		// rejects or (as a bare body) treats as match_all.
		return matchNoneQuery(), fmt.Errorf("figo: the Elasticsearch adapter does not support load= preloads")
	}

	q, err := buildElasticsearchQueryFromExprs(f.GetClauses())
	if err != nil {
		return matchNoneQuery(), err
	}
	query := ElasticsearchQuery{
		Query: q,
		// The adapter always decides a size, so render it even when it lands
		// on zero (see ElasticsearchQuery.sizeSet).
		sizeSet: true,
	}

	// Handle pagination
	p := f.GetPage()
	if p.Skip > 0 {
		query.From = p.Skip
	}
	if p.Take > 0 {
		query.Size = p.Take
	} else {
		// Take <= 0 is figo's "no limit"; render the max_result_window default
		// explicitly, otherwise ES silently returns only 10 hits. ES validates
		// from + size against the window, so the offset has to come out of the
		// size — "size": 10000 next to any "from" >= 1 was an HTTP 400 for the
		// whole search.
		if remaining := esMaxResultWindow - query.From; remaining > 0 {
			query.Size = remaining
		} else {
			query.Size = 0
		}
	}

	// Handle sorting
	sort := f.GetSort()
	if sort != nil {
		for _, c := range sort.Columns {
			// Defensively skip empty column names: {"":{"order":...}} is invalid.
			if c.Name == "" {
				continue
			}
			sortField := map[string]interface{}{
				c.Name: map[string]string{
					"order": "asc",
				},
			}
			if c.Desc {
				sortField[c.Name] = map[string]string{
					"order": "desc",
				}
			}
			query.Sort = append(query.Sort, sortField)
		}
	}

	// Handle field selection. Names go through the instance's naming func —
	// the raw/GORM/Mongo adapters all do this, so skipping it here made the
	// same AddSelectFields project different fields on ES. Keys are sorted so
	// the rendered _source list is deterministic, not map-iteration order.
	if sel := f.GetSelectFields(); len(sel) > 0 {
		for _, field := range sortedKeys(sel) {
			query.Source = append(query.Source, normalizeColumnName(f, field))
		}
	}

	return query, nil
}

// buildElasticsearchQueryFromExprs converts expressions to Elasticsearch query structure
func buildElasticsearchQueryFromExprs(exprs []figo.Expr) (map[string]interface{}, error) {
	if len(exprs) == 0 {
		return map[string]interface{}{
			"match_all": map[string]interface{}{},
		}, nil
	}

	if len(exprs) == 1 {
		q, _, _, err := esRenderExpr(exprs[0], false)
		return q, err
	}

	// Multiple expressions - combine with bool query. wantNeg is false and the
	// UNKNOWN taint is discarded: the top level is an implicit AND with no
	// negation above it, so nothing here can invert the sentinel (see esRenderExpr).
	must := []map[string]interface{}{}
	for _, expr := range exprs {
		query, _, _, err := esRenderExpr(expr, false)
		if err != nil {
			return nil, err
		}
		must = append(must, query)
	}
	return map[string]interface{}{
		"bool": map[string]interface{}{"must": must},
	}, nil
}

// esMatchNoneClause is the fail-closed predicate: a clause matching no
// document. Elasticsearch has no `1=0` equivalent and an empty bool query is
// match_all, so match_none is the only rendering that cannot widen.
//
// IMPORTANT — match_none renders BOTH of SQL's non-true values, and they are
// not interchangeable under negation:
//
//   - FALSE (e.g. `IN ()`, or the empty-OrExpr library sentinel). `NOT FALSE`
//     is TRUE, and bool.must_not of a zero-matching clause happens to match
//     every document, so the naive negation is already correct.
//   - UNKNOWN (a null comparison operand, `x NOT IN (…,NULL)`, a null BETWEEN
//     bound). `NOT UNKNOWN` is UNKNOWN, which still matches nothing — but
//     bool.must_not of match_none matches EVERY document. That inversion turned
//     `not a<nin>[null]` into a full-index dump with no error on any channel.
//
// The document cannot carry the difference, so the renderer carries it: every
// arm of buildElasticsearchQueryFromExpr returns an `unknown` flag beside the
// clause, And/Or/Not propagate it upward, and esNegate consumes it wherever a
// negation sits above it at ANY depth. Guarding only the NotExpr's immediate
// operand is not enough — in `not (a=1 or b>null)` the sentinel is a level down.
func esMatchNoneClause() map[string]interface{} {
	return map[string]interface{}{
		"match_none": map[string]interface{}{},
	}
}

// esMatchAllClause is the vacuously-true predicate.
func esMatchAllClause() map[string]interface{} {
	return map[string]interface{}{
		"match_all": map[string]interface{}{},
	}
}

// esAllOf conjoins clauses. An empty conjunction is vacuously TRUE.
func esAllOf(clauses []map[string]interface{}) map[string]interface{} {
	switch len(clauses) {
	case 0:
		return esMatchAllClause()
	case 1:
		return clauses[0]
	}
	return map[string]interface{}{
		"bool": map[string]interface{}{"must": clauses},
	}
}

// esAnyOf disjoins clauses. An empty disjunction is FALSE, and Lucene needs the
// explicit minimum_should_match: a bool whose should clauses all fail would
// otherwise still be scored as a match in a filter context.
func esAnyOf(clauses []map[string]interface{}) map[string]interface{} {
	switch len(clauses) {
	case 0:
		return esMatchNoneClause()
	case 1:
		return clauses[0]
	}
	return map[string]interface{}{
		"bool": map[string]interface{}{
			"should":               clauses,
			"minimum_should_match": 1,
		},
	}
}

// esRenderExpr renders one expression, and when wantNeg is set also renders its
// SQL-three-valued NEGATION in the SAME pass.
//
// The paired negation is what makes the UNKNOWN sentinel safe under NOT (see
// esMatchNoneClause). It has to be produced alongside the positive rendering
// rather than by a second recursive call: a NotExpr that first rendered its
// operands positively to discover the taint and then re-rendered them negated
// did 2x the work of its own subtree at every level, which is 2^depth for
// nested negations — a 30-deep `not (… and b=i)` chain took 4s and a 60-deep
// `not` chain did not finish. wantNeg keeps the extra work strictly inside NOT
// subtrees, so a query without a negation renders exactly as it always did.
//
// De Morgan holds in Kleene three-valued logic, so the structural cases just
// push the negation down; only the leaves need the FALSE/UNKNOWN distinction,
// which esNegateLeaf owns.
func esRenderExpr(expr figo.Expr, wantNeg bool) (pos, neg map[string]interface{}, unknown bool, err error) {
	switch x := expr.(type) {
	case figo.AndExpr:
		must := []map[string]interface{}{}
		var negs []map[string]interface{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			p, n, u, err := esRenderExpr(op, wantNeg)
			if err != nil {
				return nil, nil, false, err
			}
			unknown = unknown || u
			must = append(must, p)
			if wantNeg {
				negs = append(negs, n)
			}
		}
		pos = map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		}
		if wantNeg {
			// NOT(a AND b) == (NOT a) OR (NOT b)
			neg = esAnyOf(negs)
		}
		return pos, neg, unknown, nil

	case figo.OrExpr:
		should := []map[string]interface{}{}
		var negs []map[string]interface{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			p, n, u, err := esRenderExpr(op, wantNeg)
			if err != nil {
				return nil, nil, false, err
			}
			unknown = unknown || u
			should = append(should, p)
			if wantNeg {
				negs = append(negs, n)
			}
		}
		if len(should) == 0 {
			// An OrExpr with no renderable operands is figo's library-wide
			// match-NOTHING sentinel (raw/GORM render `1=0`, Mongo
			// `{"$nor":[{}]}`), and guardPluginPanic leaves exactly this shape
			// behind after a recovered plugin panic. Rendering it as an empty
			// bool inverted it: a bool query that produces no Lucene clause is
			// answered with MatchAllDocsQuery *before* minimum_should_match is
			// applied, so the guard dumped the whole index.
			//
			// This sentinel is genuinely FALSE, not UNKNOWN — `1=0` negates to
			// TRUE — so it carries no taint and negates to match_all, exactly as
			// it did before the taint existed.
			pos = esMatchNoneClause()
			if wantNeg {
				neg = esMatchAllClause()
			}
			return pos, neg, false, nil
		}
		pos = map[string]interface{}{
			"bool": map[string]interface{}{
				"should":               should,
				"minimum_should_match": 1,
			},
		}
		if wantNeg {
			// NOT(a OR b) == (NOT a) AND (NOT b)
			neg = esAllOf(negs)
		}
		return pos, neg, unknown, nil

	case figo.NotExpr:
		// figo.NotExpr means "none of the operands match" — must_not takes every
		// operand, not just the first (dropping operands widened the match). Its
		// operands are always rendered with their negations, because that is what
		// the tainted branch below needs and asking for them costs one pass.
		poss := []map[string]interface{}{}
		negs := []map[string]interface{}{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			p, n, u, err := esRenderExpr(op, true)
			if err != nil {
				return nil, nil, false, err
			}
			unknown = unknown || u
			poss = append(poss, p)
			negs = append(negs, n)
		}
		if len(poss) == 0 {
			pos = esMatchAllClause()
			if wantNeg {
				neg = esMatchNoneClause()
			}
			return pos, neg, false, nil
		}
		if unknown {
			// An operand carries SQL's UNKNOWN somewhere inside it, at ANY depth.
			// bool.must_not is an exclusion-only occurrence, so it turns the
			// operand's match_none into "match every document" — the fail-open
			// inversion `not a<nin>[null]` produced, with BuildE()==nil and
			// Err()==nil. Use the pushed-down negation instead, where each
			// UNKNOWN leaf can render match_none on the negated side too.
			//
			// figo.NotExpr is NOT(op1 OR op2 …) == NOT op1 AND NOT op2 …, so the
			// negated operands are conjoined. The taint propagates upward because
			// NOT UNKNOWN is UNKNOWN: an enclosing negation must take this path
			// too, which is what makes `not not a>null` come out right.
			pos = esAllOf(negs)
		} else {
			pos = map[string]interface{}{
				"bool": map[string]interface{}{"must_not": poss},
			}
		}
		if wantNeg {
			// NOT(NOT op1 AND NOT op2) == op1 OR op2
			neg = esAnyOf(poss)
		}
		return pos, neg, unknown, nil

	default:
		pos, unknown, err = esRenderLeaf(expr)
		if err != nil {
			return nil, nil, false, err
		}
		if wantNeg {
			neg = esNegateLeaf(expr, pos, unknown)
		}
		return pos, neg, unknown, nil
	}
}

// esNegateLeaf renders NOT(leaf) under SQL's three-valued logic, given the
// leaf's already-computed positive clause and taint.
//
// A predicate that is UNKNOWN for SOME rows and FALSE for others cannot be
// negated by wrapping its positive rendering: bool.must_not would also return
// the UNKNOWN rows. Each case below renders the rows for which the predicate is
// definitely FALSE, which is exactly where its SQL negation is TRUE.
func esNegateLeaf(expr figo.Expr, pos map[string]interface{}, unknown bool) map[string]interface{} {
	switch x := expr.(type) {
	case figo.GteExpr:
		if x.Value == nil {
			return esMatchNoneClause() // NOT UNKNOWN is UNKNOWN
		}
	case figo.GtExpr:
		if x.Value == nil {
			return esMatchNoneClause()
		}
	case figo.LtExpr:
		if x.Value == nil {
			return esMatchNoneClause()
		}
	case figo.LteExpr:
		if x.Value == nil {
			return esMatchNoneClause()
		}
	case figo.InExpr:
		if len(x.Values) == 0 {
			// `IN ()` is FALSE for every row, so its negation is TRUE.
			return esMatchAllClause()
		}
		if esHasNilValue(x.Values) {
			// `x IN (1, NULL)` is TRUE where x = 1 and UNKNOWN elsewhere, so it
			// is never FALSE: its negation matches nothing.
			return esMatchNoneClause()
		}
	case figo.NotInExpr:
		if esHasNilValue(x.Values) {
			// `x NOT IN (1, NULL)` is FALSE exactly where x IN (1) — the null
			// makes it UNKNOWN everywhere else, never TRUE. So its negation is
			// `x IN (1)`. The commit that introduced the sentinel described this
			// as "UNKNOWN for every row", which only holds for an all-null list.
			vals := esTermValues(x.Values)
			if len(vals) == 0 {
				return esMatchNoneClause()
			}
			return map[string]interface{}{
				"terms": map[string]interface{}{x.Field: vals},
			}
		}
		if len(x.Values) == 0 {
			// `NOT IN ()` is TRUE for every row; its negation is FALSE.
			return esMatchNoneClause()
		}
	case figo.BetweenExpr:
		// BETWEEN is `x >= Low AND x <= High`. With one bound null that conjunct
		// is UNKNOWN, so the whole predicate is FALSE exactly where the OTHER,
		// known bound is violated — which is where the negation is TRUE.
		switch {
		case x.Low == nil && x.High == nil:
			return esMatchNoneClause()
		case x.Low == nil:
			return map[string]interface{}{
				"range": map[string]interface{}{
					x.Field: map[string]interface{}{"gt": x.High},
				},
			}
		case x.High == nil:
			return map[string]interface{}{
				"range": map[string]interface{}{
					x.Field: map[string]interface{}{"lt": x.Low},
				},
			}
		}
	case figo.ArrayContainsExpr:
		if len(x.Values) == 0 {
			// Requiring no element is vacuously TRUE; its negation is FALSE.
			return esMatchNoneClause()
		}
		if esHasNilValue(x.Values) {
			// contains-ALL is a conjunction, so its negation is the disjunction
			// of the per-element negations; the null element contributes an
			// UNKNOWN disjunct, which a minimum_should_match:1 disjunction never
			// satisfies, so it drops out. No SQL adapter renders this expression,
			// so there is no ground truth to match — the 3VL reading is used.
			should := []map[string]interface{}{}
			for _, v := range x.Values {
				if v == nil {
					continue
				}
				should = append(should, map[string]interface{}{
					"bool": map[string]interface{}{
						"must_not": []map[string]interface{}{
							{"term": map[string]interface{}{x.Field: v}},
						},
					},
				})
			}
			return esAnyOf(should)
		}
	case figo.ArrayOverlapsExpr:
		// intersect-ANY behaves like IN: a null member can never contribute a
		// definite FALSE, so a null-bearing set is never FALSE either.
		if len(x.Values) == 0 {
			return esMatchAllClause()
		}
		if esHasNilValue(x.Values) {
			return esMatchNoneClause()
		}
	case figo.JsonPathExpr:
		if x.Value == nil {
			switch x.Op {
			case ">", ">=", "<", "<=":
				return esMatchNoneClause() // NOT UNKNOWN is UNKNOWN
			}
		}
	}

	if unknown {
		// Belt and braces: a leaf that reports UNKNOWN but has no case above must
		// fail CLOSED, never through the must_not wrapper. Reaching here means a
		// new UNKNOWN-producing arm was added to esRenderLeaf without a matching
		// negation — give it the conservative answer instead of silently matching
		// the whole index.
		return esMatchNoneClause()
	}
	// Everything else is two-valued for the rows Elasticsearch can see, so the
	// plain exclusion wrapper is the negation this adapter has always emitted.
	return map[string]interface{}{
		"bool": map[string]interface{}{
			"must_not": []map[string]interface{}{pos},
		},
	}
}

// esMatchNoneJSON is the fail-closed request body, used wherever this adapter
// would otherwise have to hand back "" (an empty ES body is match_all).
func esMatchNoneJSON() string {
	b, err := json.Marshal(matchNoneQuery())
	if err != nil {
		return `{"query":{"match_none":{}}}`
	}
	return string(b)
}

// esTermValues copies a value slice for rendering and drops nil elements.
//
// The copy keeps the emitted document independent of the figo instance —
// without it a caller editing the returned "terms" array rewrote the
// instance's InExpr.Values, so every later render, on ES and on every other
// adapter, carried the edit (raw.go copies for exactly this reason).
//
// Dropping nils is required because Elasticsearch rejects a null inside
// "terms" with a ParsingException — HTTP 400 for the whole search. For an
// any-match set this is also semantics-preserving: SQL's `IN (1, NULL)`
// matches the same rows as `IN (1)`.
func esTermValues(values []any) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		if v == nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// esHasNilValue reports whether any element of the set is nil.
func esHasNilValue(values []any) bool {
	for _, v := range values {
		if v == nil {
			return true
		}
	}
	return false
}

// esRenderLeaf converts a single non-composite expression to an Elasticsearch
// clause. It returns an error for expression types the adapter does not support,
// so an unhandled condition fails loudly rather than being silently dropped
// (which previously degraded to match_all — i.e. return the whole index).
// AndExpr/OrExpr/NotExpr are handled by esRenderExpr, which owns the negation.
//
// The second result is the UNKNOWN taint: true when the clause's SQL value is
// UNKNOWN rather than FALSE, so a negation above it must not be rendered as
// bool.must_not. See esMatchNoneClause for why the document alone cannot carry
// that difference, and esNegateLeaf for the negated rendering of each tainted
// leaf. The taint is an over-approximation by design — a false positive only
// means the exact negation path is taken, which is never wrong.
func esRenderLeaf(expr figo.Expr) (map[string]interface{}, bool, error) {
	switch x := expr.(type) {
	case figo.EqExpr:
		if x.Value == nil {
			// Canonical across adapters: a nil comparison value is the IS NULL
			// predicate ("term": null is invalid ES anyway).
			return esRenderLeaf(figo.IsNullExpr{Field: x.Field})
		}
		return map[string]interface{}{
			"term": map[string]interface{}{
				x.Field: x.Value,
			},
		}, false, nil
	case figo.GteExpr:
		if x.Value == nil {
			return esMatchNoneClause(), true, nil
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Value},
			},
		}, false, nil
	case figo.GtExpr:
		// A nil operand on the four range operators used to render
		// {"range":{"a":{"gt":null}}}; ES documents a null bound as UNBOUNDED,
		// so the predicate degenerated to "field a exists" and matched
		// essentially the whole index, while SQL's `a > NULL` is UNKNOWN and
		// matches nothing. EqExpr/NeqExpr already canonicalise nil above.
		// The clause is UNKNOWN, not FALSE: `not a>null` must still match
		// nothing, which is what the taint flag tells the NotExpr arm.
		if x.Value == nil {
			return esMatchNoneClause(), true, nil
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gt": x.Value},
			},
		}, false, nil
	case figo.LtExpr:
		if x.Value == nil {
			return esMatchNoneClause(), true, nil
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lt": x.Value},
			},
		}, false, nil
	case figo.LteExpr:
		if x.Value == nil {
			return esMatchNoneClause(), true, nil
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lte": x.Value},
			},
		}, false, nil
	case figo.NeqExpr:
		if x.Value == nil {
			// Canonical across adapters: != nil is the IS NOT NULL predicate.
			return esRenderLeaf(figo.NotNullExpr{Field: x.Field})
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"term": map[string]interface{}{x.Field: x.Value},
				},
			},
		}, false, nil
	case figo.LikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: sqlLikeToESWildcard(x.Value),
			},
		}, false, nil
	case figo.ILikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: map[string]interface{}{
					"value":            sqlLikeToESWildcard(x.Value),
					"case_insensitive": true,
				},
			},
		}, false, nil
	case figo.RegexExpr:
		return map[string]interface{}{
			"regexp": map[string]interface{}{
				x.Field: map[string]interface{}{
					"value": esRegexpContains(x.Value),
					// Lucene's regexp query defaults "flags" to ALL, which
					// enables the four operators that do not exist in PCRE,
					// Mongo's $regex or SQL REGEXP: @ (ANYSTRING), ~
					// (COMPLEMENT), & (INTERSECTION) and <n-m> (INTERVAL).
					// `email=~"@"` therefore matched every value of the field.
					// NONE disables exactly those four and leaves them literal.
					"flags": "NONE",
				},
			},
		}, false, nil
	case figo.IsNullExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"exists": map[string]interface{}{"field": x.Field},
				},
			},
		}, false, nil
	case figo.NotNullExpr:
		return map[string]interface{}{
			"exists": map[string]interface{}{"field": x.Field},
		}, false, nil
	case figo.InExpr:
		vals := esTermValues(x.Values)
		if len(vals) == 0 {
			// A nil values slice marshals to "terms": {field: null}, which
			// Elasticsearch rejects. Both remaining shapes match nothing, but
			// they differ under negation: an EMPTY set is SQL's `IN ()`, which
			// is FALSE (so `not a<in>[]` matches everything), whereas an
			// all-null set is `a IN (NULL)` — i.e. `a = NULL` — which is
			// UNKNOWN, so `not a<in>[null]` must still match nothing.
			return esMatchNoneClause(), len(x.Values) > 0, nil
		}
		// `a IN (1, NULL)` is TRUE exactly where `a IN (1)` is, so dropping the
		// null keeps the positive rendering exact — but it is never FALSE, only
		// UNKNOWN, so the negation must not become must_not[terms:[1]].
		return map[string]interface{}{
			"terms": map[string]interface{}{x.Field: vals},
		}, esHasNilValue(x.Values), nil
	case figo.NotInExpr:
		if esHasNilValue(x.Values) {
			// SQL's `x NOT IN (..., NULL)` is UNKNOWN for every row, so it can
			// never be true, and ES rejects a null inside "terms" outright.
			// match_none is both the SQL-faithful and the fail-closed answer —
			// dropping the null instead would widen the predicate.
			return esMatchNoneClause(), true, nil
		}
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every document.
			return esMatchAllClause(), false, nil
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"terms": map[string]interface{}{x.Field: esTermValues(x.Values)},
				},
			},
		}, false, nil
	case figo.BetweenExpr:
		if x.Low == nil || x.High == nil {
			// ES documents a null range bound as UNBOUNDED, so
			// {"range":{"price":{"gte":null,"lte":null}}} matched every
			// document having the field; SQL's BETWEEN with a NULL bound is
			// UNKNOWN and matches none. Fail closed, like the range operators.
			return esMatchNoneClause(), true, nil
		}
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Low, "lte": x.High},
			},
		}, false, nil
	case figo.JsonPathExpr:
		if x.Value == nil {
			switch x.Op {
			case ">", ">=", "<", "<=":
				// esJSONPath received none of the nil guards the four top-level
				// range operators grew: {"range":{"data.score":{"gt":null}}} is
				// an UNBOUNDED range on ES — every document HAVING the field —
				// where SQL's `> NULL` is UNKNOWN and matches none. Same
				// widening, same fix, same UNKNOWN taint so a negation above it
				// cannot invert the sentinel.
				return esMatchNoneClause(), true, nil
			}
		}
		q, err := esJSONPath(x)
		return q, false, err
	case figo.ArrayContainsExpr:
		// contains-ALL: every value must be present, so it needs one term per
		// value ANDed together — a single `terms` query would be any-match.
		if len(x.Values) == 0 {
			// Requiring no elements is vacuously true.
			return esMatchAllClause(), false, nil
		}
		if esHasNilValue(x.Values) {
			// "term": {field: null} is invalid ES (ParsingException). A
			// required null element cannot be contained, so fail closed
			// instead of dropping the requirement, which would widen. The
			// requirement is UNKNOWN rather than FALSE, so the negation must
			// also match nothing.
			return esMatchNoneClause(), true, nil
		}
		must := make([]map[string]interface{}, 0, len(x.Values))
		for _, v := range x.Values {
			must = append(must, map[string]interface{}{
				"term": map[string]interface{}{x.Field: v},
			})
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		}, false, nil
	case figo.ArrayOverlapsExpr:
		// intersect-ANY is exactly ES's terms query.
		vals := esTermValues(x.Values)
		if len(vals) == 0 {
			// Nothing can intersect an empty set — and a nil slice (or a null
			// element) would marshal to a "terms" ES rejects. As with InExpr,
			// an all-null set is UNKNOWN while a genuinely empty one is FALSE.
			return esMatchNoneClause(), len(x.Values) > 0, nil
		}
		return map[string]interface{}{
			"terms": map[string]interface{}{x.Field: vals},
		}, esHasNilValue(x.Values), nil
	case figo.FullTextSearchExpr:
		if x.Field == "" {
			// No target field: search across all fields via multi_match. The
			// language maps to an analyzer exactly like the field-scoped match.
			mm := map[string]interface{}{"query": x.Query}
			if x.Language != "" {
				mm["analyzer"] = x.Language
			}
			return map[string]interface{}{
				"multi_match": mm,
			}, false, nil
		}
		body := map[string]interface{}{"query": x.Query}
		if x.Language != "" {
			// The closest ES analogue of Mongo's $text $language is a
			// language analyzer on the match query.
			body["analyzer"] = x.Language
		}
		return map[string]interface{}{
			"match": map[string]interface{}{x.Field: body},
		}, false, nil
	case figo.GeoDistanceExpr:
		unit, err := esGeoUnit(x.Unit)
		if err != nil {
			return nil, false, err
		}
		if math.IsNaN(x.Distance) || math.IsInf(x.Distance, 0) {
			// A non-finite distance would render "NaNkm"/"+Infkm", which ES
			// rejects; fail the build like esGeoUnit does for bad units.
			return nil, false, fmt.Errorf("figo: non-finite geo distance %v for field %q on the Elasticsearch adapter", x.Distance, x.Field)
		}
		if x.Distance < 0 {
			// "-5km" is rejected by Elasticsearch's DistanceUnit parser, so the
			// whole search 400s while Build/BuildE/Err reported success — the
			// same class as the non-finite check above. The Mongo adapter grew
			// the identical guard and cited this adapter as its model, which is
			// how the omission was noticed.
			return nil, false, fmt.Errorf("figo: geo distance must not be negative, got %v for field %q on the Elasticsearch adapter", x.Distance, x.Field)
		}
		if math.IsNaN(x.Latitude) || math.IsInf(x.Latitude, 0) || math.IsNaN(x.Longitude) || math.IsInf(x.Longitude, 0) {
			// The coordinates go into the document as bare floats, so a
			// non-finite one is not a bad ES value but an unmarshalable one:
			// Build/BuildE/Err all reported success for a query that could
			// never be serialized.
			return nil, false, fmt.Errorf("figo: non-finite geo coordinate (lat %v, lon %v) for field %q on the Elasticsearch adapter", x.Latitude, x.Longitude, x.Field)
		}
		return map[string]interface{}{
			"geo_distance": map[string]interface{}{
				"distance": strconv.FormatFloat(x.Distance, 'f', -1, 64) + unit,
				x.Field: map[string]interface{}{
					"lat": x.Latitude,
					"lon": x.Longitude,
				},
			},
		}, false, nil
	case figo.OrderBy:
		// A stray figo.OrderBy in the clause list is a no-op predicate (ordering is
		// applied via query.Sort, not the query body). Match everything so it
		// composes harmlessly inside a bool/must — mirroring the Mongo adapter,
		// which returns an empty predicate rather than failing the whole query.
		return esMatchAllClause(), false, nil
	default:
		return nil, false, fmt.Errorf("figo: unsupported expression type %T for the Elasticsearch adapter", expr)
	}
}

// esJSONPath renders a JSON path predicate against the dotted field ES uses
// for nested object properties: path $.user.name on field "data" queries
// "data.user.name".
func esJSONPath(x figo.JsonPathExpr) (map[string]interface{}, error) {
	field := x.Field + "." + strings.TrimPrefix(x.Path, "$.")
	switch x.Op {
	case "", "=", "==", "contains":
		// ES term queries already match individual elements of array fields,
		// which covers "contains".
		return map[string]interface{}{
			"term": map[string]interface{}{field: x.Value},
		}, nil
	case "!=":
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"term": map[string]interface{}{field: x.Value},
				},
			},
		}, nil
	case ">":
		return map[string]interface{}{
			"range": map[string]interface{}{field: map[string]interface{}{"gt": x.Value}},
		}, nil
	case ">=":
		return map[string]interface{}{
			"range": map[string]interface{}{field: map[string]interface{}{"gte": x.Value}},
		}, nil
	case "<":
		return map[string]interface{}{
			"range": map[string]interface{}{field: map[string]interface{}{"lt": x.Value}},
		}, nil
	case "<=":
		return map[string]interface{}{
			"range": map[string]interface{}{field: map[string]interface{}{"lte": x.Value}},
		}, nil
	case "exists":
		return map[string]interface{}{
			"exists": map[string]interface{}{"field": field},
		}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported JSON path op %q for the Elasticsearch adapter", x.Op)
	}
}

// esGeoUnit maps a GeoDistanceExpr unit to Elasticsearch's distance suffix.
// An empty unit defaults to kilometers, matching the Mongo adapter.
func esGeoUnit(unit string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "km", "kilometers":
		return "km", nil
	case "m", "meters":
		return "m", nil
	case "mi", "miles":
		return "mi", nil
	default:
		return "", fmt.Errorf("figo: unsupported geo distance unit %q for the Elasticsearch adapter", unit)
	}
}

// sqlLikeToESWildcard translates a SQL LIKE pattern to an Elasticsearch wildcard
// pattern: '%' -> '*' (any sequence) and '_' -> '?' (single char). Literal
// '*', '?' and '\' in the value are escaped first — otherwise a user value
// containing '*' becomes a wildcard on ES only (the Mongo adapter escapes
// regex metachars, SQL treats it literally).
// esRegexpContains adapts a `=~` pattern for Elasticsearch's regexp query.
// Lucene's regexp is implicitly anchored to the whole field value, whereas the
// DSL's `=~` (like Mongo's $regex and SQL REGEXP) is an unanchored *contains*
// match. Wrapping the user pattern as `.*(<pattern>).*` restores contains
// semantics, and the parentheses keep top-level alternations (`a|b`) grouped so
// the leading/trailing `.*` don't bind to only one branch. Note: Lucene regexp
// syntax still differs from PCRE (e.g. `\d`, `^`, `$` are not supported), which
// is a backend limitation this cannot paper over.
func esRegexpContains(v any) string {
	return ".*(" + esPatternText(v) + ").*"
}

// esPatternText renders a pattern operand as the TEXT the SQL adapters match on.
//
// fmt.Sprintf("%v", []byte("a*b")) is the decimal element list "[97 42 98]", not
// the text — so the LIKE/regexp fix that exists to escape a value rendering '*'
// or '?' searched for a string no document contains whenever the value was a
// []byte, the one type its own comment names. The SQL adapters bind the byte
// slice and the server compares it as text, so decoding it here is what makes
// the adapters agree.
func esPatternText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	// A non-string value still has to be escaped and translated: returning its
	// formatting verbatim let a value rendering '*' or '?' (a fmt.Stringer)
	// become an unescaped ES wildcard matching every document, on this adapter
	// alone.
	return fmt.Sprintf("%v", v)
}

func sqlLikeToESWildcard(v any) string {
	str := esPatternText(v)
	str = strings.ReplaceAll(str, `\`, `\\`)
	str = strings.ReplaceAll(str, "*", `\*`)
	str = strings.ReplaceAll(str, "?", `\?`)
	str = strings.ReplaceAll(str, "%", "*")
	str = strings.ReplaceAll(str, "_", "?")
	return str
}

// GetElasticsearchQueryString returns the Elasticsearch query as a JSON string.
// On error the returned body is the fail-closed match_none document rather than
// "" — an empty ES request body is match_all.
func GetElasticsearchQueryString(f figo.Figo) (string, error) {
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return esMatchNoneJSON(), err
	}
	jsonBytes, err := json.MarshalIndent(query, "", "  ")
	if err != nil {
		return esMatchNoneJSON(), fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// GetElasticsearchQueryStringCompact returns the Elasticsearch query as a
// compact JSON string, failing closed to match_none exactly like
// GetElasticsearchQueryString.
func GetElasticsearchQueryStringCompact(f figo.Figo) (string, error) {
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return esMatchNoneJSON(), err
	}
	jsonBytes, err := json.Marshal(query)
	if err != nil {
		return esMatchNoneJSON(), fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ElasticsearchQueryBuilder provides a fluent interface for building Elasticsearch queries
type ElasticsearchQueryBuilder struct {
	query ElasticsearchQuery
	err   error // deferred error from FromFigo, surfaced by ToJSON/ToJSONCompact

	// pageErr is SetPagination's deferred error, kept in its own field so that
	// clearing it on a later valid SetPagination cannot also erase a FromFigo
	// error. Sharing b.err made a single bad ?from= poison the builder forever:
	// every subsequent SetPagination/AddSort/SetSource was accepted and then
	// discarded, Build kept returning match_none, and the message kept naming
	// the original bad values — the exact stickiness A7-3 removed from FromFigo,
	// reintroduced one method below it.
	pageErr error
}

// deferredErr reports the first deferred error, if any. FromFigo's error wins:
// it means the builder holds no usable query at all, whereas a pagination error
// leaves the query intact.
func (b *ElasticsearchQueryBuilder) deferredErr() error {
	if b.err != nil {
		return b.err
	}
	return b.pageErr
}

// NewElasticsearchQueryBuilder creates a new Elasticsearch query builder
func NewElasticsearchQueryBuilder() *ElasticsearchQueryBuilder {
	return &ElasticsearchQueryBuilder{
		query: ElasticsearchQuery{
			Query: map[string]interface{}{
				"match_all": map[string]interface{}{},
			},
		},
	}
}

// matchNoneQuery is the fail-closed rendering used when a build error must not
// surface a filter-stripped query: {"query":{"match_none":{}}}.
func matchNoneQuery() ElasticsearchQuery {
	return ElasticsearchQuery{
		Query: map[string]interface{}{
			"match_none": map[string]interface{}{},
		},
	}
}

// FromFigo initializes the builder with a figo instance. If the figo clauses
// contain an unsupported expression, the error is deferred and returned by
// Err/BuildE/ToJSON/ToJSONCompact (the fluent API has no error return here),
// and the stored query fails closed to match_none — previously the initial
// match_all survived, so Build() returned a match-everything query with every
// filter stripped.
//
// FromFigo replaces the whole builder state, so it also clears a previously
// deferred error on the success path: leaving that error standing pinned a
// reused builder to match_none forever while it held a perfectly good query,
// and the error it reported named an expression type no longer present.
func (b *ElasticsearchQueryBuilder) FromFigo(f figo.Figo) *ElasticsearchQueryBuilder {
	// FromFigo replaces b.query wholesale, including From/Size, so a pagination
	// error deferred against the DISCARDED pagination must go with it.
	b.pageErr = nil
	q, err := BuildElasticsearchQuery(f)
	if err != nil {
		b.err = err
		b.query = matchNoneQuery()
		return b
	}
	b.query = q
	b.err = nil
	return b
}

// Err returns the error deferred by FromFigo or SetPagination, if any.
func (b *ElasticsearchQueryBuilder) Err() error { return b.deferredErr() }

// AddSort adds a sort field to the query. An empty field name is skipped:
// {"":{"order":"asc"}} is not a valid sort key, and the adapter path already
// skips it defensively.
func (b *ElasticsearchQueryBuilder) AddSort(field string, ascending bool) *ElasticsearchQueryBuilder {
	if field == "" {
		return b
	}
	order := "asc"
	if !ascending {
		order = "desc"
	}

	sortField := map[string]interface{}{
		field: map[string]string{
			"order": order,
		},
	}

	b.query.Sort = append(b.query.Sort, sortField)
	return b
}

// SetPagination sets the from and size parameters. size == 0 is honoured
// literally (a count-only search) instead of being dropped by omitempty, which
// made ES fall back to its default of 10 hits.
//
// A negative from or size is rejected with a deferred error — Elasticsearch
// answers both with "[from]/[size] parameter cannot be negative", HTTP 400 for
// the whole search, and Build/BuildE/Err used to report success for it. The
// builder then fails closed to match_none like any other deferred error.
func (b *ElasticsearchQueryBuilder) SetPagination(from, size int) *ElasticsearchQueryBuilder {
	if from < 0 || size < 0 {
		b.pageErr = fmt.Errorf("figo: negative Elasticsearch pagination (from=%d, size=%d) is rejected by the server", from, size)
		return b
	}
	// Clearing on the valid path is the point: the deferred error describes the
	// values it rejected, and those values are now gone. Without this a caller
	// that validated its input and retried still got match_none forever, with an
	// error text naming the original from/size — and the only reset, FromFigo,
	// discards the whole query, so a pagination-only mistake was unrecoverable.
	b.pageErr = nil
	b.query.From = from
	b.query.Size = size
	b.query.sizeSet = true
	return b
}

// SetSource sets the _source fields to return. The variadic is copied so a
// caller mutating its own slice afterwards cannot rewrite the projection of a
// query it has already handed over.
func (b *ElasticsearchQueryBuilder) SetSource(fields ...string) *ElasticsearchQueryBuilder {
	b.query.Source = append([]string(nil), fields...)
	return b
}

// Build returns the final Elasticsearch query. When FromFigo deferred an
// error it returns the fail-closed match_none query — never a match-all with
// the filters stripped; use Err or BuildE to observe the error.
func (b *ElasticsearchQueryBuilder) Build() ElasticsearchQuery {
	q, _ := b.BuildE()
	return q
}

// BuildE returns the final Elasticsearch query, or the deferred FromFigo
// error alongside the fail-closed match_none query.
func (b *ElasticsearchQueryBuilder) BuildE() (ElasticsearchQuery, error) {
	if err := b.deferredErr(); err != nil {
		return matchNoneQuery(), err
	}
	return b.query, nil
}

// ToJSON returns the query as a JSON string. On a deferred error the body is
// the fail-closed match_none document, never "" — an empty Elasticsearch
// request body is match_all.
func (b *ElasticsearchQueryBuilder) ToJSON() (string, error) {
	if err := b.deferredErr(); err != nil {
		return esMatchNoneJSON(), err
	}
	jsonBytes, err := json.MarshalIndent(b.query, "", "  ")
	if err != nil {
		return esMatchNoneJSON(), fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ToJSONCompact returns the query as a compact JSON string, failing closed to
// match_none exactly like ToJSON.
func (b *ElasticsearchQueryBuilder) ToJSONCompact() (string, error) {
	if err := b.deferredErr(); err != nil {
		return esMatchNoneJSON(), err
	}
	jsonBytes, err := json.Marshal(b.query)
	if err != nil {
		return esMatchNoneJSON(), fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}
