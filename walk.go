package figo

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
	case EqExpr:
		visit(&v)
		return v
	case NeqExpr:
		visit(&v)
		return v
	case GtExpr:
		visit(&v)
		return v
	case GteExpr:
		visit(&v)
		return v
	case LtExpr:
		visit(&v)
		return v
	case LteExpr:
		visit(&v)
		return v
	case LikeExpr:
		visit(&v)
		return v
	case ILikeExpr:
		visit(&v)
		return v
	case RegexExpr:
		visit(&v)
		return v
	case InExpr:
		visit(&v)
		return v
	case NotInExpr:
		visit(&v)
		return v
	case BetweenExpr:
		visit(&v)
		return v
	case IsNullExpr:
		visit(&v)
		return v
	case NotNullExpr:
		visit(&v)
		return v
	case JsonPathExpr:
		visit(&v)
		return v
	case ArrayContainsExpr:
		visit(&v)
		return v
	case ArrayOverlapsExpr:
		visit(&v)
		return v
	case FullTextSearchExpr:
		visit(&v)
		return v
	case GeoDistanceExpr:
		visit(&v)
		return v
	case CustomExpr:
		visit(&v)
		return v
	case OrderBy:
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
func (f *figo) Walk(visit func(Expr)) {
	// Snapshot under the lock, run the user's visitor OUTSIDE it (a visitor
	// calling back into figo methods must not deadlock), then swap the
	// rebuilt trees in under the lock again.
	f.mu.Lock()
	clauses := make([]Expr, len(f.clauses))
	copy(clauses, f.clauses)
	preloads := make(map[string][]Expr, len(f.preloads))
	for k, exprs := range f.preloads {
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

	f.mu.Lock()
	f.clauses = clauses
	f.preloads = preloads
	f.mu.Unlock()
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
