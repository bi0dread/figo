package plugins

import (
	"fmt"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type hunt67User struct {
	ID   uint `gorm:"primarykey"`
	Name string
}

type hunt67Order struct {
	ID   uint `gorm:"primarykey"`
	Name string
}

func hunt67DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&hunt67User{}, &hunt67Order{}))
	return db
}

func hunt67Figo(t *testing.T, dsl string, a figo.Adapter) figo.Figo {
	t.Helper()
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(dsl))
	f.Build(a)
	return f
}

// H0: the cache key encoded the render context by POINTER ADDRESS
// (fmt.Sprintf("%v", ctx)). On the GORM path ctx is a *gorm.DB whose fields are
// all pointers, so the model/table and the WHERE scopes never reached the key,
// and GormAdapter{} is an empty struct — ctx was the only per-request
// discriminator. An in-place Model switch was served the previous table's SQL.
// The key is now derived from the context's CONTENTS, and a context that cannot
// be fingerprinted by contents (a *gorm.DB is a graph of thousands of nodes,
// most of it mutable connection state) bypasses the cache instead.
func TestH0_CacheKeyNeverKeysCtxByPointerAddress(t *testing.T) {
	db := hunt67DB(t)
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	f := hunt67Figo(t, `id=1`, adapters.GormAdapter{})

	ctx := db.Session(&gorm.Session{DryRun: true}).Model(&hunt67User{})
	users := cp.GetCachedSqlString(f, ctx)
	ctx2 := ctx.Model(&hunt67Order{}) // same *gorm.DB, different model
	orders := cp.GetCachedSqlString(f, ctx2)

	require.Same(t, ctx, ctx2, "the probe needs the in-place mutation case")
	assert.Contains(t, users, "hunt67_users")
	assert.Contains(t, orders, "hunt67_orders", "a cached entry must never be served for a different table")

	// An unkeyable ctx is not cached at all rather than keyed by address.
	assert.Empty(t, generateCacheKey(f, "sql", ctx), "a *gorm.DB ctx must produce no cache key")
	assert.Equal(t, 0, cp.Stats().Size, "nothing may be cached under an address-keyed context")
}

// H0 (b): a tenant scope added to the ctx with Where was silently dropped —
// the cached, unscoped SQL was served for the scoped request.
func TestH0_TenantScopeOnCtxIsNotDropped(t *testing.T) {
	db := hunt67DB(t)
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()

	f := hunt67Figo(t, `id=1`, adapters.GormAdapter{})
	base := db.Session(&gorm.Session{DryRun: true}).Model(&hunt67User{})
	unscoped := cp.GetCachedSqlString(f, base)
	assert.NotContains(t, unscoped, "tenant_id")

	scoped := cp.GetCachedSqlString(f, base.Where("tenant_id = ?", "A"))
	assert.Contains(t, scoped, "tenant_id", "the ctx's own scope must not be lost to a cache hit")
}

// P3: the key erased the ctx TYPE, so "{users}" (a string) and
// RawContext{Table:"users"} printed identically under %v and shared a slot;
// and it erased the conditionType element boundaries, so []string{"where",
// "select"} collided with []string{"where select"}.
func TestP3_CacheKeyKeepsCtxTypeAndConditionTypeBoundaries(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	f := hunt67Figo(t, `id=1`, adapters.RawAdapter{})

	byString := cp.GetCachedSqlString(f, "{users}")
	byStruct := cp.GetCachedSqlString(f, adapters.RawContext{Table: "users"})
	assert.Equal(t, "SELECT * FROM `{users}` WHERE `id` = 1", byString)
	assert.Equal(t, f.GetSqlString(adapters.RawContext{Table: "users"}), byStruct,
		"a string ctx and a RawContext that print alike must not share a key")

	assert.NotEqual(t,
		generateCacheKey(f, "sql", "t", "where", "select"),
		generateCacheKey(f, "sql", "t", "where select"),
		"conditionType element boundaries must survive into the key")
}

// A3-5: the plugin manager was the one instance field missing from the key, so
// an entry warmed by a plugin-free instance was served to an instance whose
// BeforeQuery authorization hook denies — and the hook never ran at all,
// because hits skip it.
func TestA35_CacheKeyIncludesThePluginSet(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()

	warm := hunt67Figo(t, `id=1`, adapters.RawAdapter{})
	assert.NotEmpty(t, cp.GetCachedSqlString(warm, "users"))

	denied := figo.New()
	veto := &hunt67VetoPlugin{}
	require.NoError(t, denied.RegisterPlugin(veto))
	require.NoError(t, denied.AddFiltersFromString(`id=1`))
	denied.Build(adapters.RawAdapter{})

	assert.Empty(t, cp.GetCachedSqlString(denied, "users"), "a vetoing instance must not be served another instance's SQL")
	assert.Equal(t, 1, veto.calls, "its BeforeQuery hook must actually run")
}

type hunt67VetoPlugin struct{ calls int }

func (v *hunt67VetoPlugin) Name() string               { return "hunt67-veto" }
func (v *hunt67VetoPlugin) Version() string            { return "1.0.0" }
func (v *hunt67VetoPlugin) Initialize(figo.Figo) error { return nil }
func (v *hunt67VetoPlugin) BeforeQuery(figo.Figo, any) error {
	v.calls++
	return fmt.Errorf("denied")
}
func (v *hunt67VetoPlugin) AfterQuery(figo.Figo, any, any) error { return nil }
func (v *hunt67VetoPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	return dsl, nil
}
func (v *hunt67VetoPlugin) AfterParse(figo.Figo, string) error { return nil }

// A3-4: a segment that renders as the empty string SUCCESSFULLY (ORDERBY on a
// query with no sort=) was treated as a failed render: one ErrorCount per call
// for a healthy query, and the segment was never cached — while GetCachedQuery
// cached the identical state happily.
func TestA34_EmptyButSuccessfulSegmentIsCachedAndNotAnError(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	monitor := NewPerformanceMonitor(true)
	cp.SetPerformanceMonitor(monitor)

	f := hunt67Figo(t, `id=1 and name="x"`, adapters.RawAdapter{})
	ctx := adapters.RawContext{Table: "users"}
	sql, ok := adapters.RawAdapter{}.GetSqlString(f, ctx, "ORDERBY")
	require.True(t, ok, "the ORDERBY segment renders successfully")
	require.Empty(t, sql, "...and it renders empty")

	for i := 0; i < 5; i++ {
		assert.Empty(t, cp.GetCachedSqlString(f, ctx, "ORDERBY"))
	}
	stats := cp.Stats()
	assert.Equal(t, int64(4), stats.Hits, "an empty-but-successful render must be cacheable")
	assert.Equal(t, 1, stats.Size)
	assert.Zero(t, monitor.GetMetrics().ErrorCount, "a successful render must not be reported as an error")
}

// A3-4 (control): a render that actually FAILED is still never cached, so a
// transient BeforeQuery veto cannot pin "" for the whole TTL.
func TestA34_VetoedRenderIsStillNotCached(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	monitor := NewPerformanceMonitor(true)
	cp.SetPerformanceMonitor(monitor)

	f := figo.New()
	veto := &hunt67TransientVeto{deny: true}
	require.NoError(t, f.RegisterPlugin(veto))
	require.NoError(t, f.AddFiltersFromString(`id=1`))
	f.Build(adapters.RawAdapter{})

	assert.Empty(t, cp.GetCachedSqlString(f, "users"))
	assert.Equal(t, 0, cp.Stats().Size, "a vetoed render must not be cached")
	assert.Equal(t, int64(1), monitor.GetMetrics().ErrorCount)

	veto.deny = false // the veto lifts
	assert.NotEmpty(t, cp.GetCachedSqlString(f, "users"), "the lifted veto must not be masked by a cached \"\"")
}

type hunt67TransientVeto struct{ deny bool }

func (v *hunt67TransientVeto) Name() string               { return "hunt67-transient" }
func (v *hunt67TransientVeto) Version() string            { return "1.0.0" }
func (v *hunt67TransientVeto) Initialize(figo.Figo) error { return nil }
func (v *hunt67TransientVeto) BeforeQuery(figo.Figo, any) error {
	if v.deny {
		return fmt.Errorf("denied")
	}
	return nil
}
func (v *hunt67TransientVeto) AfterQuery(figo.Figo, any, any) error { return nil }
func (v *hunt67TransientVeto) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	return dsl, nil
}
func (v *hunt67TransientVeto) AfterParse(figo.Figo, string) error { return nil }

// A3-1: with no CleanupInterval, TTL expiry freed nothing — Get only drops the
// key it was handed, and a key space driven by untrusted filter strings is
// never requested twice. The map grew for the life of the process while Stats()
// reported the cache as empty.
func TestA31_ExpiredEntriesAreReclaimedWithoutACleanupGoroutine(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, TTL: 20 * time.Millisecond}) // no CleanupInterval
	defer c.Close()

	const n = 2000
	for i := 0; i < n; i++ {
		c.Set(fmt.Sprintf("unique-key-%d", i), "payload", 20*time.Millisecond)
	}
	c.mu.RLock()
	full := len(c.entries)
	c.mu.RUnlock()
	require.Equal(t, n, full)

	time.Sleep(60 * time.Millisecond)
	// Nothing touches the expired keys again — an attacker's key space never is.
	for i := 0; i < 10; i++ {
		c.Get(fmt.Sprintf("absent-%d", i))
	}
	stats := c.Stats()
	assert.Equal(t, 0, stats.Size)
	assert.Zero(t, stats.MemoryUsage)
	c.mu.RLock()
	retained := len(c.entries)
	c.mu.RUnlock()
	assert.Zero(t, retained, "expired entries must be freed, not merely hidden from Stats()")
}

// A3-1 (b): the reclaim also happens on the write path, so a long-running
// unique-key workload does not grow without bound between Stats() calls.
func TestA31_WritesReclaimExpiredEntries(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, TTL: 10 * time.Millisecond})
	defer c.Close()

	for i := 0; i < 500; i++ {
		c.Set(fmt.Sprintf("first-%d", i), "payload", 10*time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	for i := 0; i < 500; i++ {
		c.Set(fmt.Sprintf("second-%d", i), "payload", time.Hour)
	}
	c.mu.RLock()
	retained := len(c.entries)
	c.mu.RUnlock()
	assert.LessOrEqual(t, retained, 500+32,
		"the expired first generation must be reaped by the writes of the second")
}

// A3-3: Stats() walked every entry while holding the lock, so a metrics loop
// over a large cache stalled every Get. Size and MemoryUsage are now maintained
// incrementally.
func TestA33_StatsDoesNotScanTheWholeCache(t *testing.T) {
	measure := func(n int) time.Duration {
		c := NewInMemoryCache(CacheConfig{Enabled: true, TTL: time.Hour})
		defer c.Close()
		for i := 0; i < n; i++ {
			c.Set(fmt.Sprintf("k%d", i), "v", time.Hour)
		}
		start := time.Now()
		for i := 0; i < 100; i++ {
			c.Stats()
		}
		return time.Since(start)
	}
	small := measure(1000)
	large := measure(100000)
	assert.Equal(t, 100000, func() int {
		c := NewInMemoryCache(CacheConfig{Enabled: true, TTL: time.Hour})
		defer c.Close()
		for i := 0; i < 100000; i++ {
			c.Set(fmt.Sprintf("k%d", i), "v", time.Hour)
		}
		return c.Stats().Size
	}(), "Size must still be exact")
	assert.Less(t, large, 10*small+time.Millisecond,
		"Stats() must not scale with the entry count (small=%v large=%v)", small, large)
}

// A3-6 / A7-4: evictLRU scanned the entire map on every insert at capacity,
// under the exclusive lock — so configuring a BIGGER cache made every
// cache-missing request slower, at the mercy of whoever sends the filters.
func TestA36_EvictionDoesNotScaleWithMaxSize(t *testing.T) {
	measure := func(maxSize int) time.Duration {
		c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: maxSize, TTL: time.Hour})
		defer c.Close()
		for i := 0; i < maxSize; i++ {
			c.Set(fmt.Sprintf("fill%d", i), i, time.Hour)
		}
		start := time.Now()
		for i := 0; i < 1000; i++ { // all-new keys: every insert evicts
			c.Set(fmt.Sprintf("fresh%d", i), i, time.Hour)
		}
		return time.Since(start)
	}
	small := measure(2000)
	large := measure(50000)
	assert.Less(t, large, 5*small+time.Millisecond,
		"eviction cost must not scale with MaxSize (MaxSize=2000: %v, MaxSize=50000: %v)", small, large)
}

// A3-6 (invariant): the recency list and the expiry heap that replaced the
// per-insert map scan must stay consistent with the map under a mixed
// workload of inserts, overwrites, hits, deletes and expiries.
func TestA36_LRUAndExpiryIndexesStayConsistent(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 64})
	defer c.Close()

	for i := 0; i < 5000; i++ {
		key := fmt.Sprintf("k%d", i%200)
		switch i % 4 {
		case 0:
			c.Set(key, i, time.Hour)
		case 1:
			c.Set(key, i, 5*time.Millisecond)
		case 2:
			c.Get(key)
		case 3:
			c.Delete(key)
		}
	}
	time.Sleep(10 * time.Millisecond)
	c.Set("final", 1, time.Hour)

	c.mu.RLock()
	defer c.mu.RUnlock()
	forward := 0
	var last *CacheEntry
	for e := c.mru; e != nil; e = e.next {
		forward++
		last = e
		_, inMap := c.entries[e.key]
		require.True(t, inMap, "a listed entry must be in the map")
		require.LessOrEqual(t, forward, len(c.entries)+1, "the recency list must not cycle")
	}
	backward := 0
	for e := c.lru; e != nil; e = e.prev {
		backward++
	}
	assert.Equal(t, len(c.entries), forward, "every entry must be on the recency list")
	assert.Equal(t, len(c.entries), backward, "the list must be intact in both directions")
	assert.Equal(t, c.lru, last, "the tail pointer must be the last node")
	assert.LessOrEqual(t, len(c.entries), 64, "MaxSize must hold")
	for i, e := range c.expiry {
		assert.Equal(t, i, e.heapIdx, "heap indexes must stay in sync")
		_, inMap := c.entries[e.key]
		assert.True(t, inMap, "a queued entry must be in the map")
	}
}

// A3-7: evictLRU used the zero value of the key as its "nothing chosen yet"
// sentinel, so an empty-string key either shielded the real victim (an
// arbitrary, possibly most-recently-used entry was evicted instead) or skipped
// the eviction entirely and let the cache grow past MaxSize without bound.
func TestA37_EmptyStringKeyDoesNotDefeatEviction(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 2})
	defer c.Close()
	c.Set("", 0, time.Minute) // least recently used: the correct first victim
	c.Set("k1", 1, time.Minute)
	c.Set("k2", 2, time.Minute)
	c.Set("k3", 3, time.Minute)

	present := func(k string) bool { _, ok := c.Get(k); return ok }
	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()
	assert.Equal(t, 2, size, "MaxSize must be enforced")
	assert.False(t, present(""), "the least recently used entry is the victim, whatever its key")
	assert.True(t, present("k2"))
	assert.True(t, present("k3"))

	c2 := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 1})
	defer c2.Close()
	c2.Set("", 0, time.Minute)
	for i := 0; i < 1000; i++ {
		c2.Set(fmt.Sprintf("k%d", i), i, time.Minute)
	}
	assert.Equal(t, 1, c2.Stats().Size, "the cache must never exceed MaxSize")
}

// A3-9: an injected cache whose own config omitted Enabled was permanently
// write-dead — every Set a silent no-op while Get kept counting misses, the
// plugin reporting Enabled=true and nothing ever erroring.
func TestA39_InjectedCacheWithoutEnabledStillStores(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute})
	defer cp.Close()
	cp.SetCache(NewInMemoryCache(CacheConfig{MaxSize: 1000, TTL: time.Minute})) // Enabled unset

	f := hunt67Figo(t, `id=1`, adapters.RawAdapter{})
	for i := 0; i < 5; i++ {
		assert.NotEmpty(t, cp.GetCachedSqlString(f, "users"))
	}
	stats := cp.Stats()
	assert.Equal(t, int64(4), stats.Hits, "the injected cache's own Enabled flag must not silently disable writes")
	assert.Equal(t, 1, stats.Size)
}
