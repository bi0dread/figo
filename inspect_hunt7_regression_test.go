package figo_test

import (
	"fmt"
	"strings"
	"testing"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// L6 — Explain had no OrderBy case, so a shape clone.go and walk.go both handle
// printed as the bare Go type "figo.OrderBy".
// ---------------------------------------------------------------------------

func TestL6_ExplainRendersOrderBy(t *testing.T) {
	f := New()
	f.AddFilter(OrderBy{Columns: []OrderByColumn{{Name: "id", Desc: true}, {Name: "name"}}})
	assert.Equal(t, "ORDER BY id DESC, name ASC\n", f.Explain())

	e := New()
	e.AddFilter(OrderBy{})
	assert.Equal(t, "ORDER BY (none)\n", e.Explain())
}

// ---------------------------------------------------------------------------
// L6 (hardening) — Explain repeats the whole indentation on every line, so its
// output is O(nodes x depth): a ~27KB DSL rendered a 16MB string, and nothing
// bounded it. Explain may be called on untrusted input.
// ---------------------------------------------------------------------------

func TestL6_ExplainOutputIsBounded(t *testing.T) {
	const depth = 2000
	var sb strings.Builder
	sb.WriteString("a0=1")
	for i := 1; i < depth; i++ {
		fmt.Fprintf(&sb, " and (a%d=1", i)
	}
	sb.WriteString(strings.Repeat(")", depth-1))
	dsl := sb.String()

	f := New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(nil)

	out := f.Explain()
	// Before the cap this was ~16MB from a ~27KB input (a ~600x amplification).
	assert.Less(t, len(out), 2<<20, "Explain output must stay bounded (input %d bytes)", len(dsl))
	assert.Contains(t, out, "output truncated")
	// The part that did fit is still a valid rendering of the head of the tree.
	assert.True(t, strings.HasPrefix(out, "AND\n"), "unexpected rendering head")
}

// ---------------------------------------------------------------------------
// A7-1 — Figo.Walk was a non-atomic read-modify-write: it snapshotted the
// clauses/preloads, ran the visitor unlocked, then overwrote the instance
// wholesale, silently discarding anything added meanwhile and resurrecting
// anything cleared.
// ---------------------------------------------------------------------------

func TestA7_1_WalkKeepsFilterAddedByVisitor(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`status="pending"`))
	f.Build(RawAdapter{})

	f.Walk(func(n Expr) {
		if fld, ok := NodeField(n); ok && fld == "status" {
			// A per-request tenant scope added from inside the visitor. The docs
			// say a visitor may call other Figo methods.
			f.AddFilter(EqExpr{Field: "tenant_id", Value: "acme"})
		}
	})

	where, args, _ := BuildRawWhere(f)
	assert.Len(t, f.GetClauses(), 2, "the filter added during the walk must survive")
	assert.Contains(t, where, "`tenant_id`")
	assert.Contains(t, args, "acme")
}

func TestA7_1_WalkAppliesItsOwnMutationsWhenAFilterIsAdded(t *testing.T) {
	// The merge must not throw the walk's own work away either.
	f := New()
	require.NoError(t, f.AddFiltersFromString(`status="pending"`))
	f.Build(RawAdapter{})

	f.Walk(func(n Expr) {
		if fld, ok := NodeField(n); ok && fld == "status" {
			SetNodeField(n, "orders.status")
			f.AddFilter(EqExpr{Field: "tenant_id", Value: "acme"})
		}
	})

	where, _, _ := BuildRawWhere(f)
	assert.Contains(t, where, "`orders`.`status`", "the walk's rename must be installed too: %s", where)
	assert.Contains(t, where, "`tenant_id`", "where: %s", where)
}

func TestA7_1_WalkKeepsPreloadAddedByVisitor(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`status="pending"`))
	f.Build(RawAdapter{})

	f.Walk(func(n Expr) {
		if fld, ok := NodeField(n); ok && fld == "status" {
			require.NoError(t, f.AddFiltersFromString(`b=2 load=[Orders:x=1]`))
			f.Build(RawAdapter{})
		}
	})

	// The write-back used to install the (empty) preload snapshot over the new
	// state, leaving GetDSL() advertising a load= that no longer existed.
	assert.Contains(t, f.GetDSL(), "load=[Orders:x=1]")
	assert.Contains(t, f.GetPreloads(), "Orders", "preloads: %v", f.GetPreloads())
}

func TestA7_1_WalkDoesNotResurrectClearedClauses(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`secret="leak" and a=1`))
	f.Build(RawAdapter{})

	f.Walk(func(n Expr) {
		if fld, ok := NodeField(n); ok && fld == "secret" {
			require.NoError(t, f.AddFiltersFromString(""))
			f.Build(RawAdapter{})
		}
	})

	where, args, _ := BuildRawWhere(f)
	assert.Equal(t, "", f.GetDSL())
	assert.Empty(t, f.GetClauses(), "cleared clauses must not come back")
	assert.Equal(t, "", where)
	assert.Empty(t, args)
}

func TestA7_1_WalkKeepsConcurrentAddFilter(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`status="pending"`))
	f.Build(RawAdapter{})

	inWalk := make(chan struct{})
	added := make(chan struct{})
	go func() {
		<-inWalk
		f.AddFilter(EqExpr{Field: "tenant_id", Value: "acme"})
		close(added)
	}()

	once := false
	f.Walk(func(Expr) {
		if !once {
			once = true
			close(inWalk)
			<-added // the filter is in place before the write-back runs
		}
	})

	where, _, _ := BuildRawWhere(f)
	assert.Len(t, f.GetClauses(), 2, "a filter added by another goroutine must not be lost")
	assert.Contains(t, where, "`tenant_id`")
}

// ---------------------------------------------------------------------------
// A7-2 — Clone copied the struct but not the object behind a node's `any`
// payload, so clone and original shared it and a write through either was
// visible on the other, contradicting Clone's documented independence.
// ---------------------------------------------------------------------------

func TestA7_2_CloneDeepCopiesDynamicValue(t *testing.T) {
	t.Run("MapValue", func(t *testing.T) {
		o := New()
		o.AddFilter(EqExpr{Field: "password", Value: map[string]any{"$ne": nil}})
		c := o.Clone()
		c.Walk(func(n Expr) {
			if eq, ok := n.(*EqExpr); ok {
				eq.Value.(map[string]any)["POISONED"] = true
			}
		})
		orig := o.GetClauses()[0].(EqExpr).Value.(map[string]any)
		assert.NotContains(t, orig, "POISONED", "clone must not write through to the original: %v", orig)
	})

	t.Run("NestedSliceElement", func(t *testing.T) {
		o := New()
		o.AddFilter(InExpr{Field: "tags", Values: []any{[]any{"a"}}})
		c := o.Clone()
		c.Walk(func(n Expr) {
			if in, ok := n.(*InExpr); ok {
				in.Values[0].([]any)[0] = "POISONED"
			}
		})
		assert.Equal(t, []any{[]any{"a"}}, o.GetClauses()[0].(InExpr).Values)
	})

	t.Run("BetweenLowHigh", func(t *testing.T) {
		o := New()
		o.AddFilter(BetweenExpr{Field: "n", Low: map[string]any{"lo": 1}, High: []any{2}})
		c := o.Clone()
		c.Walk(func(n Expr) {
			if b, ok := n.(*BetweenExpr); ok {
				b.Low.(map[string]any)["POISONED"] = true
				b.High.([]any)[0] = "POISONED"
			}
		})
		bet := o.GetClauses()[0].(BetweenExpr)
		assert.NotContains(t, bet.Low.(map[string]any), "POISONED")
		assert.Equal(t, []any{2}, bet.High)
	})

	t.Run("CloneExprNeedsNoWalk", func(t *testing.T) {
		src := EqExpr{Field: "f", Value: map[string]any{"k": 1}}
		cl := CloneExpr(src).(EqExpr)
		cl.Value.(map[string]any)["POISONED"] = true
		assert.NotContains(t, src.Value.(map[string]any), "POISONED")
	})

	t.Run("TypedContainers", func(t *testing.T) {
		// Not just the decoded-JSON shapes: any slice or map is copied.
		o := New()
		o.AddFilter(EqExpr{Field: "tags", Value: []string{"a"}})
		o.AddFilter(EqExpr{Field: "meta", Value: map[string]int{"n": 1}})
		o.AddFilter(EqExpr{Field: "blob", Value: []byte("x")})
		c := o.Clone()
		c.Walk(func(n Expr) {
			eq, ok := n.(*EqExpr)
			if !ok {
				return
			}
			switch v := eq.Value.(type) {
			case []string:
				v[0] = "POISONED"
			case map[string]int:
				v["n"] = 999
			case []byte:
				v[0] = 'P'
			}
		})
		cl := o.GetClauses()
		assert.Equal(t, []string{"a"}, cl[0].(EqExpr).Value)
		assert.Equal(t, map[string]int{"n": 1}, cl[1].(EqExpr).Value)
		assert.Equal(t, []byte("x"), cl[2].(EqExpr).Value)
	})

	t.Run("ControlScalarsAndUncopyableTypesStillWork", func(t *testing.T) {
		type opaque struct{ N int }
		o := New()
		o.AddFilter(EqExpr{Field: "n", Value: 1})
		o.AddFilter(EqExpr{Field: "s", Value: opaque{N: 2}})
		c := o.Clone()
		assert.Equal(t, 1, c.GetClauses()[0].(EqExpr).Value)
		assert.Equal(t, opaque{N: 2}, c.GetClauses()[1].(EqExpr).Value)
	})
}

func TestA7_2_WalkDoesNotWriteThroughTheSnapshottedValue(t *testing.T) {
	// The package-level Walk hands the visitor a copy of the node; the object
	// behind Value must be copied with it, or the visitor writes into the tree
	// the caller still holds (and into any snapshot handed out earlier).
	m := map[string]any{"k": 1}
	var ast Expr = EqExpr{Field: "a", Value: m}

	out := Walk(ast, func(n Expr) {
		if eq, ok := n.(*EqExpr); ok {
			eq.Value.(map[string]any)["POISONED"] = true
		}
	})

	assert.NotContains(t, m, "POISONED", "Walk must not mutate the input tree's value in place")
	assert.Contains(t, out.(EqExpr).Value.(map[string]any), "POISONED", "the visitor's edit must still land in the returned tree")
}

// ---------------------------------------------------------------------------
// A7-4 — Clone shared pointer-form nodes by reference, so mutating the clone
// rewrote the original. (Walk's non-recursion into pointer nodes is documented
// as deliberate at walk.go's default case and is left alone.)
// ---------------------------------------------------------------------------

func TestA7_4_CloneIsIndependentForPointerNodes(t *testing.T) {
	t.Run("PointerLeaf", func(t *testing.T) {
		p := &EqExpr{Field: "tenant_id", Value: "A"}
		f := New()
		f.AddFilter(p)
		c := f.Clone()
		c.Walk(func(n Expr) { SetNodeField(n, "HACKED") })
		assert.Equal(t, "tenant_id", p.Field, "clone.Walk must not rewrite the original's node")
	})

	t.Run("PointerLogicalNode", func(t *testing.T) {
		inner := EqExpr{Field: "a", Value: 1}
		p := &AndExpr{Operands: []Expr{inner}}
		f := New()
		f.AddFilter(p)
		c := f.Clone()

		// Mutate through the clone's own clause handle.
		cp, ok := c.GetClauses()[0].(*AndExpr)
		require.True(t, ok)
		assert.NotSame(t, p, cp, "the clone must hold a fresh pointer node")
		cp.Operands[0] = EqExpr{Field: "HACKED", Value: 1}

		assert.Equal(t, []Expr{inner}, p.Operands, "the original's operands must be untouched")
	})

	t.Run("CyclicPointerASTTerminates", func(t *testing.T) {
		cyc := &AndExpr{}
		cyc.Operands = []Expr{cyc}
		f := New()
		f.AddFilter(cyc)
		assert.NotPanics(t, func() { _ = f.Clone() })
	})
}

// ---------------------------------------------------------------------------
// A7-5 — Explain printed materially different ASTs identically: a CustomExpr no
// SQL adapter can render printed exactly like a working EqExpr, and two
// JsonPathExprs with different Mongo paths printed the same string.
// ---------------------------------------------------------------------------

func TestA7_5_ExplainIsInjectiveForCustomAndJsonPath(t *testing.T) {
	nodes := map[string]Expr{
		"EqExpr{a,1}":          EqExpr{Field: "a", Value: 1},
		"CustomExpr{a,=,1}":    CustomExpr{Field: "a", Operator: "=", Value: 1},
		"LikeExpr{a,x}":        LikeExpr{Field: "a", Value: "x"},
		"CustomExpr{a,LIKE,x}": CustomExpr{Field: "a", Operator: "LIKE", Value: "x"},
		"RegexExpr{a,x}":       RegexExpr{Field: "a", Value: "x"},
		"CustomExpr{a,=~,x}":   CustomExpr{Field: "a", Operator: "=~", Value: "x"},
		"JsonPath{a,$.b}":      JsonPathExpr{Field: "a", Path: "$.b", Op: "=", Value: 1},
		"JsonPath{a$,.b}":      JsonPathExpr{Field: "a$", Path: ".b", Op: "=", Value: 1},
		"CustomExpr{a$.b,=,1}": CustomExpr{Field: "a$.b", Operator: "=", Value: 1},
	}

	seen := map[string]string{}
	for name, e := range nodes {
		f := New()
		f.AddFilter(e)
		out := f.Explain()
		if prev, dup := seen[out]; dup {
			t.Errorf("Explain collision: %s and %s both render %q", prev, name, out)
		}
		seen[out] = name
	}
}

// ---------------------------------------------------------------------------
// A7-6 — Explain read only the clauses, so a DSL whose only predicate lives in
// load=[...] reported "(no filters)" while that predicate was emitted into the
// statement.
// ---------------------------------------------------------------------------

func TestA7_6_ExplainShowsPreloads(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:status="secret"]`))
	f.Build(RawAdapter{})

	out := f.Explain()
	assert.Contains(t, out, "PRELOADS")
	assert.Contains(t, out, "Orders")
	assert.Contains(t, out, `status = "secret"`)

	// The predicate Explain must not hide is real state: since hunt #6 H10 the
	// raw adapter no longer folds preloads into the main SELECT (a keyless JOIN
	// altered the primary row set), so BuildRawPreloads is where it surfaces.
	sql, ok := RawAdapter{}.GetSqlString(f, "users")
	require.True(t, ok)
	assert.NotContains(t, sql, "JOIN", "a preload must not alter the main statement: %s", sql)

	pre, err := BuildRawPreloads(f)
	require.NoError(t, err)
	require.Contains(t, pre, "Orders")
	assert.Contains(t, pre["Orders"].Where, "status")
	assert.Equal(t, []any{"secret"}, pre["Orders"].Args)
}

func TestA7_6_ExplainPreloadsAreSortedAndKeepClauseTree(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 load=[Zeta:a=1] load=[Alpha:b=2]`))
	f.Build(RawAdapter{})

	out := f.Explain()
	assert.True(t, strings.HasPrefix(out, "id = 1\n"), "clause tree still comes first: %q", out)
	assert.Less(t, strings.Index(out, "Alpha"), strings.Index(out, "Zeta"),
		"relations are printed in a deterministic (sorted) order: %q", out)

	// An instance with neither clauses nor preloads is unchanged.
	assert.Equal(t, "(no filters)", New().Explain())
}
