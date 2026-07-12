package figo

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ElasticsearchAdapter provides query building for Elasticsearch
type ElasticsearchAdapter struct{}

// GetSqlString returns the JSON representation of the Elasticsearch query
func (e ElasticsearchAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
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
func (e ElasticsearchAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	if f == nil {
		return nil, false
	}
	query, err := BuildElasticsearchQuery(f)
	if err != nil {
		return nil, false
	}
	return ElasticsearchQueryWrapper{Query: query}, true
}

// ElasticsearchQueryWrapper wraps ElasticsearchQuery to implement the Query interface
type ElasticsearchQueryWrapper struct {
	Query ElasticsearchQuery
}

// isQuery implements the Query interface
func (e ElasticsearchQueryWrapper) isQuery() {}

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
func BuildElasticsearchQuery(f Figo) (ElasticsearchQuery, error) {
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
	}

	// Handle sorting
	sort := f.GetSort()
	if sort != nil {
		for _, c := range sort.Columns {
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

	// Handle field selection
	if len(f.GetSelectFields()) > 0 {
		for field := range f.GetSelectFields() {
			query.Source = append(query.Source, field)
		}
	}

	return query, nil
}

// buildElasticsearchQueryFromExprs converts expressions to Elasticsearch query structure
func buildElasticsearchQueryFromExprs(exprs []Expr) (map[string]interface{}, error) {
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
func buildElasticsearchQueryFromExpr(expr Expr) (map[string]interface{}, error) {
	switch x := expr.(type) {
	case EqExpr:
		return map[string]interface{}{
			"term": map[string]interface{}{
				x.Field: x.Value,
			},
		}, nil
	case GteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Value},
			},
		}, nil
	case GtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gt": x.Value},
			},
		}, nil
	case LtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lt": x.Value},
			},
		}, nil
	case LteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"lte": x.Value},
			},
		}, nil
	case NeqExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"term": map[string]interface{}{x.Field: x.Value},
				},
			},
		}, nil
	case LikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: sqlLikeToESWildcard(x.Value),
			},
		}, nil
	case ILikeExpr:
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: map[string]interface{}{
					"value":            sqlLikeToESWildcard(x.Value),
					"case_insensitive": true,
				},
			},
		}, nil
	case RegexExpr:
		return map[string]interface{}{
			"regexp": map[string]interface{}{
				x.Field: esRegexpContains(x.Value),
			},
		}, nil
	case IsNullExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"exists": map[string]interface{}{"field": x.Field},
				},
			},
		}, nil
	case NotNullExpr:
		return map[string]interface{}{
			"exists": map[string]interface{}{"field": x.Field},
		}, nil
	case InExpr:
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
	case NotInExpr:
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
	case BetweenExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{"gte": x.Low, "lte": x.High},
			},
		}, nil
	case AndExpr:
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
	case OrExpr:
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
	case NotExpr:
		// NotExpr means "none of the operands match" — must_not takes every
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
	case OrderBy:
		// A stray OrderBy in the clause list is a no-op predicate (ordering is
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
func GetElasticsearchQueryString(f Figo) (string, error) {
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
func GetElasticsearchQueryStringCompact(f Figo) (string, error) {
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

// FromFigo initializes the builder with a figo instance. If the figo clauses
// contain an unsupported expression, the error is deferred and returned by
// ToJSON/ToJSONCompact (the fluent API has no error return here).
func (b *ElasticsearchQueryBuilder) FromFigo(f Figo) *ElasticsearchQueryBuilder {
	q, err := BuildElasticsearchQuery(f)
	if err != nil {
		b.err = err
		return b
	}
	b.query = q
	return b
}

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

// Build returns the final Elasticsearch query
func (b *ElasticsearchQueryBuilder) Build() ElasticsearchQuery {
	return b.query
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
