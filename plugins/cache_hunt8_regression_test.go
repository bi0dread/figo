package plugins

import (
	"fmt"
	"strings"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// H8-CACHE-1: one oversized render context must not disable the cache for that
// context TYPE. The budget is a fail-safe for a VALUE that cannot be keyed by
// contents; it used to be remembered per reflect.Type in a sticky sync.Map, so
// a single large context permanently and silently switched caching off for
// every later value of the type, process-wide, including for CachePlugins
// created afterwards with their own InMemoryCache.
// ---------------------------------------------------------------------------

// hunt8Ctx is an application-supplied render context whose size varies between
// requests — which the figo.Adapter contract explicitly permits.
type hunt8Ctx struct {
	Table  string
	Params []string
}

type hunt8Adapter struct{}

func (hunt8Adapter) GetSqlString(f figo.Figo, ctx any, conditionType ...string) (string, bool) {
	c, ok := ctx.(hunt8Ctx)
	if !ok {
		return "", false
	}
	return adapters.RawAdapter{}.GetSqlString(f, adapters.RawContext{Table: c.Table}, conditionType...)
}

func (hunt8Adapter) GetQuery(f figo.Figo, ctx any, conditionType ...string) (figo.Query, bool) {
	c, ok := ctx.(hunt8Ctx)
	if !ok {
		return nil, false
	}
	return adapters.RawAdapter{}.GetQuery(f, adapters.RawContext{Table: c.Table}, conditionType...)
}

func hunt8CtxOf(params int) hunt8Ctx {
	p := make([]string, params)
	for i := range p {
		p[i] = fmt.Sprintf("p%d", i)
	}
	return hunt8Ctx{Table: "users", Params: p}
}

func TestH8CACHE1_OversizedCtxValueDoesNotPoisonTheCtxType(t *testing.T) {
	build := func() figo.Figo {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString("id=1"))
		f.Build(hunt8Adapter{})
		return f
	}

	f := build()
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Hour, MaxSize: 1000})
	defer cp.Close()

	small := hunt8CtxOf(1)
	for i := 0; i < 4; i++ {
		_ = cp.GetCachedSqlString(f, small)
	}
	require.Equal(t, int64(3), cp.Stats().Hits, "baseline: 4 renders of one small ctx = 1 miss + 3 hits")

	// ONE render whose ctx is far over the node budget. It legitimately
	// bypasses the cache — but only for itself.
	_ = cp.GetCachedSqlString(f, hunt8CtxOf(2000))

	before := cp.Stats().Hits
	for i := 0; i < 4; i++ {
		_ = cp.GetCachedSqlString(f, small)
	}
	assert.Equal(t, int64(4), cp.Stats().Hits-before,
		"an oversized ctx VALUE must not make later small values of the same TYPE unkeyable")

	// ...and it must not have leaked into a brand-new plugin with a brand-new
	// cache either (the memo was process-global).
	cp2 := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Hour, MaxSize: 1000})
	defer cp2.Close()
	f2 := build()
	for i := 0; i < 4; i++ {
		_ = cp2.GetCachedSqlString(f2, small)
	}
	assert.Equal(t, int64(3), cp2.Stats().Hits,
		"a fresh CachePlugin with a fresh cache must not inherit another plugin's bypass decision")
}

// The critic's correction to H8-CACHE-1: map[string]adapters.MongoJoin is the
// documented ctx of the Mongo aggregate path (adapters/mongo.go), and at a
// 64-node budget it went unkeyable at ELEVEN joins — so a realistic aggregate
// application lost caching entirely, and (with the sticky memo) so did every
// smaller join set that followed.
func TestH8CACHE1_MongoJoinCtxStaysKeyable(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString("id=1"))
	f.Build(adapters.MongoAdapter{})

	joins := func(n int) map[string]adapters.MongoJoin {
		m := make(map[string]adapters.MongoJoin, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("rel%d", i)] = adapters.MongoJoin{
				From:         fmt.Sprintf("c%d", i),
				LocalField:   "id",
				ForeignField: "uid",
				As:           fmt.Sprintf("a%d", i),
			}
		}
		return m
	}

	keys := map[int]string{}
	for _, n := range []int{4, 11, 32, 64, 4} {
		k := generateCacheKey(f, "sql", joins(n), "AGG")
		assert.NotEmpty(t, k, "a %d-join Mongo aggregate ctx must be keyable by contents", n)
		if prev, ok := keys[n]; ok {
			assert.Equal(t, prev, k, "the same join set must key identically after a bigger one was seen")
		}
		keys[n] = k
	}
	assert.NotEqual(t, keys[4], keys[11], "different join sets must not share a key")
}

// ---------------------------------------------------------------------------
// H8-CACHE-2: the clause component keyed payloads by ADDRESS.
// ---------------------------------------------------------------------------

// Two CustomExpr handlers minted from one factory closure share a CODE pointer,
// so %#v rendered them identically and one tenant's WHERE was served to the
// other. A clause carrying a live func cannot be keyed by contents at all, so
// it must fail SAFE: no key, no caching, correct SQL every time.
func TestH8CACHE2_CustomExprHandlerIsNotServedAcrossTenants(t *testing.T) {
	mk := func(tenant string) func(field, operator string, value any) (string, []any, error) {
		return func(field, operator string, value any) (string, []any, error) {
			return fmt.Sprintf("tenant_id = '%s'", tenant), nil, nil
		}
	}
	build := func(tenant string) figo.Figo {
		f := figo.New()
		f.AddFilter(figo.CustomExpr{Field: "x", Operator: "scope", Value: 1, Handler: mk(tenant)})
		f.Build(adapters.RawAdapter{})
		return f
	}
	ctx := adapters.RawContext{Table: "users"}

	assert.Empty(t, generateCacheKey(build("acme"), "sql", ctx),
		"a clause carrying a live handler func is not keyable by contents and must bypass the cache")

	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Hour, MaxSize: 1000})
	defer cp.Close()
	first := cp.GetCachedSqlString(build("acme"), ctx)
	second := cp.GetCachedSqlString(build("globex"), ctx)
	assert.Contains(t, first, "tenant_id = 'acme'")
	assert.Contains(t, second, "tenant_id = 'globex'",
		"globex must not be served acme's cached WHERE (handler closures share a code pointer)")
	assert.Zero(t, cp.Stats().Size, "nothing may be stored for an unkeyable clause tree")
}

type hunt8Tenant struct{ Name string }

// A pointer-valued clause payload used to key by its heap ADDRESS: two equal
// payloads at different addresses never shared a slot, and address reuse
// collided unrelated payloads into one.
func TestH8CACHE2_PointerPayloadIsKeyedByContents(t *testing.T) {
	build := func(p *hunt8Tenant) figo.Figo {
		f := figo.New()
		f.AddFilter(figo.EqExpr{Field: "tenant", Value: p})
		f.Build(adapters.RawAdapter{})
		return f
	}
	ctx := adapters.RawContext{Table: "users"}

	k1 := generateCacheKey(build(&hunt8Tenant{Name: "acme"}), "sql", ctx)
	k2 := generateCacheKey(build(&hunt8Tenant{Name: "acme"}), "sql", ctx)
	k3 := generateCacheKey(build(&hunt8Tenant{Name: "globex"}), "sql", ctx)
	require.NotEmpty(t, k1)
	assert.Equal(t, k1, k2, "equal payloads at different addresses must share a key (else the hit rate is 0%)")
	assert.NotEqual(t, k1, k3, "different payloads must never share a key")

	// Address reuse across many short-lived payloads must not collide.
	seen := make(map[string]string, 20000)
	collisions := 0
	for i := 0; i < 20000; i++ {
		name := fmt.Sprintf("tenant-%d", i)
		k := generateCacheKey(build(&hunt8Tenant{Name: name}), "sql", ctx)
		require.NotEmpty(t, k)
		if prev, ok := seen[k]; ok && prev != name {
			collisions++
		}
		seen[k] = name
	}
	assert.Zero(t, collisions, "pointer payloads must key by contents, not by a reusable address")
	assert.Equal(t, 20000, len(seen), "20000 distinct payloads must produce 20000 distinct keys")
}

// The clause fingerprint renders a map entry as "key=value"; a key that itself
// contains '=' made map{"a=b":"c"} and map{"a":"b=c"} share a cache slot. Both
// the preload map (relation names) and an application ctx map reach this.
func TestH8CACHE2_MapKeyIsLengthDelimited(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString("id=1"))
	f.Build(adapters.RawAdapter{})

	a := generateCacheKey(f, "sql", map[string]string{"a=b": "c"})
	b := generateCacheKey(f, "sql", map[string]string{"a": "b=c"})
	require.NotEmpty(t, a)
	assert.NotEqual(t, a, b, `map{"a=b":"c"} and map{"a":"b=c"} are different contexts and must key differently`)
}

// Both directions at scale: no two configurations that render DIFFERENT SQL may
// share a key (a wrong serve), and the same configuration must always produce
// the same key (a silent 0% hit rate is a defect too).
func TestH8CACHE2_KeyMatrixNoWrongServeAndDeterministic(t *testing.T) {
	ctxs := []any{
		nil,
		"users",
		adapters.RawContext{Table: "users"},
		adapters.RawContext{Table: "orders"},
		hunt8CtxOf(1),
		hunt8CtxOf(20),
		map[string]string{"a=b": "c"},
		map[string]string{"a": "b=c"},
	}
	dsls := []string{
		"id=1", "id=2", `id=1 and name="a"`, `id=1 or name="a"`,
		"not id=1", "id=1 and (a=2 or b=3)", "id=1 and (a=2 or (b=3 and c=4))",
		"id=1 and (a=2 or (b=3 and (c=4 or d=5)))",
		`id="1"`, "id=1.0", "id>1 and id<5", "id=1 page=take:10,skip:5",
		"id=1 sort=name:desc", "id=1 select=[name,age]",
		"load=[Orders:total>100]", "load=[Orders:total>100.5]",
		"load=[Orders:total>100] load=[Items:qty=2]",
	}
	kinds := []string{"sql", "query"}
	conds := [][]string{nil, {"where"}, {"where", "select"}, {"where select"}}
	adapterCfgs := []figo.Adapter{
		adapters.RawAdapter{},
		adapters.RawAdapter{Dialect: adapters.PostgresDialect},
		adapters.RawAdapter{Dialect: adapters.SQLiteDialect},
		hunt8Adapter{},
	}

	// Programmatic payloads the DSL cannot express, applied on top of every
	// combination above: a pointer (keyed by address before this fix, so two
	// identical configurations produced DIFFERENT keys) and two handler
	// closures from one factory (one code pointer, so two configurations that
	// render different SQL produced the SAME key).
	mkHandler := func(tenant string) func(field, operator string, value any) (string, []any, error) {
		return func(field, operator string, value any) (string, []any, error) {
			return fmt.Sprintf("tenant_id = '%s'", tenant), nil, nil
		}
	}
	// One call site for mkHandler, so both tenants' handlers really do share a
	// code pointer: two call sites let the compiler inline mkHandler twice and
	// hand out two distinct code pointers, which would hide the collision.
	mutate := func(f figo.Figo, mut string) {
		switch mut {
		case "plain":
		case "ptr":
			f.AddFilter(figo.EqExpr{Field: "tenant", Value: &hunt8Tenant{Name: "acme"}})
		default:
			tenant := strings.TrimPrefix(mut, "handler-")
			f.AddFilter(figo.CustomExpr{Field: "x", Operator: "scope", Value: 1, Handler: mkHandler(tenant)})
		}
	}
	mutatorNames := []string{"plain", "ptr", "handler-acme", "handler-globex"}

	type cfg struct {
		id  string
		sql string
	}
	byKey := make(map[string]cfg)
	configs, keyed, bypassed, wrongServe, nondet, unexpectedBypass := 0, 0, 0, 0, 0, 0

	// The mutator runs AFTER Build: an AddFilter before Build is discarded when
	// a DSL is set (a documented sharp edge), which would make every mutator a
	// no-op and this matrix vacuous.
	build := func(dsl string, a figo.Adapter, mut string) figo.Figo {
		f := figo.New()
		_ = f.AddFiltersFromString(dsl)
		f.Build(a)
		mutate(f, mut)
		return f
	}

	// Sanity: the mutators really do change the rendered output.
	sanityCtx := adapters.RawContext{Table: "users"}
	require.Contains(t, build("id=1", adapters.RawAdapter{}, "handler-acme").GetSqlString(sanityCtx), "tenant_id = 'acme'")
	require.Contains(t, build("id=1", adapters.RawAdapter{}, "handler-globex").GetSqlString(sanityCtx), "tenant_id = 'globex'")
	require.Contains(t, build("id=1", adapters.RawAdapter{}, "ptr").GetSqlString(sanityCtx), "tenant")

	for ai, a := range adapterCfgs {
		for _, dsl := range dsls {
			for ci, ctx := range ctxs {
				for _, kind := range kinds {
					for si, cond := range conds {
						for _, mut := range mutatorNames {
							configs++
							id := fmt.Sprintf("adapter=%d dsl=%q ctx=%d kind=%s cond=%d mut=%s", ai, dsl, ci, kind, si, mut)
							f := build(dsl, a, mut)
							key := generateCacheKey(f, kind, ctx, cond...)
							if key == "" {
								bypassed++
								// Only a live func payload is allowed to be
								// unkeyable here; everything else in this
								// matrix must produce a key.
								if mut != "handler-acme" && mut != "handler-globex" {
									unexpectedBypass++
									if unexpectedBypass <= 5 {
										t.Errorf("UNEXPECTED BYPASS (silent 0%% hit rate) for %s", id)
									}
								}
								continue
							}
							keyed++
							// Direction 2: the SAME configuration, built again
							// (fresh allocations), must produce the same key.
							if k2 := generateCacheKey(build(dsl, a, mut), kind, ctx, cond...); k2 != key {
								nondet++
								if nondet <= 5 {
									t.Errorf("NONDETERMINISTIC key for %s: %s vs %s", id, key, k2)
								}
							}
							sql := ""
							if kind == "sql" {
								sql = f.GetSqlString(ctx, cond...)
							} else {
								sql = fmt.Sprintf("%v", f.GetQuery(ctx, cond...))
							}
							// Direction 1: a shared key is only legitimate when
							// the rendered output is identical — that is what a
							// cache hit serves.
							if prev, ok := byKey[key]; ok {
								if prev.sql != sql {
									wrongServe++
									if wrongServe <= 5 {
										t.Errorf("WRONG SERVE: key %s shared by\n  A=%s -> %q\n  B=%s -> %q",
											key, prev.id, prev.sql, id, sql)
									}
								}
								continue
							}
							byKey[key] = cfg{id: id, sql: sql}
						}
					}
				}
			}
		}
	}
	t.Logf("configurations=%d keyed=%d distinctKeys=%d bypassed=%d wrongServe=%d nondeterministic=%d unexpectedBypass=%d",
		configs, keyed, len(byKey), bypassed, wrongServe, nondet, unexpectedBypass)
	assert.Zero(t, wrongServe)
	assert.Zero(t, nondet)
	assert.Zero(t, unexpectedBypass)
}

// The content walk recurses where %#v did not, so a DAG-shaped payload (the
// cross-area interaction the completeness critic flagged: a value whose slice
// nodes are shared, so a naive walk is exponential in its depth) must be stopped
// by the node budget and fail safe, not hang the render. At ef28331 the %#v of
// the same clause tree took ~195ms at depth 20 and doubled per level; the
// budgeted walk takes microseconds and reports "not cacheable".
func TestH8CACHE2_DagPayloadIsBoundedAndFailsSafe(t *testing.T) {
	var node any = "leaf"
	for i := 0; i < 24; i++ {
		node = []any{node, node} // slice headers are not deduplicated by pointer
	}
	clauses := []figo.Expr{figo.EqExpr{Field: "x", Value: node}}

	var sb strings.Builder
	start := time.Now()
	sound := writeClauseFingerprint(&sb, clauses)
	elapsed := time.Since(start)

	assert.False(t, sound, "a payload that cannot be walked within the budget must not be keyed")
	assert.Less(t, elapsed, 5*time.Second,
		"the clause walk must be bounded by the node budget, not by the payload's shape (took %s)", elapsed)
}

// Nested groups and preloads must stay CACHEABLE: the clause walk is
// depth-bounded, and a bound that truncated an ordinary nested filter would
// trade a wrong serve for a silent 0% hit rate.
func TestH8CACHE2_NestedClausesAndPreloadsStayCacheable(t *testing.T) {
	ctx := adapters.RawContext{Table: "users"}
	for _, dsl := range []string{
		"a=1 and (b=2 or (c=3 and (d=4 or (e=5 and f=6))))",
		"load=[Orders:total>100 and (qty=1 or qty=2)]",
		"not (a=1 and (b=2 or c=3))",
	} {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(dsl))
		f.Build(adapters.RawAdapter{})
		assert.NotEmpty(t, generateCacheKey(f, "sql", ctx), "nested DSL must stay keyable: %s", dsl)
	}
}
