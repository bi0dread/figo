package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

// cloneForInspection clones f for the measurement/validation Builds that
// LimitsPlugin and ValidationPlugin run in their AfterParse hooks. The clone
// keeps expression filters (FieldsPlugin pruning shapes the query that will
// actually run) but drops every ClauseFinalizer: finalizers inject policy
// clauses (e.g. ScopePlugin's mandatory tenant filter) that are not part of
// the user's input, and counting or validating them wrongly rejected
// legitimate DSL that was within limits on its own.
func cloneForInspection(f figo.Figo) figo.Figo {
	c := f.Clone()
	src := f.GetPluginManager()
	if src == nil {
		return c
	}
	pm := figo.NewPluginManager()
	for _, p := range src.ListPlugins() {
		if filt, ok := p.(figo.ExprFilter); ok {
			_ = pm.RegisterPlugin(exprFilterOnly{Plugin: p, filter: filt})
		}
	}
	c.SetPluginManager(pm)
	return c
}

// exprFilterOnly re-exposes a plugin's ExprFilter while hiding any other
// optional interface it implements (ClauseFinalizer in particular): the
// embedded Plugin provides the base hooks, and the wrapper type itself
// satisfies only ExprFilter.
type exprFilterOnly struct {
	figo.Plugin
	filter figo.ExprFilter
}

func (w exprFilterOnly) FilterExpr(f figo.Figo, e figo.Expr) figo.Expr {
	return w.filter.FilterExpr(f, e)
}
