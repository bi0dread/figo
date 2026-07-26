package figo_test

// Regression tests for hunt #8, area "AST inspection & mutation surface"
// (clone.go, walk.go, explain.go).

import (
	"reflect"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/bi0dread/figo/v4/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dagValueH8 builds n maps, each holding the previous one under TWO keys: n+1
// live objects, but 2^n distinct root-to-leaf paths.
func dagValueH8(n int) any {
	var cur any = map[string]any{"leaf": "x"}
	for i := 0; i < n; i++ {
		cur = map[string]any{"a": cur, "b": cur}
	}
	return cur
}

// countContainersH8 counts DISTINCT slice/map objects reachable from v. It is
// memoised on the object's backing pointer, so it terminates on a DAG (and would
// count a path-expanded copy as the millions of objects it is).
func countContainersH8(v any) int {
	seen := map[uintptr]bool{}
	var rec func(any)
	rec = func(x any) {
		rv := reflect.ValueOf(x)
		if !rv.IsValid() {
			return
		}
		switch rv.Kind() {
		case reflect.Map:
			if rv.IsNil() || seen[rv.Pointer()] {
				return
			}
			seen[rv.Pointer()] = true
			it := rv.MapRange()
			for it.Next() {
				rec(it.Value().Interface())
			}
		case reflect.Slice:
			if rv.IsNil() || seen[rv.Pointer()] {
				return
			}
			seen[rv.Pointer()] = true
			for i := 0; i < rv.Len(); i++ {
				rec(rv.Index(i).Interface())
			}
		}
	}
	rec(v)
	return len(seen)
}

// A10-3: a DAG-shaped bind value must cost what its OBJECT count says, not what
// its PATH count says. deepCopyValue's only bound used to be recursion depth, so
// 21 live objects were copied into 2,097,151 and one GetClauses — which every
// render calls, twice — took 350ms instead of microseconds.
func TestH8A103DeepCopyOfSharedSubtreeIsNotExponential(t *testing.T) {
	const depth = 20 // 21 live objects, 2^20 paths
	v := dagValueH8(depth)
	require.Equal(t, depth+1, countContainersH8(v), "probe value should hold depth+1 objects")

	f := figo.New()
	f.AddFilter(figo.EqExpr{Field: "meta", Value: v})

	start := time.Now()
	snap := f.GetClauses()
	elapsed := time.Since(start)

	copies := countContainersH8(snap[0].(figo.EqExpr).Value)
	// Bounded by the memo, not by 2^depth. Without the fix this is 2097151.
	assert.Less(t, copies, 1000, "copy of a %d-object DAG expanded into %d objects", depth+1, copies)
	assert.Less(t, elapsed, 50*time.Millisecond, "GetClauses of a %d-object DAG took %v", depth+1, elapsed)

	// The same on the Clone path, and on a render (which calls GetClauses).
	start = time.Now()
	_ = f.Clone()
	assert.Less(t, time.Since(start), 50*time.Millisecond, "Clone of a DAG value")

	start = time.Now()
	sql, args, err := adapters.BuildRawSelect(f, "users")
	renderTook := time.Since(start)
	require.NoError(t, err)
	assert.Len(t, args, 1, "the value is a bind parameter, it never reaches the SQL text")
	assert.Equal(t, "SELECT * FROM `users` WHERE `meta` = ? LIMIT 20", sql)
	assert.Less(t, renderTook, 50*time.Millisecond, "raw render with a DAG value took %v", renderTook)
}

// A10-3, past the depth budget: the dossier's "33 live objects would need ~4.3e9
// allocations". Nothing here may become unbounded at or beyond the budget.
func TestH8A103DeepCopyStaysBoundedAtAndPastDepthBudget(t *testing.T) {
	for _, depth := range []int{24, 32, 40, 64} {
		f := figo.New()
		f.AddFilter(figo.EqExpr{Field: "meta", Value: dagValueH8(depth)})
		start := time.Now()
		snap := f.GetClauses()
		elapsed := time.Since(start)
		copies := countContainersH8(snap[0].(figo.EqExpr).Value)
		assert.Less(t, copies, 1000, "depth %d expanded into %d objects", depth, copies)
		assert.Less(t, elapsed, 50*time.Millisecond, "depth %d took %v", depth, elapsed)
	}
}

// A10-3 must not have reopened hunt #7's A7-2 (Clone/GetClauses sharing the
// object behind a node's `any`): a write anywhere in the snapshot or the clone,
// including through a subtree the original shares between two keys, must not be
// visible on the instance's own value.
func TestH8A103CloneIndependenceSurvivesTheMemo(t *testing.T) {
	inner := map[string]any{"k": "orig"}
	root := map[string]any{"a": inner, "b": inner, "list": []any{inner}}

	f := figo.New()
	f.AddFilter(figo.EqExpr{Field: "meta", Value: root})

	snap := f.GetClauses()
	cv := snap[0].(figo.EqExpr).Value.(map[string]any)
	cv["a"].(map[string]any)["k"] = "mut-a"
	cv["b"].(map[string]any)["k"] = "mut-b"
	cv["list"].([]any)[0].(map[string]any)["k"] = "mut-list"
	cv["added-key"] = 1

	assert.Equal(t, "orig", inner["k"], "a write through the GetClauses snapshot reached the caller's value")
	assert.Len(t, root, 3, "a key added to the snapshot reached the caller's map")

	cl := f.Clone()
	cc := cl.GetClauses()[0].(figo.EqExpr).Value.(map[string]any)
	cc["a"].(map[string]any)["k"] = "clone-mut"
	assert.Equal(t, "orig", inner["k"], "a write through the Clone reached the caller's value")

	// Typed containers (the reflect path, not the map[string]any fast path).
	typed := map[string][]string{"s": {"a", "b"}}
	f2 := figo.New()
	f2.AddFilter(figo.EqExpr{Field: "m", Value: typed})
	f2.AddFilter(figo.InExpr{Field: "in", Values: []any{[]int{1, 2, 3}, []byte("bs")}})
	s2 := f2.GetClauses()
	s2[0].(figo.EqExpr).Value.(map[string][]string)["s"][0] = "MUT"
	s2[1].(figo.InExpr).Values[0].([]int)[0] = 99
	s2[1].(figo.InExpr).Values[1].([]byte)[0] = 'Z'
	assert.Equal(t, []string{"a", "b"}, typed["s"])
	assert.Equal(t, []any{[]int{1, 2, 3}, []byte("bs")}, f2.GetClauses()[1].(figo.InExpr).Values)
}

// A10-3: the depth budget exists because a container behind an `any` can be
// cyclic. Keying the memo on (identity, remaining depth) must not have turned
// that termination into an infinite loop or an aliased copy.
func TestH8A103CyclicValueStillTerminates(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	s := []any{nil}
	s[0] = s

	f := figo.New()
	f.AddFilter(figo.EqExpr{Field: "c", Value: m})
	f.AddFilter(figo.EqExpr{Field: "s", Value: s})

	done := make(chan []figo.Expr, 1)
	go func() { done <- f.GetClauses() }()
	select {
	case snap := <-done:
		// The outermost copy must be a fresh object, not the caller's.
		got := snap[0].(figo.EqExpr).Value.(map[string]any)
		got["added"] = 1
		assert.Len(t, m, 1, "the cyclic copy aliased the caller's map")
	case <-time.After(10 * time.Second):
		t.Fatal("GetClauses did not terminate on a cyclic value")
	}
}

// Hunt #8, the gap the completeness critic named: Walk is the one route into the
// clause tree that bypasses AddFilter. These tests pin what the guards do on that
// route, so the behaviour documented on (Figo).Walk cannot drift silently.

// The raw adapter's identifier validation (added by ef28331) is render-time, so it
// applies to a name Walk installed exactly as it does to one AddFilter installed.
func TestH8WalkCannotSmuggleAnUnrenderableIdentifierPastRaw(t *testing.T) {
	for _, bad := range []string{"a\x00b", "...", "a..b", "name\nDROP"} {
		viaWalk := figo.New()
		require.NoError(t, viaWalk.AddFiltersFromString(`name="x"`))
		viaWalk.Build(nil)
		viaWalk.Walk(func(n figo.Expr) {
			if fld, ok := figo.NodeField(n); ok && fld == "name" {
				figo.SetNodeField(n, bad)
			}
		})
		sqlWalk, _, errWalk := adapters.BuildRawSelect(viaWalk, "users")

		viaAdd := figo.New()
		viaAdd.AddFilter(figo.EqExpr{Field: bad, Value: "x"})
		_, _, errAdd := adapters.BuildRawSelect(viaAdd, "users")

		assert.Error(t, errWalk, "raw rendered %q installed through Walk", bad)
		assert.Empty(t, sqlWalk)
		assert.Error(t, errAdd, "raw rendered %q installed through AddFilter", bad)
	}
}

// Characterisation, matching the (Figo).Walk doc comment: a name written through
// Walk is final — the naming strategy is not re-applied and a FieldsPlugin ignore
// list does not re-prune. If a future change starts enforcing either, this test
// is the one to update, together with that doc comment.
func TestH8WalkNameIsFinalNotRenormalisedNorRepoliced(t *testing.T) {
	snake := func(s string) string {
		out := make([]rune, 0, len(s)+2)
		for i, r := range s {
			if r >= 'A' && r <= 'Z' {
				if i > 0 {
					out = append(out, '_')
				}
				r = r - 'A' + 'a'
			}
			out = append(out, r)
		}
		return string(out)
	}

	// Naming: AddFilter and the DSL both convert; Walk does not.
	viaAdd := figo.New()
	viaAdd.SetNamingFunc(snake)
	viaAdd.AddFilter(figo.EqExpr{Field: "userName", Value: "x"})
	sqlAdd, _, err := adapters.BuildRawSelect(viaAdd, "users")
	require.NoError(t, err)
	assert.Contains(t, sqlAdd, "`user_name`")

	viaWalk := figo.New()
	viaWalk.SetNamingFunc(snake)
	require.NoError(t, viaWalk.AddFiltersFromString(`name="x"`))
	viaWalk.Build(nil)
	viaWalk.Walk(func(n figo.Expr) {
		if fld, ok := figo.NodeField(n); ok && fld == "name" {
			figo.SetNodeField(n, "userName")
		}
	})
	sqlWalk, _, err := adapters.BuildRawSelect(viaWalk, "users")
	require.NoError(t, err)
	assert.Contains(t, sqlWalk, "`userName`", "Walk names are documented as verbatim")

	// Field policy: the ignore list prunes on the way in, not on the way out.
	newDenied := func() figo.Figo {
		fp := plugins.NewFieldsPlugin()
		fp.AddIgnoreFields("password")
		pm := figo.NewPluginManager()
		require.NoError(t, pm.RegisterPlugin(fp))
		g := figo.New()
		g.SetPluginManager(pm)
		return g
	}

	guarded := newDenied()
	require.NoError(t, guarded.AddFiltersFromString(`password="secret"`))
	guarded.Build(nil)
	sqlGuarded, _, err := adapters.BuildRawSelect(guarded, "users")
	require.NoError(t, err)
	assert.NotContains(t, sqlGuarded, "password", "ignore list must still prune on the DSL path")

	walked := newDenied()
	require.NoError(t, walked.AddFiltersFromString(`name="secret"`))
	walked.Build(nil)
	walked.Walk(func(n figo.Expr) {
		if fld, ok := figo.NodeField(n); ok && fld == "name" {
			figo.SetNodeField(n, "password")
		}
	})
	sqlWalked, _, err := adapters.BuildRawSelect(walked, "users")
	require.NoError(t, err)
	assert.Contains(t, sqlWalked, "`password`",
		"documented on (Figo).Walk: the visitor's own code is what limits which columns it may name")
}

// The completeness critic's spot-check, confirmed here so it stays confirmed:
// switching adapters repeatedly on one instance leaves no adapter-specific
// residue, and a Clone taken afterwards renders identically.
func TestH8AdapterSwitchingAndCloneLeaveNoResidue(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`id=1 and name="a" page=take:5,skip:10 sort=id:desc`))
	f.Build(nil)

	f.Build(adapters.ElasticsearchAdapter{})
	es1 := f.GetQuery(nil)
	_, _, err := adapters.BuildRawSelect(f, "users")
	require.NoError(t, err)
	f.Build(adapters.ElasticsearchAdapter{})
	es2 := f.GetQuery(nil)
	f.Build(adapters.MongoAdapter{})
	require.NotNil(t, f.GetQuery(nil))
	f.Build(adapters.ElasticsearchAdapter{})
	es3 := f.GetQuery(nil)

	assert.Equal(t, es1, es2, "ES output changed after a raw render on the same instance")
	assert.Equal(t, es1, es3, "ES output changed after a Mongo build on the same instance")
	assert.Equal(t, figo.Page{Skip: 10, Take: 5}, f.GetPage())
	require.NotNil(t, f.GetSort())

	cl := f.Clone()
	assert.Equal(t, es3, cl.GetQuery(nil), "the clone renders differently from the original")
	assert.Equal(t, f.GetPage(), cl.GetPage())
	assert.Equal(t, f.GetSort(), cl.GetSort())
	assert.Equal(t, f.GetClauses(), cl.GetClauses())
}
