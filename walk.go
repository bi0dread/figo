package figo

import "reflect"

// Walk traverses the expression tree rooted at e, calling visit once per node.
//
// visit receives a POINTER to each node (*EqExpr, *AndExpr, …) — figo's Expr
// types are value types, so a pointer is what lets the callback mutate a node in
// place (e.g. rename a field). The possibly-updated tree is returned; assign it
// back to wherever the root was held. To walk and mutate the clauses of a Figo
// instance directly, use (Figo).Walk, which handles the write-back for you.
//
//	newAst := figo.Walk(ast, func(n figo.Expr) {
//	    if c, ok := n.(*figo.EqExpr); ok && c.Field == "first_name" {
//	        c.Field = "users.first_name"
//	    }
//	})
func Walk(e Expr, visit func(Expr)) Expr {
	if e == nil {
		return nil
	}
	switch v := e.(type) {
	// Logical nodes: recurse into operands first, then visit the node itself.
	// Operands are rebuilt into a fresh slice — writing into v.Operands would
	// mutate the backing array shared with the snapshots GetClauses/Explain
	// hand out, racing with concurrent readers.
	case AndExpr:
		v.Operands = walkOperands(v.Operands, visit)
		visit(&v)
		return v
	case OrExpr:
		v.Operands = walkOperands(v.Operands, visit)
		visit(&v)
		return v
	case NotExpr:
		v.Operands = walkOperands(v.Operands, visit)
		visit(&v)
		return v

	// Leaf nodes: visit via a pointer to an addressable copy, then write it back.
	// Leaves carrying slices or a dynamic Value copy them first: the struct copy
	// shares the slice BACKING ARRAY (and the object behind Value) with the
	// snapshots GetClauses/Explain hand out, so a visitor writing v.Values[i] or
	// v.Value.(map[string]any)["k"] would mutate them in place (and race with
	// readers). The visitor's edits still land: the copy is what is written back.
	case EqExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case NeqExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case GtExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case GteExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case LtExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case LteExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case LikeExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case ILikeExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case RegexExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case InExpr:
		v.Values = cloneAnySlice(v.Values)
		visit(&v)
		return v
	case NotInExpr:
		v.Values = cloneAnySlice(v.Values)
		visit(&v)
		return v
	case BetweenExpr:
		v.Low = deepCopyValue(v.Low)
		v.High = deepCopyValue(v.High)
		visit(&v)
		return v
	case IsNullExpr:
		visit(&v)
		return v
	case NotNullExpr:
		visit(&v)
		return v
	case JsonPathExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case ArrayContainsExpr:
		v.Values = cloneAnySlice(v.Values)
		visit(&v)
		return v
	case ArrayOverlapsExpr:
		v.Values = cloneAnySlice(v.Values)
		visit(&v)
		return v
	case FullTextSearchExpr:
		visit(&v)
		return v
	case GeoDistanceExpr:
		visit(&v)
		return v
	case CustomExpr:
		v.Value = deepCopyValue(v.Value)
		visit(&v)
		return v
	case OrderBy:
		v = *cloneOrderBy(&v)
		visit(&v)
		return v

	default:
		// Unknown or already-pointer type: pass through unchanged.
		visit(e)
		return e
	}
}

func walkOperands(operands []Expr, visit func(Expr)) []Expr {
	rebuilt := make([]Expr, len(operands))
	for i := range operands {
		rebuilt[i] = Walk(operands[i], visit)
	}
	return rebuilt
}

// Walk traverses every clause (and preload) of this instance, invoking visit on
// each node, and writes any mutations back into the instance. Call it after
// Build (or after adding filters).
//
//	f.Walk(func(n figo.Expr) {
//	    if field, ok := figo.NodeField(n); ok && field == "first_name" {
//	        figo.SetNodeField(n, "users.first_name")
//	    }
//	})
//
// A name written through Walk is FINAL — Walk edits the parsed tree directly and
// therefore does not re-enter the checks AddFilter and the DSL parser run on the
// way in. Specifically, and by design (Walk exists to say exactly which column to
// address, so re-processing the name would defeat it):
//
//   - The naming strategy (SetNamingFunc) is NOT applied. AddFilter and the
//     parser convert on the way in, so with a snake-case strategy
//     AddFilter(EqExpr{Field: "userName"}) addresses `user_name` while a Walk
//     that sets "userName" addresses `userName`. Pass names already in the
//     target store's spelling.
//   - Plugin field policy is NOT re-applied. A FieldsPlugin ignore list prunes
//     conditions as they arrive; a condition Walk retargets at an ignored column
//     afterwards is rendered. Only the visitor's own code limits what it may
//     name.
//   - A subsequent Build(...) re-parses the DSL and discards Walk's edits, which
//     is why the docs say to walk after Build.
//
// What still holds is render-time validation: the raw adapter rejects a name that
// quoting cannot make executable (control bytes, empty dot segments) whichever
// route set it, so Walk cannot produce a broken statement that way.
func (f *figo) Walk(visit func(Expr)) {
	// Snapshot under the lock, run the user's visitor OUTSIDE it (a visitor
	// calling back into figo methods must not deadlock), then merge the
	// rebuilt trees in under the lock again.
	f.mu.Lock()
	// origClauses/origPreloads hold the snapshotted slices themselves (not just
	// their elements) so the identity checks in mergeWalked cannot be fooled by
	// a recycled backing array.
	origClauses := f.clauses
	clauses := make([]Expr, len(origClauses))
	copy(clauses, origClauses)
	origPreloads := make(map[string][]Expr, len(f.preloads))
	preloads := make(map[string][]Expr, len(f.preloads))
	for k, exprs := range f.preloads {
		origPreloads[k] = exprs
		cp := make([]Expr, len(exprs))
		copy(cp, exprs)
		preloads[k] = cp
	}
	f.mu.Unlock()

	for i := range clauses {
		clauses[i] = Walk(clauses[i], visit)
	}
	for k, exprs := range preloads {
		for i := range exprs {
			exprs[i] = Walk(exprs[i], visit)
		}
		preloads[k] = exprs
	}

	// Write back WITHOUT clobbering state that changed while the visitor ran.
	// This used to overwrite f.clauses/f.preloads wholesale, which made Walk a
	// non-atomic read-modify-write: a filter or preload added during the walk —
	// by the visitor itself (the docs bless calling figo methods from a visitor)
	// or by another goroutine — was silently discarded, and clauses CLEARED
	// during the walk were resurrected, leaving GetDSL() and the clause state
	// permanently disagreeing. Newer state now wins over the stale snapshot.
	f.mu.Lock()
	f.clauses = mergeWalked(f.clauses, origClauses, clauses)
	for k, walked := range preloads {
		live, ok := f.preloads[k]
		if !ok {
			continue // relation removed while the visitor ran: do not resurrect it
		}
		f.preloads[k] = mergeWalked(live, origPreloads[k], walked)
	}
	f.mu.Unlock()
}

// mergeWalked installs the walked trees over the live slice slot by slot. A slot
// is overwritten only while it still holds the value that was snapshotted;
// anything the state changed underneath the walk keeps the LIVE value, and
// elements appended meanwhile are preserved. The result is a fresh slice
// whenever anything is installed, so the backing array behind the snapshots
// GetClauses/Explain hand out is never written in place.
func mergeWalked(live, orig, walked []Expr) []Expr {
	// Fast path: nothing touched the slice while the visitor ran.
	if sameExprSlice(live, orig) {
		return walked
	}
	n := len(walked)
	if len(live) < n {
		n = len(live)
	}
	var out []Expr
	for i := 0; i < n; i++ {
		// Equality, not identity: an in-place append keeps the backing array but
		// a reallocating one does not, and either way a slot whose value still
		// matches the snapshot is one the walk is entitled to rewrite.
		if !reflect.DeepEqual(live[i], orig[i]) {
			continue
		}
		if out == nil {
			out = make([]Expr, len(live))
			copy(out, live)
		}
		out[i] = walked[i]
	}
	if out == nil {
		return live
	}
	return out
}

// sameExprSlice reports whether two slices are the same slice: same length and
// same backing array. Expr values are not comparable in general (logical nodes
// hold slices), so identity is what can be checked cheaply here.
func sameExprSlice(a, b []Expr) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
}

// NodeField returns the field name a node filters on, and whether it has one.
// It reads the pointer nodes that Walk passes to its visitor, so any field-
// bearing node type can be handled uniformly (logical nodes report false).
func NodeField(e Expr) (string, bool) {
	switch v := e.(type) {
	case *EqExpr:
		return v.Field, true
	case *NeqExpr:
		return v.Field, true
	case *GtExpr:
		return v.Field, true
	case *GteExpr:
		return v.Field, true
	case *LtExpr:
		return v.Field, true
	case *LteExpr:
		return v.Field, true
	case *LikeExpr:
		return v.Field, true
	case *ILikeExpr:
		return v.Field, true
	case *RegexExpr:
		return v.Field, true
	case *InExpr:
		return v.Field, true
	case *NotInExpr:
		return v.Field, true
	case *BetweenExpr:
		return v.Field, true
	case *IsNullExpr:
		return v.Field, true
	case *NotNullExpr:
		return v.Field, true
	case *JsonPathExpr:
		return v.Field, true
	case *ArrayContainsExpr:
		return v.Field, true
	case *ArrayOverlapsExpr:
		return v.Field, true
	case *FullTextSearchExpr:
		return v.Field, true
	case *GeoDistanceExpr:
		return v.Field, true
	case *CustomExpr:
		return v.Field, true
	default:
		return "", false
	}
}

// SetNodeField sets the field name on a node and reports whether it did. It
// requires the pointer form that Walk hands its visitor (a value can't be
// mutated). Logical nodes and OrderBy return false.
//
// The name is used verbatim: it is not passed through the naming strategy and it
// is not re-checked against plugin field policy — see (Figo).Walk.
func SetNodeField(e Expr, field string) bool {
	switch v := e.(type) {
	case *EqExpr:
		v.Field = field
	case *NeqExpr:
		v.Field = field
	case *GtExpr:
		v.Field = field
	case *GteExpr:
		v.Field = field
	case *LtExpr:
		v.Field = field
	case *LteExpr:
		v.Field = field
	case *LikeExpr:
		v.Field = field
	case *ILikeExpr:
		v.Field = field
	case *RegexExpr:
		v.Field = field
	case *InExpr:
		v.Field = field
	case *NotInExpr:
		v.Field = field
	case *BetweenExpr:
		v.Field = field
	case *IsNullExpr:
		v.Field = field
	case *NotNullExpr:
		v.Field = field
	case *JsonPathExpr:
		v.Field = field
	case *ArrayContainsExpr:
		v.Field = field
	case *ArrayOverlapsExpr:
		v.Field = field
	case *FullTextSearchExpr:
		v.Field = field
	case *GeoDistanceExpr:
		v.Field = field
	case *CustomExpr:
		v.Field = field
	default:
		return false
	}
	return true
}
