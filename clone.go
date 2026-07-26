package figo

import "reflect"

// Clone returns a deep copy of the Figo instance.
//
// The query-building state is fully independent: filters (clauses), preloads,
// pagination, sort, the select-field set, the DSL string and naming strategy
// are all copied, so mutating the clone (AddFilter, SetPage, AddSelectFields,
// …) never affects the original and vice versa.
//
// Independence extends into a node's dynamic value: the containers figo can
// carry behind an `any` (slices, maps and []byte, nested) are copied too, so
// writing through a clone's EqExpr.Value cannot reach the original. It stops at
// values figo cannot copy without knowing the caller's type — a struct, a
// pointer, a channel — which are shared; the adapters treat those as opaque
// bind parameters.
//
// Shared collaborators are referenced, not duplicated: the adapter and the
// plugin manager are stateful services meant to be shared, and copying them
// would be unsafe or semantically wrong. If you need the clone to be isolated
// from one of these, assign a fresh one on the clone
// (e.g. clone.SetPluginManager(...), clone.SetAdapterObject(...)).
func (f *figo) Clone() Figo {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// A fresh struct literal gives the clone its own zero-value mutex; copying
	// the source struct by value would copy f.mu (a sync.RWMutex) — a vet error.
	return &figo{
		// Independent value-typed state (safe to copy directly).
		page:         f.page,
		dsl:          f.dsl,
		pageFromDSL:  f.pageFromDSL,
		sortFromDSL:  f.sortFromDSL,
		builtFromDSL: f.builtFromDSL,
		namingFunc:   f.namingFunc, // shared transformer; assumed pure

		// Deep-copied reference-typed state.
		clauses:           cloneExprs(f.clauses),
		clausesAsked:      cloneExprs(f.clausesAsked),
		preloads:          clonePreloads(f.preloads),
		selectFields:      cloneStringBoolMap(f.selectFields),
		selectFieldsAsked: cloneStringBoolMap(f.selectFieldsAsked),
		sort:              cloneOrderBy(f.sort),

		// Shared collaborators (referenced, see doc comment).
		pluginManager: f.pluginManager,
		adapterObj:    f.adapterObj,
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

// clonePtrDepth bounds how many pointer indirections cloneExpr follows. A
// value-typed Expr tree cannot be cyclic, but a hand-built pointer AST can be
// (&AndExpr whose Operands contain itself), so following pointers needs a
// bound; past it the node is shared rather than copied, as before.
const clonePtrDepth = 32

// cloneExprs deep-copies a slice of expressions.
func cloneExprs(exprs []Expr) []Expr { return cloneExprsAt(exprs, clonePtrDepth) }

func cloneExprsAt(exprs []Expr, ptrBudget int) []Expr {
	if exprs == nil {
		return nil
	}
	c := make([]Expr, len(exprs))
	for i, e := range exprs {
		c[i] = cloneExprAt(e, ptrBudget)
	}
	return c
}

// cloneExpr returns an independent copy of an expression tree. The type-switch
// binding already copies the struct; what it does NOT copy is the object behind
// a node's `any` payload, so those are deep-copied explicitly (see
// deepCopyValue) — sharing them let a write through the clone reach the
// original, contradicting Clone's contract.
func cloneExpr(e Expr) Expr { return cloneExprAt(e, clonePtrDepth) }

func cloneExprAt(e Expr, ptrBudget int) Expr {
	switch v := e.(type) {
	// Logical nodes: recurse into operands.
	case AndExpr:
		return AndExpr{Operands: cloneExprsAt(v.Operands, ptrBudget)}
	case OrExpr:
		return OrExpr{Operands: cloneExprsAt(v.Operands, ptrBudget)}
	case NotExpr:
		return NotExpr{Operands: cloneExprsAt(v.Operands, ptrBudget)}

	// Nodes carrying value slices: copy the slice AND its elements (an element
	// may itself be a slice or map).
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

	// Leaf nodes carrying a dynamic Value: deep-copy the payload's containers.
	case EqExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case NeqExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case GtExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case GteExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case LtExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case LteExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case LikeExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case ILikeExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case RegexExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case JsonPathExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case CustomExpr:
		v.Value = deepCopyValue(v.Value)
		return v
	case BetweenExpr:
		v.Low = deepCopyValue(v.Low)
		v.High = deepCopyValue(v.High)
		return v

	// Leaf nodes with only scalar/immutable fields — the switch copy is a deep
	// copy. Enumerated explicitly (rather than a default) so a future Expr type
	// with reference fields is caught here instead of silently shared.
	case IsNullExpr, NotNullExpr, FullTextSearchExpr, GeoDistanceExpr:
		return v

	default:
		// Pointer forms (&EqExpr{…}, &AndExpr{…}). No DSL path produces one and
		// no adapter renders one, but Clone still promises independence, so
		// deep-copy the pointee and hand back a FRESH pointer — sharing the
		// caller's node meant mutating the clone rewrote the original.
		if ptrBudget > 0 {
			if c, ok := clonePointerExpr(e, ptrBudget-1); ok {
				return c
			}
		}
		// Unknown type: return as-is rather than dropping it.
		return e
	}
}

// clonePointerExpr deep-copies a pointer-form node, reporting false for
// anything that is not a non-nil pointer to an Expr.
func clonePointerExpr(e Expr, ptrBudget int) (Expr, bool) {
	rv := reflect.ValueOf(e)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return nil, false
	}
	inner, ok := rv.Elem().Interface().(Expr)
	if !ok {
		return nil, false
	}
	cloned := reflect.ValueOf(cloneExprAt(inner, ptrBudget))
	if !cloned.IsValid() || cloned.Type() != rv.Type().Elem() {
		return nil, false
	}
	p := reflect.New(rv.Type().Elem())
	p.Elem().Set(cloned)
	out, ok := p.Interface().(Expr)
	return out, ok
}

// cloneAnySlice copies a slice of values, deep-copying each element's
// containers (an element behind `any` may itself be a slice or a map).
func cloneAnySlice(s []any) []any {
	if s == nil {
		return nil
	}
	c := make([]any, len(s))
	for i, v := range s {
		c[i] = deepCopyValue(v)
	}
	return c
}

// deepCopyValueDepth bounds how deep deepCopyValue descends. Containers behind
// an `any` can be cyclic (a map holding itself), so the descent needs a bound;
// past it the value is shared, which is the behaviour every value had before.
//
// The bound is on DEPTH, which on its own does not bound the WORK: a value graph
// that is a DAG rather than a tree (one sub-map fanned into several keys of its
// parent, as a hand-built config/metadata graph often is) has 2^depth distinct
// root-to-leaf PATHS over a handful of objects, and a plain recursive copy walks
// paths. A depth-20 DAG of 21 live objects was copied into 2,097,151, which made
// one GetClauses — called on every render, twice — take 350ms and one raw render
// 680ms; at the depth-32 budget, 33 objects could not be copied at all. The memo
// below is what bounds the work (the same value now costs ~17us); see
// valueCopier.
const deepCopyValueDepth = 32

// deepCopyValue returns an independent copy of the dynamic value carried by a
// node (Value / Low / High, and the elements of a Values slice).
//
// The boundary is deliberate: figo copies the CONTAINERS it can reason about —
// slices and maps of any element type (fast paths for the decoded-JSON shapes
// []any and map[string]any), and []byte — recursively. Everything else is
// copied by assignment and therefore shared: strings and numbers are immutable
// so sharing is invisible, and an arbitrary caller struct, pointer or channel
// cannot be deep-copied without knowing its type (reflect cannot set unexported
// fields, and a pointer's identity may be meaningful to the caller). Adapters
// bind such values as opaque parameters, so figo never mutates them itself.
func deepCopyValue(v any) any {
	if v == nil {
		return v
	}
	switch v.(type) {
	case string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		// The overwhelmingly common case (a scalar bind value): answer it before
		// allocating a copier, so the hot GetClauses path stays allocation-free.
		return v
	}
	c := valueCopier{}
	return c.copyAt(v, deepCopyValueDepth)
}

// valueCopier carries the per-value memo table for one deepCopyValue call.
//
// copyAt is memoised on (container identity, remaining depth budget), which makes
// it a pure memoisation of the recursion it replaces: the same container copied
// under the same budget always yields the same shape, so reusing the earlier copy
// cannot change the result — it only stops the copier from redoing it once per
// PATH. Two consequences, both wanted:
//
//   - Work is bounded by deepCopyValueDepth x (number of edges) instead of by the
//     number of paths, so a DAG-shaped value costs what its object count says it
//     should — see deepCopyValueDepth for the numbers.
//   - Sharing in the original is mirrored in the copy (one sub-map fanned into
//     two keys stays one object behind two keys) instead of being expanded into
//     independent duplicates.
//
// Independence is unaffected: every container the copier returns is freshly
// allocated, so no write through the copy can reach the caller's object. The
// depth budget still terminates a cyclic value on its own — it is keyed into the
// memo precisely so a cycle cannot be short-circuited into an aliased copy.
//
// The memo only starts once valueCopyMemoAfter containers have been copied: a
// plain tree (the shape JSON decoding produces) has no repeated node to hit, so
// paying reflection plus a map insert per node there buys nothing and measured
// 2x time and 2x allocations on a 150-container tree value.
type valueCopier struct {
	seen   map[copyRef]any
	copied int
}

// valueCopyMemoAfter is how many containers deepCopyValue copies before it starts
// memoising. It bounds the un-memoised prefix of the walk, so total work stays
// O(this + depth x edges) no matter the graph shape, while a small value — every
// value a decoded JSON body can produce — pays nothing for the memo at all.
// 64 matches the node budget the cache plugin's value walker already uses.
const valueCopyMemoAfter = 64

// copyRef identifies one memo entry: which container (type + backing pointer +
// length), copied under which remaining depth budget.
type copyRef struct {
	typ   reflect.Type
	ptr   uintptr
	len   int
	depth int
}

// containerRef reports the memo key for rv, and false when rv is not a container
// worth memoising (a nil or empty slice/map has nothing to share, and distinct
// empty slices can share a backing pointer).
func containerRef(rv reflect.Value, depth int) (copyRef, bool) {
	switch rv.Kind() {
	case reflect.Slice, reflect.Map:
		if rv.IsNil() || rv.Len() == 0 {
			return copyRef{}, false
		}
		return copyRef{typ: rv.Type(), ptr: rv.Pointer(), len: rv.Len(), depth: depth}, true
	default:
		return copyRef{}, false
	}
}

func (c *valueCopier) copyAt(v any, depth int) any {
	if v == nil || depth <= 0 {
		return v
	}
	switch v.(type) {
	case string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return v // immutable scalars: the common case, no reflection needed
	}

	rv := reflect.ValueOf(v)
	var key copyRef
	memo := false
	if c.copied >= valueCopyMemoAfter {
		if key, memo = containerRef(rv, depth); memo {
			if hit, ok := c.seen[key]; ok {
				return hit
			}
		}
	}
	c.copied++
	out := c.copyContainer(v, rv, depth)
	if memo {
		if c.seen == nil {
			c.seen = make(map[copyRef]any)
		}
		c.seen[key] = out
	}
	return out
}

func (c *valueCopier) copyContainer(v any, rv reflect.Value, depth int) any {
	switch t := v.(type) {
	case []byte:
		cp := make([]byte, len(t))
		copy(cp, t)
		return cp
	case []any:
		cp := make([]any, len(t))
		for i := range t {
			cp[i] = c.copyAt(t[i], depth-1)
		}
		return cp
	case map[string]any:
		cp := make(map[string]any, len(t))
		for k, e := range t {
			cp[k] = c.copyAt(e, depth-1)
		}
		return cp
	}

	switch rv.Kind() {
	case reflect.Slice:
		if rv.IsNil() {
			return v // preserve nil-ness; a nil slice has nothing to share
		}
		cp := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			c.setCopied(cp.Index(i), rv.Index(i), depth)
		}
		return cp.Interface()
	case reflect.Map:
		if rv.IsNil() {
			return v
		}
		cp := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			ev := reflect.New(rv.Type().Elem()).Elem()
			c.setCopied(ev, iter.Value(), depth)
			cp.SetMapIndex(iter.Key(), ev)
		}
		return cp.Interface()
	default:
		return v
	}
}

// setCopied writes a deep copy of src into dst, falling back to a plain copy if
// the copied value is not assignable (it always is for the containers above).
func (c *valueCopier) setCopied(dst, src reflect.Value, depth int) {
	copied := reflect.ValueOf(c.copyAt(src.Interface(), depth-1))
	if copied.IsValid() && copied.Type().AssignableTo(dst.Type()) {
		dst.Set(copied)
		return
	}
	dst.Set(src)
}
