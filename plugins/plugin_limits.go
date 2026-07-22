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
//
// MaxNestingDepth counts logical nesting as a user reads the query: a bare
// condition or one flat connector chain ("a=1 and b=2 and ... z=9") is depth
// 1 — regardless of how the parser nested its binary nodes internally — and
// each embedded group of a DIFFERENT connector adds a level, so
// "a=1 and (b=2 or c=3)" is depth 2.
type QueryLimits struct {
	MaxNestingDepth    int // max logical nesting (a flat query has depth 1)
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
func (p *LimitsPlugin) AfterQuery(figo.Figo, any, any) error { return nil }

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
// FieldsPlugin pruning) but WITHOUT clause finalizers (see
// cloneForInspection), i.e. on the user's query as it would actually run —
// not on policy clauses a ScopePlugin injects.
func (p *LimitsPlugin) AfterParse(f figo.Figo, _ string) error {
	limits := p.GetLimits()
	if limits == (QueryLimits{}) {
		return nil
	}

	c := cloneForInspection(f)
	c.Build(nil)

	m := &queryMeasure{fields: make(map[string]bool)}
	for _, e := range c.GetClauses() {
		measureExpr(e, m)
		if d := logicalDepth(e); d > m.maxDepth {
			m.maxDepth = d
		}
	}
	for _, exprs := range c.GetPreloads() {
		for _, e := range exprs {
			measureExpr(e, m)
			if d := logicalDepth(e); d > m.maxDepth {
				m.maxDepth = d
			}
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

// measureExpr walks an expression tree counting nodes, values, and distinct
// fields. Depth is measured separately by logicalDepth.
func measureExpr(e figo.Expr, m *queryMeasure) {
	if e == nil {
		return
	}
	m.expressions++

	switch v := e.(type) {
	case figo.AndExpr:
		for _, op := range v.Operands {
			measureExpr(op, m)
		}
	case figo.OrExpr:
		for _, op := range v.Operands {
			measureExpr(op, m)
		}
	case figo.NotExpr:
		for _, op := range v.Operands {
			measureExpr(op, m)
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

// logicalDepth measures nesting the way QueryLimits documents it: leaves and
// flat single-connector chains are depth 1; each embedded group of a
// different connector adds a level. Counting raw tree levels (the previous
// behavior) inflated flat chains — the parser builds "a and b and ... and k"
// as left-nested binary AndExprs, so an 11-term FLAT query measured depth 11
// and tripped the default MaxNestingDepth of 10.
func logicalDepth(e figo.Expr) int {
	if d := exprDepth(e, figo.Operation("")); d > 1 {
		return d
	}
	return 1
}

// exprDepth returns how many logical levels e contributes under a parent
// connector. Operands using the SAME connector as their parent continue the
// parent's level (chain flattening); a different connector opens a new one.
func exprDepth(e figo.Expr, parentOp figo.Operation) int {
	var op figo.Operation
	var operands []figo.Expr
	switch v := e.(type) {
	case figo.AndExpr:
		op, operands = figo.OperationAnd, v.Operands
	case figo.OrExpr:
		op, operands = figo.OperationOr, v.Operands
	case figo.NotExpr:
		op, operands = figo.OperationNot, v.Operands
	default:
		return 0
	}

	maxChild := 0
	for _, o := range operands {
		if o == nil {
			continue
		}
		if d := exprDepth(o, op); d > maxChild {
			maxChild = d
		}
	}
	if op == parentOp {
		return maxChild
	}
	return maxChild + 1
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
