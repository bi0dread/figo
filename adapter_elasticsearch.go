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
	query := BuildElasticsearchQuery(f)
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
	query := BuildElasticsearchQuery(f)
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
func BuildElasticsearchQuery(f Figo) ElasticsearchQuery {
	query := ElasticsearchQuery{
		Query: buildElasticsearchQueryFromExprs(f.GetClauses()),
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

	return query
}

// buildElasticsearchQueryFromExprs converts expressions to Elasticsearch query structure
func buildElasticsearchQueryFromExprs(exprs []Expr) map[string]interface{} {
	if len(exprs) == 0 {
		return map[string]interface{}{
			"match_all": map[string]interface{}{},
		}
	}

	if len(exprs) == 1 {
		return buildElasticsearchQueryFromExpr(exprs[0])
	}

	// Multiple expressions - combine with bool query
	boolQuery := map[string]interface{}{
		"bool": map[string]interface{}{
			"must": []map[string]interface{}{},
		},
	}

	for _, expr := range exprs {
		query := buildElasticsearchQueryFromExpr(expr)
		boolQuery["bool"].(map[string]interface{})["must"] = append(
			boolQuery["bool"].(map[string]interface{})["must"].([]map[string]interface{}),
			query,
		)
	}

	return boolQuery
}

// buildElasticsearchQueryFromExpr converts a single expression to Elasticsearch query
func buildElasticsearchQueryFromExpr(expr Expr) map[string]interface{} {
	switch x := expr.(type) {
	case EqExpr:
		return map[string]interface{}{
			"term": map[string]interface{}{
				x.Field: x.Value,
			},
		}
	case GteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{
					"gte": x.Value,
				},
			},
		}
	case GtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{
					"gt": x.Value,
				},
			},
		}
	case LtExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{
					"lt": x.Value,
				},
			},
		}
	case LteExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{
					"lte": x.Value,
				},
			},
		}
	case NeqExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"term": map[string]interface{}{
						x.Field: x.Value,
					},
				},
			},
		}
	case LikeExpr:
		// Convert SQL LIKE wildcards (%) to Elasticsearch wildcards (*)
		var wildcardValue string
		if str, ok := x.Value.(string); ok {
			wildcardValue = strings.ReplaceAll(str, "%", "*")
		} else {
			wildcardValue = fmt.Sprintf("%v", x.Value)
		}
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: wildcardValue,
			},
		}
	case ILikeExpr:
		// Convert SQL LIKE wildcards (%) to Elasticsearch wildcards (*)
		var wildcardValue string
		if str, ok := x.Value.(string); ok {
			wildcardValue = strings.ReplaceAll(str, "%", "*")
		} else {
			wildcardValue = fmt.Sprintf("%v", x.Value)
		}
		return map[string]interface{}{
			"wildcard": map[string]interface{}{
				x.Field: map[string]interface{}{
					"value":            wildcardValue,
					"case_insensitive": true,
				},
			},
		}
	case RegexExpr:
		return map[string]interface{}{
			"regexp": map[string]interface{}{
				x.Field: x.Value,
			},
		}
	case IsNullExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"exists": map[string]interface{}{
						"field": x.Field,
					},
				},
			},
		}
	case NotNullExpr:
		return map[string]interface{}{
			"exists": map[string]interface{}{
				"field": x.Field,
			},
		}
	case InExpr:
		return map[string]interface{}{
			"terms": map[string]interface{}{
				x.Field: x.Values,
			},
		}
	case NotInExpr:
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": map[string]interface{}{
					"terms": map[string]interface{}{
						x.Field: x.Values,
					},
				},
			},
		}
	case BetweenExpr:
		return map[string]interface{}{
			"range": map[string]interface{}{
				x.Field: map[string]interface{}{
					"gte": x.Low,
					"lte": x.High,
				},
			},
		}
	case AndExpr:
		boolQuery := map[string]interface{}{
			"bool": map[string]interface{}{
				"must": []map[string]interface{}{},
			},
		}
		for _, op := range x.Operands {
			if op != nil {
				query := buildElasticsearchQueryFromExpr(op)
				boolQuery["bool"].(map[string]interface{})["must"] = append(
					boolQuery["bool"].(map[string]interface{})["must"].([]map[string]interface{}),
					query,
				)
			}
		}
		return boolQuery
	case OrExpr:
		boolQuery := map[string]interface{}{
			"bool": map[string]interface{}{
				"should": []map[string]interface{}{},
			},
		}
		for _, op := range x.Operands {
			if op != nil {
				query := buildElasticsearchQueryFromExpr(op)
				boolQuery["bool"].(map[string]interface{})["should"] = append(
					boolQuery["bool"].(map[string]interface{})["should"].([]map[string]interface{}),
					query,
				)
			}
		}
		// Set minimum_should_match to 1 for OR queries
		boolQuery["bool"].(map[string]interface{})["minimum_should_match"] = 1
		return boolQuery
	case NotExpr:
		if len(x.Operands) > 0 && x.Operands[0] != nil {
			return map[string]interface{}{
				"bool": map[string]interface{}{
					"must_not": buildElasticsearchQueryFromExpr(x.Operands[0]),
				},
			}
		}
		return map[string]interface{}{
			"match_all": map[string]interface{}{},
		}
	default:
		return map[string]interface{}{
			"match_all": map[string]interface{}{},
		}
	}
}

// GetElasticsearchQueryString returns the Elasticsearch query as a JSON string
func GetElasticsearchQueryString(f Figo) (string, error) {
	query := BuildElasticsearchQuery(f)
	jsonBytes, err := json.MarshalIndent(query, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// GetElasticsearchQueryStringCompact returns the Elasticsearch query as a compact JSON string
func GetElasticsearchQueryStringCompact(f Figo) (string, error) {
	query := BuildElasticsearchQuery(f)
	jsonBytes, err := json.Marshal(query)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ElasticsearchQueryBuilder provides a fluent interface for building Elasticsearch queries
type ElasticsearchQueryBuilder struct {
	query ElasticsearchQuery
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

// FromFigo initializes the builder with a figo instance
func (b *ElasticsearchQueryBuilder) FromFigo(f Figo) *ElasticsearchQueryBuilder {
	b.query = BuildElasticsearchQuery(f)
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
	jsonBytes, err := json.MarshalIndent(b.query, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}

// ToJSONCompact returns the query as a compact JSON string
func (b *ElasticsearchQueryBuilder) ToJSONCompact() (string, error) {
	jsonBytes, err := json.Marshal(b.query)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Elasticsearch query: %w", err)
	}
	return string(jsonBytes), nil
}
