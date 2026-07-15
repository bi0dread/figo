package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"fmt"
	"sync"
)

// figo.Query complexity limits are provided as a plugin. Historically QueryLimits
// lived on the figo.Figo instance but was never enforced; LimitsPlugin makes the
// feature real: once registered, its AfterParse hook measures every parsed
// DSL and fails AddFiltersFromString when a limit is exceeded.
//
//	lp := plugins.NewLimitsPlugin(plugins.DefaultQueryLimits())
//	f.RegisterPlugin(lp)
//	err := f.AddFiltersFromString(hugeUntrustedDSL) // errors when over a limit

// QueryLimits defines limits for query complexity. A zero (or negative) value
// disables that particular limit.
type QueryLimits struct {
	MaxNestingDepth    int // max nesting of logical operators (a flat query has depth 1)
	MaxFieldCount      int // max number of distinct fields referenced
	MaxParameterCount  int // max total number of filter values (each <in> element counts)
	MaxExpressionCount int // max total number of expression nodes
}

// DefaultQueryLimits returns the defaults figo's core used to seed
// (nesting 10, fields 50, parameters 100, expressions 200).
func DefaultQueryLimits() QueryLimits {
	return QueryLimits{
		MaxNestingDepth:    10,
		MaxFieldCount:      50,
		MaxParameterCount:  100,
		MaxExpressionCount: 200,
	}
}

// LimitsPlugin enforces QueryLimits on parsed DSL input
type LimitsPlugin struct {
	mu     sync.RWMutex
	limits QueryLimits
}

// NewLimitsPlugin creates a limits plugin enforcing the given limits
func NewLimitsPlugin(limits QueryLimits) *LimitsPlugin {
	return &LimitsPlugin{limits: limits}
}

// Name implements Plugin
func (p *LimitsPlugin) Name() string { return "figo-limits" }

// Version implements Plugin
func (p *LimitsPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *LimitsPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *LimitsPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *LimitsPlugin) AfterQuery(figo.Figo, any, interface{}) error { return nil }

// BeforeParse implements Plugin
func (p *LimitsPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// SetLimits replaces the enforced limits
func (p *LimitsPlugin) SetLimits(limits QueryLimits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = limits
}

// GetLimits returns the enforced limits
func (p *LimitsPlugin) GetLimits() QueryLimits {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.limits
}

// AfterParse measures the freshly parsed DSL against the limits. The clause
// tree is only materialized by Build, which the caller may not have run yet,
// so the DSL is built on a clone — the caller's instance is left untouched.
// Measurement happens after any registered expression filters (e.g.
// FieldsPlugin pruning), i.e. on the query that would actually run.
func (p *LimitsPlugin) AfterParse(f figo.Figo, _ string) error {
	limits := p.GetLimits()
	if limits == (QueryLimits{}) {
		return nil
	}

	c := f.Clone()
	c.Build(nil)

	m := &queryMeasure{fields: make(map[string]bool)}
	for _, e := range c.GetClauses() {
		measureExpr(e, 1, m)
	}
	for _, exprs := range c.GetPreloads() {
		for _, e := range exprs {
			measureExpr(e, 1, m)
		}
	}

	if limits.MaxNestingDepth > 0 && m.maxDepth > limits.MaxNestingDepth {
		return fmt.Errorf("query exceeds MaxNestingDepth: %d > %d", m.maxDepth, limits.MaxNestingDepth)
	}
	if limits.MaxFieldCount > 0 && len(m.fields) > limits.MaxFieldCount {
		return fmt.Errorf("query exceeds MaxFieldCount: %d > %d", len(m.fields), limits.MaxFieldCount)
	}
	if limits.MaxParameterCount > 0 && m.params > limits.MaxParameterCount {
		return fmt.Errorf("query exceeds MaxParameterCount: %d > %d", m.params, limits.MaxParameterCount)
	}
	if limits.MaxExpressionCount > 0 && m.expressions > limits.MaxExpressionCount {
		return fmt.Errorf("query exceeds MaxExpressionCount: %d > %d", m.expressions, limits.MaxExpressionCount)
	}
	return nil
}

// queryMeasure accumulates complexity metrics over an expression tree
type queryMeasure struct {
	expressions int
	params      int
	maxDepth    int
	fields      map[string]bool
}

// measureExpr walks an expression tree counting nodes, values, distinct
// fields, and the deepest level of logical nesting (a flat query is depth 1).
func measureExpr(e figo.Expr, depth int, m *queryMeasure) {
	if e == nil {
		return
	}
	if depth > m.maxDepth {
		m.maxDepth = depth
	}
	m.expressions++

	switch v := e.(type) {
	case figo.AndExpr:
		for _, op := range v.Operands {
			measureExpr(op, depth+1, m)
		}
	case figo.OrExpr:
		for _, op := range v.Operands {
			measureExpr(op, depth+1, m)
		}
	case figo.NotExpr:
		for _, op := range v.Operands {
			measureExpr(op, depth+1, m)
		}
	case figo.OrderBy:
		// Sorting isn't filter complexity; only the node itself is counted.
	default:
		if field := figo.ExprField(e); field != "" {
			m.fields[field] = true
		}
		m.params += exprParamCount(e)
	}
}

// exprParamCount returns the number of filter values a leaf node carries
func exprParamCount(e figo.Expr) int {
	switch v := e.(type) {
	case figo.EqExpr, figo.NeqExpr, figo.GtExpr, figo.GteExpr, figo.LtExpr, figo.LteExpr,
		figo.LikeExpr, figo.ILikeExpr, figo.RegexExpr, figo.JsonPathExpr,
		figo.FullTextSearchExpr, figo.CustomExpr:
		return 1
	case figo.InExpr:
		return len(v.Values)
	case figo.NotInExpr:
		return len(v.Values)
	case figo.ArrayContainsExpr:
		return len(v.Values)
	case figo.ArrayOverlapsExpr:
		return len(v.Values)
	case figo.BetweenExpr:
		return 2
	case figo.GeoDistanceExpr:
		return 3 // latitude, longitude, distance
	default:
		return 0 // IsNull/NotNull, figo.OrderBy, unknown types
	}
}
