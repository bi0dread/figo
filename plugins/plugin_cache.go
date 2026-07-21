package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"crypto/md5"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// figo.Query caching is provided as a plugin rather than core figo state: create a
// CachePlugin and render through it (GetCachedSqlString/GetCachedQuery) to get
// cached results, or use any QueryCache implementation standalone.

// CacheConfig defines caching configuration.
//
// TTL <= 0 means entries never expire (previously a zero TTL stored every
// entry pre-expired, so an enabled cache without an explicit TTL never hit).
// MaxSize <= 0 means unlimited; CleanupInterval <= 0 disables the periodic
// sweep (expired entries are still dropped lazily on Get).
type CacheConfig struct {
	Enabled         bool
	TTL             time.Duration
	MaxSize         int
	CleanupInterval time.Duration
}

// CacheEntry represents a cached query result
type CacheEntry struct {
	Data           interface{}
	ExpiresAt      time.Time
	CreatedAt      time.Time
	LastAccessedAt time.Time // updated on each Get; drives LRU eviction
	HitCount       int64
}

// QueryCache interface for different cache implementations
type QueryCache interface {
	Get(key string) (interface{}, bool)
	Set(key string, value interface{}, ttl time.Duration)
	Delete(key string)
	Clear()
	Stats() CacheStats
}

// CacheStats provides cache performance metrics
type CacheStats struct {
	Hits        int64
	Misses      int64
	Size        int
	HitRate     float64
	MemoryUsage int64
}

// CachePlugin caches rendered SQL/query results keyed by the full state of the
// figo.Figo instance passed to it. It implements Plugin so it can be registered
// alongside other plugins, but its real API is the pair of render wrappers:
//
//	cp := plugins.NewCachePlugin(plugins.CacheConfig{Enabled: true, TTL: time.Minute})
//	sql := cp.GetCachedSqlString(f, ctx)
//	q := cp.GetCachedQuery(f, ctx)
//
// One plugin may serve many figo.Figo instances — the cache key covers everything
// that changes the rendered output, so instances never collide.
type CachePlugin struct {
	mu      sync.RWMutex
	cache   QueryCache
	owned   *InMemoryCache // cache created by SetConfig, stopped on replacement/GC
	config  CacheConfig
	monitor *PerformanceMonitor // optional; records hits/misses when set
}

// NewCachePlugin creates a cache plugin. When config.Enabled is true and no
// cache is injected via SetCache, an InMemoryCache is created and owned by the
// plugin (its cleanup goroutine is stopped when the plugin is replaced,
// Closed, or garbage-collected).
func NewCachePlugin(config CacheConfig) *CachePlugin {
	p := &CachePlugin{}
	p.SetConfig(config)
	return p
}

// Name implements Plugin
func (p *CachePlugin) Name() string { return "figo-cache" }

// Version implements Plugin
func (p *CachePlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *CachePlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *CachePlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *CachePlugin) AfterQuery(figo.Figo, any, any) error { return nil }

// BeforeParse implements Plugin
func (p *CachePlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse implements Plugin
func (p *CachePlugin) AfterParse(figo.Figo, string) error { return nil }

// SetCache injects a cache implementation, replacing (and stopping) any cache
// the plugin created itself.
func (p *CachePlugin) SetCache(cache QueryCache) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopOwnedLocked()
	p.cache = cache
}

// stopOwnedLocked stops the cleanup goroutine of a cache the plugin created
// itself (via SetConfig) when that cache is being replaced. Caller holds p.mu.
func (p *CachePlugin) stopOwnedLocked() {
	if p.owned != nil && QueryCache(p.owned) == p.cache {
		p.owned.Stop()
	}
	p.owned = nil
}

// GetCache returns the current cache instance
func (p *CachePlugin) GetCache() QueryCache {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cache
}

// SetConfig sets the cache configuration. When enabled and no cache has been
// injected, an owned InMemoryCache is created — and when an owned cache
// already exists, it is RECREATED with the new settings (dropping its
// entries): MaxSize and CleanupInterval live inside the cache, and the old
// behavior of only storing p.config meant an owned cache silently kept its
// original limits forever. An injected cache (SetCache) is caller-managed;
// only the plugin-level settings (Enabled, TTL) apply to it.
func (p *CachePlugin) SetConfig(config CacheConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = config

	ownedCurrent := p.owned != nil && QueryCache(p.owned) == p.cache

	if !config.Enabled {
		// Disabling releases an owned cache (and its cleanup goroutine).
		if ownedCurrent {
			p.owned.Stop()
			p.owned = nil
			p.cache = nil
		}
		return
	}

	if p.cache != nil && !ownedCurrent {
		return // injected cache: not ours to reconfigure
	}

	if ownedCurrent {
		if p.owned.config == config {
			return // no-op reconfigure keeps the warm cache
		}
		p.owned.Stop()
	}

	owned := NewInMemoryCache(config)
	p.cache = owned
	p.owned = owned
	// The cleanup goroutine keeps the cache alive forever; stop it when
	// this plugin is garbage-collected so plugins created per request
	// don't accumulate goroutines. Clear any previous registration first —
	// SetFinalizer fatals on double-set, and reconfiguring an owned cache
	// passes through here more than once.
	runtime.SetFinalizer(p, nil)
	runtime.SetFinalizer(p, func(pp *CachePlugin) {
		if pp.owned != nil {
			pp.owned.Stop()
		}
	})
}

// GetConfig returns the current cache configuration
func (p *CachePlugin) GetConfig() CacheConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config
}

// SetPerformanceMonitor attaches a monitor; every GetCachedSqlString /
// GetCachedQuery call then records its latency and hit/miss outcome into it.
// A MetricsPlugin's embedded monitor works here too.
func (p *CachePlugin) SetPerformanceMonitor(monitor *PerformanceMonitor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor = monitor
}

// GetPerformanceMonitor returns the attached monitor (nil if none)
func (p *CachePlugin) GetPerformanceMonitor() *PerformanceMonitor {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.monitor
}

// Stats returns cache performance statistics
func (p *CachePlugin) Stats() CacheStats {
	p.mu.RLock()
	cache := p.cache
	p.mu.RUnlock()
	if cache == nil {
		return CacheStats{}
	}
	return cache.Stats()
}

// Clear removes all cached entries
func (p *CachePlugin) Clear() {
	p.mu.RLock()
	cache := p.cache
	p.mu.RUnlock()
	if cache != nil {
		cache.Clear()
	}
}

// Close stops the plugin's owned cache (if any). Injected caches are the
// caller's to manage.
func (p *CachePlugin) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopOwnedLocked()
	p.cache = nil
}

// errRenderFailed is recorded into the performance monitor when a cached
// render wrapper's underlying render produced no result (vetoed by a
// BeforeQuery hook, no adapter configured, or an unsupported expression).
var errRenderFailed = errors.New("figo: render returned no result (vetoed, no adapter, or unsupported expression)")

// GetCachedSqlString retrieves the SQL string from cache or renders it via
// f.GetSqlString. Hits and misses are recorded into the plugin's performance
// monitor when one is attached.
func (p *CachePlugin) GetCachedSqlString(f figo.Figo, ctx any, conditionType ...string) string {
	start := time.Now()
	var cacheHit bool

	p.mu.RLock()
	cache := p.cache
	enabled := p.config.Enabled
	ttl := p.config.TTL
	monitor := p.monitor
	p.mu.RUnlock()

	defer func() {
		if monitor != nil {
			monitor.RecordQuery(time.Since(start), cacheHit, nil)
		}
	}()

	if cache == nil || !enabled {
		return f.GetSqlString(ctx, conditionType...)
	}

	key := generateCacheKey(f, "sql", ctx, conditionType...)

	// Try to get from cache
	if cached, found := cache.Get(key); found {
		if sql, ok := cached.(string); ok {
			cacheHit = true
			return sql
		}
	}

	// Generate and cache. The key was computed before rendering; if a
	// concurrent Build/AddFiltersFromString changed the state in between, the
	// rendered SQL belongs to the NEW state and storing it under the OLD key
	// would poison the cache — verify and skip caching.
	//
	// An empty render is never cached either: "" means the render failed
	// (hook veto, no adapter, unsupported expression) or the requested
	// segment is empty. Caching it served the empty string as a hit for the
	// whole TTL — even after a transient veto lifted.
	sql := f.GetSqlString(ctx, conditionType...)
	if sql != "" && generateCacheKey(f, "sql", ctx, conditionType...) == key {
		cache.Set(key, sql, ttl)
	}

	return sql
}

// GetCachedQuery retrieves the query from cache or renders it via f.GetQuery.
// Hits and misses are recorded into the plugin's performance monitor when one
// is attached.
func (p *CachePlugin) GetCachedQuery(f figo.Figo, ctx any, conditionType ...string) figo.Query {
	start := time.Now()
	var cacheHit bool
	var renderErr error

	p.mu.RLock()
	cache := p.cache
	enabled := p.config.Enabled
	ttl := p.config.TTL
	monitor := p.monitor
	p.mu.RUnlock()

	defer func() {
		if monitor != nil {
			monitor.RecordQuery(time.Since(start), cacheHit, renderErr)
		}
	}()

	if cache == nil || !enabled {
		q := f.GetQuery(ctx, conditionType...)
		if q == nil {
			renderErr = errRenderFailed
		}
		return q
	}

	key := generateCacheKey(f, "query", ctx, conditionType...)

	// Try to get from cache
	if cached, found := cache.Get(key); found {
		if query, ok := cached.(figo.Query); ok {
			cacheHit = true
			return query
		}
	}

	// Generate and cache (see GetCachedSqlString for the key re-check).
	query := f.GetQuery(ctx, conditionType...)
	if query == nil {
		// A failed render is observable in the monitor's ErrorCount instead
		// of counting as an ordinary miss.
		renderErr = errRenderFailed
	}
	if query != nil && generateCacheKey(f, "query", ctx, conditionType...) == key {
		cache.Set(key, query, ttl)
	}

	return query
}

// generateCacheKey creates a unique cache key for a query. kind ("sql"/"query")
// keeps the two result types in separate slots — otherwise GetCachedSqlString and
// GetCachedQuery share a key and clobber each other's entries. The key covers
// everything that changes the rendered output: the adapter type, the custom
// naming func, and the global regex SQL operator. It reads the instance through
// its public getters, so the snapshot is not atomic under concurrent mutation —
// the key re-check in the render wrappers guards against caching a torn result.
func generateCacheKey(f figo.Figo, kind string, ctx any, conditionType ...string) string {
	clauses := f.GetClauses()
	preloads := f.GetPreloads()

	components := []string{
		kind,
		f.GetDSL(),
		// %#v keeps value types in the key: a = int64(1) and a = "1" render
		// different SQL and must not share a cache slot (%v printed both as
		// "1", colliding when instances share one cache). %#v alone is not
		// enough for numeric types — it renders int(1), int64(1) and float64(1)
		// identically as "1" — so append an explicit type signature of every
		// clause/preload value to keep those from colliding.
		fmt.Sprintf("%#v", clauses),
		valueTypeSignature(clauses),
		fmt.Sprintf("%#v", preloads),
		preloadValueTypeSignature(preloads),
		fmt.Sprintf("%v", f.GetPage()),
		fmt.Sprintf("%v", f.GetSort()),
		// Ignore/whitelist policy needs no key component: FieldsPlugin prunes
		// expressions before they enter the clause tree, so the clauses
		// component above already reflects it.
		fmt.Sprintf("%v", f.GetSelectFields()),
		fmt.Sprintf("%p", f.GetNamingFunc()),
		// %+v (not just %T): adapter configuration changes the rendered SQL —
		// e.g. RawAdapter{Dialect: PostgresDialect} vs the MySQL default must
		// not share a cache slot.
		fmt.Sprintf("%T%+v", f.GetAdapterObject(), f.GetAdapterObject()),
		figo.GetRegexSQLOperator(),
		fmt.Sprintf("%v", ctx),
		fmt.Sprintf("%v", conditionType),
	}

	content := strings.Join(components, "|")
	hash := md5.Sum([]byte(content))
	return fmt.Sprintf("figo:%x", hash)
}

// valueTypeSignature renders the Go type of every value carried by the given
// expression trees, in order. It exists because the cache key's %#v encoding
// collapses int(1)/int64(1)/float64(1) to the same text; appending this makes
// two clauses that differ only in a value's numeric type produce distinct keys.
func valueTypeSignature(exprs []figo.Expr) string {
	var b strings.Builder
	for _, e := range exprs {
		appendValueTypes(&b, e)
		b.WriteByte('|')
	}
	return b.String()
}

// preloadValueTypeSignature does the same across all preload expression lists,
// keyed by relation name so two preloads can't swap type signatures unnoticed.
func preloadValueTypeSignature(preloads map[string][]figo.Expr) string {
	var b strings.Builder
	for _, name := range sortedKeys2(preloads) {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(valueTypeSignature(preloads[name]))
	}
	return b.String()
}

// sortedKeys2 returns the map keys in deterministic order (the key must be
// stable regardless of Go's random map iteration).
func sortedKeys2(m map[string][]figo.Expr) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func appendValueTypes(b *strings.Builder, e figo.Expr) {
	writeT := func(v any) { b.WriteString(fmt.Sprintf("%T,", v)) }
	writeList := func(vs []any) {
		for _, v := range vs {
			writeT(v)
		}
	}
	switch x := e.(type) {
	case figo.EqExpr:
		writeT(x.Value)
	case figo.NeqExpr:
		writeT(x.Value)
	case figo.GtExpr:
		writeT(x.Value)
	case figo.GteExpr:
		writeT(x.Value)
	case figo.LtExpr:
		writeT(x.Value)
	case figo.LteExpr:
		writeT(x.Value)
	case figo.LikeExpr:
		writeT(x.Value)
	case figo.ILikeExpr:
		writeT(x.Value)
	case figo.RegexExpr:
		writeT(x.Value)
	case figo.InExpr:
		writeList(x.Values)
	case figo.NotInExpr:
		writeList(x.Values)
	case figo.BetweenExpr:
		writeT(x.Low)
		writeT(x.High)
	// The advanced expr types carry `any` values too and were missing here,
	// so int64(1) vs float64(1) in a CustomExpr/JsonPathExpr collided into
	// one cache slot — the exact collapse this signature exists to prevent.
	case figo.JsonPathExpr:
		writeT(x.Value)
	case figo.ArrayContainsExpr:
		writeList(x.Values)
	case figo.ArrayOverlapsExpr:
		writeList(x.Values)
	case figo.FullTextSearchExpr:
		writeT(x.Query)
	case figo.CustomExpr:
		writeT(x.Value)
	case figo.AndExpr:
		for _, op := range x.Operands {
			appendValueTypes(b, op)
		}
	case figo.OrExpr:
		for _, op := range x.Operands {
			appendValueTypes(b, op)
		}
	case figo.NotExpr:
		for _, op := range x.Operands {
			appendValueTypes(b, op)
		}
	}
}

// InMemoryCache implements QueryCache using in-memory storage
type InMemoryCache struct {
	mu       sync.RWMutex
	entries  map[string]*CacheEntry
	config   CacheConfig
	hits     int64
	misses   int64
	stopChan chan struct{}
	stopOnce sync.Once // guards stopChan against double-close
}

// NewInMemoryCache creates a new in-memory cache instance
func NewInMemoryCache(config CacheConfig) *InMemoryCache {
	cache := &InMemoryCache{
		entries:  make(map[string]*CacheEntry),
		config:   config,
		stopChan: make(chan struct{}),
	}

	if config.Enabled && config.CleanupInterval > 0 {
		go cache.cleanup()
	}

	return cache
}

// Close properly stops the cache and cleans up resources
func (c *InMemoryCache) Close() {
	c.Stop()
}

// Get retrieves a value from the cache. It takes a full write lock because it
// mutates hit/miss counters, HitCount, and LastAccessedAt — doing this under a
// read lock (as before) is a data race across concurrent Get callers.
func (c *InMemoryCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[key]
	if !exists {
		c.misses++
		return nil, false
	}

	// Check if expired (a zero ExpiresAt never expires: TTL <= 0). Delete it
	// here so expired entries don't linger (and inflate Size) when no
	// periodic cleanup runs — e.g. CleanupInterval <= 0.
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		delete(c.entries, key)
		c.misses++
		return nil, false
	}

	entry.HitCount++
	entry.LastAccessedAt = time.Now()
	c.hits++
	return entry.Data, true
}

// Set stores a value in the cache
func (c *InMemoryCache) Set(key string, value interface{}, ttl time.Duration) {
	if !c.config.Enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check size limit. MaxSize <= 0 means unlimited; without this guard a
	// zero MaxSize would evict on every Set, pinning the cache to one entry.
	// Overwriting an existing key doesn't grow the map, so it must never
	// evict an unrelated entry.
	if _, exists := c.entries[key]; !exists && c.config.MaxSize > 0 && len(c.entries) >= c.config.MaxSize {
		c.evictLRU()
	}

	now := time.Now()
	entry := &CacheEntry{
		Data:           value,
		CreatedAt:      now,
		LastAccessedAt: now,
		HitCount:       0,
	}
	// TTL <= 0 means "never expires" (zero ExpiresAt). Storing now.Add(0)
	// created entries that were already expired on arrival, so an enabled
	// cache with no explicit TTL could never produce a hit.
	if ttl > 0 {
		entry.ExpiresAt = now.Add(ttl)
	}
	c.entries[key] = entry
}

// Delete removes a value from the cache
func (c *InMemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Clear removes all entries from the cache
func (c *InMemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CacheEntry)
}

// Stats returns cache performance statistics. Size counts only LIVE entries:
// with no periodic cleanup, expired entries linger in the map until a Get
// touches them, and counting those reported a fuller cache than callers can
// ever hit. MemoryUsage stays a rough estimate (keys + string payloads + a
// fixed per-entry overhead), not an exact accounting.
func (c *InMemoryCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}

	now := time.Now()
	size := 0
	var mem int64
	for key, entry := range c.entries {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		size++
		mem += int64(len(key)) + 64 // fixed bookkeeping estimate per entry
		if s, ok := entry.Data.(string); ok {
			mem += int64(len(s))
		} else {
			mem += 100 // opaque non-string payload estimate
		}
	}

	return CacheStats{
		Hits:        c.hits,
		Misses:      c.misses,
		Size:        size,
		HitRate:     hitRate,
		MemoryUsage: mem,
	}
}

// evictLRU removes the least recently used entry (by last access time, not
// creation time — otherwise a hot early entry would be evicted before a cold
// later one, i.e. FIFO rather than LRU).
func (c *InMemoryCache) evictLRU() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.entries {
		if oldestKey == "" || entry.LastAccessedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.LastAccessedAt
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// cleanup removes expired entries periodically
func (c *InMemoryCache) cleanup() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanupExpired()
		case <-c.stopChan:
			return
		}
	}
}

// cleanupExpired removes expired entries
func (c *InMemoryCache) cleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.entries {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(c.entries, key)
		}
	}
}

// Stop stops the cleanup goroutine. Safe to call multiple times (and alongside
// Close, which delegates here) — the underlying channel is closed at most once.
func (c *InMemoryCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
}
