package plugins

import (
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B1: the cache key encoded the naming strategy as the naming func's code
// pointer. Closures created at the same call site (a per-tenant naming-func
// factory) share one code pointer regardless of captured state, so two
// instances differing only in captured naming state collided — a cross-tenant
// cache hit returned the wrong columns (tenant2 was served "SELECT t1_name").
// The key now fingerprints naming BEHAVIOR by probing the func.
func TestCacheKeyNamingBehaviorNotIdentity(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	// Per-tenant factory: every returned closure shares one code pointer.
	mk := func(prefix string) figo.NamingFunc {
		return func(field string) string { return prefix + field }
	}

	build := func(prefix string) figo.Figo {
		f := figo.New()
		require.NoError(t, f.AddFiltersFromString(`name=x`))
		f.SetNamingFunc(mk(prefix))
		f.AddSelectFields("name")
		f.Build(adapters.RawAdapter{})
		return f
	}

	sql1 := cp.GetCachedSqlString(build("t1_"), adapters.RawContext{Table: "t"})
	sql2 := cp.GetCachedSqlString(build("t2_"), adapters.RawContext{Table: "t"})

	assert.Contains(t, sql1, "t1_name")
	assert.Contains(t, sql2, "t2_name",
		"tenant2 must not be served tenant1's cached render (naming closures share a code pointer)")
}

// B2: the adapter key component printed RawAdapter's *SQLDialect field as a
// bare pointer ADDRESS. Reconfiguring the dialect in place kept the old key,
// so a Postgres request was served the stale MySQL-quoted SQL. The key now
// renders the dialect's contents.
func TestCacheKeyDialectContentsNotAddress(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`id=1`))

	d := *adapters.MySQLDialect // private copy, reconfigured in place below
	f.Build(adapters.RawAdapter{Dialect: &d})
	mysqlSQL := cp.GetCachedSqlString(f, adapters.RawContext{Table: "t"})
	assert.Contains(t, mysqlSQL, "`id`")

	d = *adapters.PostgresDialect // same address, new contents
	pgSQL := cp.GetCachedSqlString(f, adapters.RawContext{Table: "t"})
	assert.Contains(t, pgSQL, `"id"`,
		"in-place dialect reconfigure must not be served the stale MySQL-quoted SQL")
}

// B2 (nil handling): a nil dialect must fingerprint without panicking and not
// collide with an explicit dialect.
func TestAdapterFingerprintNilDialect(t *testing.T) {
	assert.NotPanics(t, func() { _ = adapterFingerprint(adapters.RawAdapter{}) })
	assert.NotEqual(t,
		adapterFingerprint(adapters.RawAdapter{}),
		adapterFingerprint(adapters.RawAdapter{Dialect: adapters.PostgresDialect}))
	assert.Equal(t, "<nil>", adapterFingerprint(nil))

	// Two distinct copies with identical contents key identically — that is
	// the point: address reuse across per-request copies must not matter.
	d1 := *adapters.PostgresDialect
	d2 := *adapters.PostgresDialect
	assert.Equal(t,
		adapterFingerprint(adapters.RawAdapter{Dialect: &d1}),
		adapterFingerprint(adapters.RawAdapter{Dialect: &d2}))
}

// B3: key components were joined with "|" while ctx/conditionType are
// %v-rendered and may contain "|"/"["/"]", so boundaries were ambiguous: ctx
// `users|[where]` (no conditionType) collided with ctx `users` +
// conditionType `where]|[`. Components are now length-prefixed.
func TestCacheKeyComponentBoundaries(t *testing.T) {
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`id=1`))
	f.Build(adapters.RawAdapter{})

	k1 := generateCacheKey(f, "sql", "users|[where]")
	k2 := generateCacheKey(f, "sql", "users", "where]|[")
	assert.NotEqual(t, k1, k2,
		"a '|' inside ctx must not shift component boundaries into a collision")
}

// B4: cached Query objects were returned by reference — a caller mutating the
// returned Args slice poisoned every subsequent cache hit. Queries are now
// copied on store and on hit.
func TestCachedQueryArgsAreNotAliased(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(adapters.RawAdapter{})

	q1, ok := cp.GetCachedQuery(f, adapters.RawContext{Table: "t"}).(figo.SQLQuery)
	require.True(t, ok)
	require.NotEmpty(t, q1.Args)
	q1.Args[0] = "poisoned-by-miss-caller"

	q2, ok := cp.GetCachedQuery(f, adapters.RawContext{Table: "t"}).(figo.SQLQuery)
	require.True(t, ok)
	require.NotEmpty(t, q2.Args)
	assert.Equal(t, int64(1), q2.Args[0],
		"mutating the miss-path result must not reach the cached entry")
	q2.Args[0] = "poisoned-by-hit-caller"

	q3, ok := cp.GetCachedQuery(f, adapters.RawContext{Table: "t"}).(figo.SQLQuery)
	require.True(t, ok)
	assert.Equal(t, int64(1), q3.Args[0],
		"mutating a hit-path result must not reach the cached entry")
}

// B5: GetCachedSqlString's monitor recording hardcoded a nil error, so failed
// renders (veto, no adapter, unsupported expr → "") never incremented
// ErrorCount — while GetCachedQuery recorded errRenderFailed for the identical
// condition. Both wrappers now record it.
func TestFailedSqlRenderRecordsError(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	mon := NewPerformanceMonitor(true)
	cp.SetPerformanceMonitor(mon)

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(nil) // no adapter: renders ""

	assert.Equal(t, "", cp.GetCachedSqlString(f, "t"))
	assert.EqualValues(t, 1, mon.GetMetrics().ErrorCount,
		"an empty SQL render must count as an error, matching GetCachedQuery")
}

// B5 (disabled-cache path): the render-failure recording covers the
// cache-bypass branch too.
func TestFailedSqlRenderRecordsErrorWithoutCache(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: false})
	defer cp.Close()
	mon := NewPerformanceMonitor(true)
	cp.SetPerformanceMonitor(mon)

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(nil)

	assert.Equal(t, "", cp.GetCachedSqlString(f, "t"))
	assert.EqualValues(t, 1, mon.GetMetrics().ErrorCount)
}
