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
// silently cap the result at 10 hits, so the window default is rendered
// explicitly instead. Result sets beyond it need search_after/scroll.
const esMaxResultWindow = 10000

// ElasticsearchAdapter provides query building for Elasticsearch
type ElasticsearchAdapter struct{}

// GetSqlString returns the JSON representation of the Elasticsearch query
func (e ElasticsearchAdapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	if f == nil {
		return "", false
	}
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return "", false
	}
	jsonBytes, err := json.Marshal(query)
	if err != nil {
		return "", false
	}
	return string(jsonBytes), true
}

// GetQuery returns the Elasticsearch query for the given figo instance
func (e ElasticsearchAdapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	if f == nil {
		return nil, false
	}
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return nil, false
	}
	// An unmarshalable value (e.g. a NaN float) would surface only from the
	// wrapper's GetSQL, which has no error return and yielded a silent "".
	// Fail here instead, like GetSqlString does.
	if _, err := json.Marshal(query); err != nil {
		return nil, false
	}
	return ElasticsearchQueryWrapper{Query: query}, true
}

// ElasticsearchQueryWrapper wraps ElasticsearchQuery to implement the figo.Query interface
type ElasticsearchQueryWrapper struct {
	Query ElasticsearchQuery
}

// IsQuery implements the figo.Query interface
func (e ElasticsearchQueryWrapper) IsQuery() {}

// GetSQL returns the JSON representation of the Elasticsearch query
func (e ElasticsearchQueryWrapper) GetSQL() string {
	jsonBytes, _ := json.Marshal(e.Query)
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
}

// BuildElasticsearchQuery converts the built figo expressions into an Elasticsearch query
func BuildElasticsearchQuery(f figo.Figo) (ElasticsearchQuery, error) {
	// ES has no join/preload analogue, so load= cannot be rendered. Every
	// other adapter honors preloads (GORM Preload, Mongo $lookup, raw JOIN);
	// failing the build keeps the file's fail-closed convention instead of
	// silently discarding them.
	if len(f.GetPreloads()) > 0 {
		return ElasticsearchQuery{}, fmt.Errorf("figo: the Elasticsearch adapter does not support load= preloads")
	}

	q, err := buildElasticsearchQueryFromExprs(f.GetClauses())
	if err != nil {
		return ElasticsearchQuery{}, err
	}
	query := ElasticsearchQuery{
		Query: q,
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
		// explicitly, otherwise ES silently returns only 10 hits.
		query.Size = esMaxResultWindow
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
		return buildElasticsearchQueryFromExpr(exprs[0])
	}

	// Multiple expressions - combine with bool query
	must := []map[string]interface{}{}
	for _, expr := range exprs {
		query, err := buildElasticsearchQueryFromExpr(expr)
		if err != nil {
			return nil, err
		}
		must = append(must, query)
	}
	return map[string]interface{}{
		"bool": map[string]interface{}{"must": must},
	}, nil
}

// buildElasticsearchQueryFromExpr converts a single expression to an Elasticsearch
// query. It returns an error for expression types the adapter does not support,
// so an unhandled condition fails loudly rather than being silently dropped
// (which previously degraded to match_all — i.e. return the whole index).
func buildElasticsearchQueryFromExpr(expr figo.Expr) (map[string]interface{}, error) {
	switch x := expr.(type) {
	case figo.EqExpr:
		if x.Value == nil {
			// Canonical across adapters: a nil comparison value is the IS NULL
			// predicate ("term": null is invalid ES anyway).
			return buildElasticsearchQueryFromExpr(figo.IsNullExpr{Field: x.Field})
		}
		return map[string]interface{}{
			"term": map[string]interface{}{
				x.Field: x.Value,
			},
		}, nil
	case figo.GteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Value},
			},
		}, nil
	case figo.GtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gt": x.Value},
			},
		}, nil
	case figo.LtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lt": x.Value},
			},
		}, nil
	case figo.LteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lte": x.Value},
			},
		}, nil
	case figo.NeqExpr:
		if x.Value == nil {
			// Canonical across adapters: != nil is the IS NOT NULL predicate.
			return buildElasticsearchQueryFromExpr(figo.NotNullExpr{Field: x.Field})
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"term": map[string]interface{}{x.Field: x.Value},
				},
			},
		}, nil
	case figo.LikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: sqlLikeToESWildcard(x.Value),
			},
		}, nil
	case figo.ILikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: map[string]interface{}{
					"value":            sqlLikeToESWildcard(x.Value),
					"case_insensitive": true,
				},
			},
		}, nil
	case figo.RegexExpr:
		return map[string]interface{}{
			"regexp": map[string]interface{}{
				x.Field: esRegexpContains(x.Value),
			},
		}, nil
	case figo.IsNullExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"exists": map[string]interface{}{"field": x.Field},
				},
			},
		}, nil
	case figo.NotNullExpr:
		return map[string]interface{}{
			"exists": map[string]interface{}{"field": x.Field},
		}, nil
	case figo.InExpr:
		if len(x.Values) == 0 {
			// A nil values slice marshals to "terms": {field: null}, which
			// Elasticsearch rejects. An empty IN set matches nothing.
			return map[string]interface{}{
				"match_none": map[string]interface{}{},
			}, nil
		}
		return map[string]interface{}{
			"terms": map[string]interface{}{x.Field: x.Values},
		}, nil
	case figo.NotInExpr:
		if len(x.Values) == 0 {
			// "NOT IN (empty set)" is true for every document.
			return map[string]interface{}{
				"match_all": map[string]interface{}{},
			}, nil
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"terms": map[string]interface{}{x.Field: x.Values},
				},
			},
		}, nil
	case figo.BetweenExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Low, "lte": x.High},
			},
		}, nil
	case figo.JsonPathExpr:
		return esJSONPath(x)
	case figo.ArrayContainsExpr:
		// contains-ALL: every value must be present, so it needs one term per
		// value ANDed together — a single `terms` query would be any-match.
		if len(x.Values) == 0 {
			// Requiring no elements is vacuously true.
			return map[string]interface{}{
				"match_all": map[string]interface{}{},
			}, nil
		}
		must := make([]map[string]interface{}, 0, len(x.Values))
		for _, v := range x.Values {
			must = append(must, map[string]interface{}{
				"term": map[string]interface{}{x.Field: v},
			})
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		}, nil
	case figo.ArrayOverlapsExpr:
		// intersect-ANY is exactly ES's terms query.
		if len(x.Values) == 0 {
			// Nothing can intersect an empty set — and a nil slice would
			// marshal to "terms": {field: null}, which ES rejects.
			return map[string]interface{}{
				"match_none": map[string]interface{}{},
			}, nil
		}
		return map[string]interface{}{
			"terms": map[string]interface{}{x.Field: x.Values},
		}, nil
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
			}, nil
		}
		body := map[string]interface{}{"query": x.Query}
		if x.Language != "" {
			// The closest ES analogue of Mongo's $text $language is a
			// language analyzer on the match query.
			body["analyzer"] = x.Language
		}
		return map[string]interface{}{
			"match": map[string]interface{}{x.Field: body},
		}, nil
	case figo.GeoDistanceExpr:
		unit, err := esGeoUnit(x.Unit)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(x.Distance) || math.IsInf(x.Distance, 0) {
			// A non-finite distance would render "NaNkm"/"+Infkm", which ES
			// rejects; fail the build like esGeoUnit does for bad units.
			return nil, fmt.Errorf("figo: non-finite geo distance %v for field %q on the Elasticsearch adapter", x.Distance, x.Field)
		}
		return map[string]interface{}{
			"geo_distance": map[string]interface{}{
				"distance": strconv.FormatFloat(x.Distance, 'f', -1, 64) + unit,
				x.Field: map[string]interface{}{
					"lat": x.Latitude,
					"lon": x.Longitude,
				},
			},
		}, nil
	case figo.AndExpr:
		must := []map[string]interface{}{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			q, err := buildElasticsearchQueryFromExpr(op)
			if err != nil {
				return nil, err
			}
			must = append(must, q)
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		}, nil
	case figo.OrExpr:
		should := []map[string]interface{}{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			q, err := buildElasticsearchQueryFromExpr(op)
			if err != nil {
				return nil, err
			}
			should = append(should, q)
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"should":               should,
				"minimum_should_match": 1,
			},
		}, nil
	case figo.NotExpr:
		// figo.NotExpr means "none of the operands match" — must_not takes every
		// operand, not just the first (dropping operands widened the match).
		musts := []map[string]interface{}{}
		for _, op := range x.Operands {
			if op == nil {
				continue
			}
			inner, err := buildElasticsearchQueryFromExpr(op)
			if err != nil {
				return nil, err
			}
			musts = append(musts, inner)
		}
		if len(musts) == 0 {
			return map[string]interface{}{
				"match_all": map[string]interface{}{},
			}, nil
		}
		return map[string]interface{}{
			"bool": map[string]interface{}{"must_not": musts},
		}, nil
	case figo.OrderBy:
		// A stray figo.OrderBy in the clause list is a no-op predicate (ordering is
		// applied via query.Sort, not the query body). Match everything so it
		// composes harmlessly inside a bool/must — mirroring the Mongo adapter,
		// which returns an empty predicate rather than failing the whole query.
		return map[string]interface{}{
			"match_all": map[string]interface{}{},
		}, nil
	default:
		return nil, fmt.Errorf("figo: unsupported expression type %T for the Elasticsearch adapter", expr)
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
	str, ok := v.(string)
	if !ok {
		str = fmt.Sprintf("%v", v)
	}
	return ".*(" + str + ").*"
}

func sqlLikeToESWildcard(v any) string {
	str, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	str = strings.ReplaceAll(str, `\`, `\\`)
	str = strings.ReplaceAll(str, "*", `\*`)
	str = strings.ReplaceAll(str, "?", `\?`)
	str = strings.ReplaceAll(str, "%", "*")
	str = strings.ReplaceAll(str, "_", "?")
	return str
}

// GetElasticsearchQueryString returns the Elasticsearch query as a JSON string
func GetElasticsearchQueryString(f figo.Figo) (string, error) {
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return "", err
	}
	jsonBytes, err := json.MarshalIndent(query, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// GetElasticsearchQueryStringCompact returns the Elasticsearch query as a compact JSON string
func GetElasticsearchQueryStringCompact(f figo.Figo) (string, error) {
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return "", err
	}
	jsonBytes, err := json.Marshal(query)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ElasticsearchQueryBuilder provides a fluent interface for building Elasticsearch queries
type ElasticsearchQueryBuilder struct {
	query ElasticsearchQuery
	err   error // deferred error from FromFigo, surfaced by ToJSON/ToJSONCompact
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
func (b *ElasticsearchQueryBuilder) FromFigo(f figo.Figo) *ElasticsearchQueryBuilder {
	q, err := BuildElasticsearchQuery(f)
	if err != nil {
		b.err = err
		b.query = matchNoneQuery()
		return b
	}
	b.query = q
	return b
}

// Err returns the error deferred by FromFigo, if any.
func (b *ElasticsearchQueryBuilder) Err() error { return b.err }

// AddSort adds a sort field to the query
func (b *ElasticsearchQueryBuilder) AddSort(field string, ascending bool) *ElasticsearchQueryBuilder {
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

// SetPagination sets the from and size parameters
func (b *ElasticsearchQueryBuilder) SetPagination(from, size int) *ElasticsearchQueryBuilder {
	b.query.From = from
	b.query.Size = size
	return b
}

// SetSource sets the _source fields to return
func (b *ElasticsearchQueryBuilder) SetSource(fields ...string) *ElasticsearchQueryBuilder {
	b.query.Source = fields
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
	if b.err != nil {
		return matchNoneQuery(), b.err
	}
	return b.query, nil
}

// ToJSON returns the query as a JSON string
func (b *ElasticsearchQueryBuilder) ToJSON() (string, error) {
	if b.err != nil {
		return "", b.err
	}
	jsonBytes, err := json.MarshalIndent(b.query, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ToJSONCompact returns the query as a compact JSON string
func (b *ElasticsearchQueryBuilder) ToJSONCompact() (string, error) {
	if b.err != nil {
		return "", b.err
	}
	jsonBytes, err := json.Marshal(b.query)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}
