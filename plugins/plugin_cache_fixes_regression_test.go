package plugins

import (
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cache-key value-type signature covered the core expr types but not the
// advanced ones, so two instances differing only in a CustomExpr value's
// numeric type (int64(1) vs float64(1) — identical under %#v) shared a slot:
// the second caller was served the first caller's cached query.
func TestCacheKeyDistinguishesAdvancedExprValueTypes(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()

	newCustom := func(v any) figo.Figo {
		f := figo.New()
		f.AddFilter(figo.CustomExpr{
			Field:    "score",
			Operator: "=",
			Value:    v,
			Handler: func(field, operator string, value any) (string, []any, error) {
				return "`score` = ?", []any{value}, nil
			},
		})
		f.Build(adapters.RawAdapter{})
		return f
	}

	q1, ok := cp.GetCachedQuery(newCustom(int64(1)), "t").(figo.SQLQuery)
	require.True(t, ok)
	q2, ok := cp.GetCachedQuery(newCustom(float64(1)), "t").(figo.SQLQuery)
	require.True(t, ok)

	assert.IsType(t, int64(0), q1.Args[0])
	assert.IsType(t, float64(0), q2.Args[0], "float64 render must not be served the cached int64 entry")
}

// An empty SQL render (hook veto, no adapter) was cached and served as a hit
// for the whole TTL — a transient authorization veto poisoned the slot.
func TestEmptySqlRenderIsNotCached(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(nil) // no adapter: renders ""

	assert.Equal(t, "", cp.GetCachedSqlString(f, "t"))
	assert.Equal(t, "", cp.GetCachedSqlString(f, "t"))
	stats := cp.Stats()
	assert.Zero(t, stats.Hits, "the empty render must not be served as a hit")

	// Once the render works (adapter set), the same key caches normally.
	f.Build(adapters.RawAdapter{})
	first := cp.GetCachedSqlString(f, "t")
	require.NotEmpty(t, first)
	assert.Equal(t, first, cp.GetCachedSqlString(f, "t"))
	assert.NotZero(t, cp.Stats().Hits)
}

// SetConfig only stored the plugin-level config; an owned cache silently kept
// its original MaxSize/CleanupInterval forever.
func TestSetConfigReconfiguresOwnedCache(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	cp.SetConfig(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 2})

	cache := cp.GetCache()
	require.NotNil(t, cache)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		cache.Set(k, "v", time.Minute)
	}
	assert.LessOrEqual(t, cache.Stats().Size, 2, "the reconfigured MaxSize must be enforced")
}

// TTL 0 used to store entries pre-expired: an enabled cache with no explicit
// TTL could never produce a hit. TTL <= 0 now means "never expires".
func TestZeroTTLMeansNoExpiry(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true}) // no TTL
	defer cp.Close()

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(adapters.RawAdapter{})

	first := cp.GetCachedSqlString(f, "t")
	require.NotEmpty(t, first)
	assert.Equal(t, first, cp.GetCachedSqlString(f, "t"))
	stats := cp.Stats()
	assert.EqualValues(t, 1, stats.Hits, "second render must hit (entries without TTL never expire)")
}

// Stats().Size counts only live entries; expired-but-untouched entries used
// to inflate it when no periodic cleanup ran.
func TestStatsSizeExcludesExpiredEntries(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true})
	defer c.Stop()

	c.Set("live", "v", time.Minute)
	c.Set("dead", "v", time.Nanosecond)
	time.Sleep(5 * time.Millisecond)

	assert.Equal(t, 1, c.Stats().Size)
}

// A nil query render records into the monitor's ErrorCount (it was
// structurally impossible for the cache path to ever count an error).
func TestFailedQueryRenderRecordsError(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	mon := NewPerformanceMonitor(true)
	cp.SetPerformanceMonitor(mon)

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`a=1`))
	f.Build(nil) // no adapter: GetQuery returns nil

	assert.Nil(t, cp.GetCachedQuery(f, "t"))
	assert.EqualValues(t, 1, mon.GetMetrics().ErrorCount)
}
