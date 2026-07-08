package figo

// Clone returns a deep copy of the Figo instance.
//
// The query-building state is fully independent: filters (clauses), preloads,
// pagination, sort, the ignore/select/allowed field sets, query limits, the DSL
// string, naming strategy and cache config are all copied, so mutating the clone
// (AddFilter, SetPage, AddIgnoreFields, EnableFieldWhitelist, …) never affects
// the original and vice versa.
//
// Shared collaborators are referenced, not duplicated: the adapter, cache,
// performance monitor, plugin manager and validation manager are stateful
// services (several hold their own locks or background goroutines) meant to be
// shared, and copying them would be unsafe or semantically wrong. If you need
// the clone to be isolated from one of these, assign a fresh one on the clone
// (e.g. clone.SetCache(...), clone.SetPerformanceMonitor(...)).
func (f *figo) Clone() Figo {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// A fresh struct literal gives the clone its own zero-value mutex; copying
	// the source struct by value would copy f.mu (a sync.RWMutex) — a vet error.
	return &figo{
		// Independent value-typed state (safe to copy directly).
		page:           f.page,
		fieldWhitelist: f.fieldWhitelist,
		queryLimits:    f.queryLimits,
		cacheConfig:    f.cacheConfig,
		dsl:            f.dsl,
		namingStrategy: f.namingStrategy,
		namingFunc:     f.namingFunc, // shared transformer; assumed pure

		// Deep-copied reference-typed state.
		clauses:       cloneExprs(f.clauses),
		preloads:      clonePreloads(f.preloads),
		ignoreFields:  cloneStringBoolMap(f.ignoreFields),
		selectFields:  cloneStringBoolMap(f.selectFields),
		allowedFields: cloneStringBoolMap(f.allowedFields),
		sort:          cloneOrderBy(f.sort),

		// Shared collaborators (referenced, see doc comment).
		cache:             f.cache,
		monitor:           f.monitor,
		pluginManager:     f.pluginManager,
		validationManager: f.validationManager,
		adapterObj:        f.adapterObj,
	}
}

// cloneStringBoolMap returns an independent copy of a set-like map. It always
// returns a non-nil map (matching New()) so the clone's Add*Fields methods,
// which assume an initialized map, are safe to call.
func cloneStringBoolMap(m map[string]bool) map[string]bool {
	c := make(map[string]bool, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// cloneOrderBy deep-copies a sort spec. nil (no sort) is preserved as nil.
func cloneOrderBy(o *OrderBy) *OrderBy {
	if o == nil {
		return nil
	}
	cols := make([]OrderByColumn, len(o.Columns)) // OrderByColumn is a pure value struct
	copy(cols, o.Columns)
	return &OrderBy{Columns: cols}
}

// clonePreloads deep-copies the preload map and each of its expression slices.
func clonePreloads(m map[string][]Expr) map[string][]Expr {
	c := make(map[string][]Expr, len(m))
	for k, v := range m {
		c[k] = cloneExprs(v)
	}
	return c
}

// cloneExprs deep-copies a slice of expressions.
func cloneExprs(exprs []Expr) []Expr {
	if exprs == nil {
		return nil
	}
	c := make([]Expr, len(exprs))
	for i, e := range exprs {
		c[i] = cloneExpr(e)
	}
	return c
}

// cloneExpr returns an independent copy of an expression tree. Leaf comparison
// nodes whose only reference-typed field is a scalar Value are copied by value
// (the type-switch binding is already a copy); nodes that carry slices copy
// those slices explicitly.
func cloneExpr(e Expr) Expr {
	switch v := e.(type) {
	// Logical nodes: recurse into operands.
	case AndExpr:
		return AndExpr{Operands: cloneExprs(v.Operands)}
	case OrExpr:
		return OrExpr{Operands: cloneExprs(v.Operands)}
	case NotExpr:
		return NotExpr{Operands: cloneExprs(v.Operands)}

	// Nodes carrying value slices: copy the slice (elements are scalars).
	case InExpr:
		return InExpr{Field: v.Field, Values: cloneAnySlice(v.Values)}
	case NotInExpr:
		return NotInExpr{Field: v.Field, Values: cloneAnySlice(v.Values)}
	case ArrayContainsExpr:
		return ArrayContainsExpr{Field: v.Field, Values: cloneAnySlice(v.Values)}
	case ArrayOverlapsExpr:
		return ArrayOverlapsExpr{Field: v.Field, Values: cloneAnySlice(v.Values)}

	// OrderBy is also an Expr; deep-copy its columns.
	case OrderBy:
		return *cloneOrderBy(&v)

	// Leaf nodes with only scalar/immutable fields — the switch copy is a deep
	// copy. Enumerated explicitly (rather than a default) so a future Expr type
	// with reference fields is caught here instead of silently shared.
	case EqExpr, NeqExpr, GtExpr, GteExpr, LtExpr, LteExpr,
		LikeExpr, ILikeExpr, RegexExpr, BetweenExpr,
		IsNullExpr, NotNullExpr, JsonPathExpr, FullTextSearchExpr,
		GeoDistanceExpr, CustomExpr:
		return v

	default:
		// Unknown type: return as-is rather than dropping it.
		return e
	}
}

// cloneAnySlice copies a slice of scalar values.
func cloneAnySlice(s []any) []any {
	if s == nil {
		return nil
	}
	c := make([]any, len(s))
	copy(c, s)
	return c
}
