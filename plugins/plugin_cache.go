package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"crypto/md5"
	"errors"
	"fmt"
	"reflect"
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
		sql := f.GetSqlString(ctx, conditionType...)
		if sql == "" {
			renderErr = errRenderFailed
		}
		return sql
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
	if sql == "" {
		// A failed render is observable in the monitor's ErrorCount instead
		// of counting as an ordinary miss (mirrors GetCachedQuery).
		renderErr = errRenderFailed
	}
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

	// Try to get from cache. Hits are served as a defensive copy — the cached
	// entry is shared across callers, and one caller mutating the returned
	// Args slice must not poison every subsequent hit.
	if cached, found := cache.Get(key); found {
		if query, ok := cached.(figo.Query); ok {
			cacheHit = true
			return copyQuery(query)
		}
	}

	// Generate and cache (see GetCachedSqlString for the key re-check). A
	// copy is stored so the caller mutating the query returned from THIS
	// call can't reach into the cache either.
	query := f.GetQuery(ctx, conditionType...)
	if query == nil {
		// A failed render is observable in the monitor's ErrorCount instead
		// of counting as an ordinary miss.
		renderErr = errRenderFailed
	}
	if query != nil && generateCacheKey(f, "query", ctx, conditionType...) == key {
		cache.Set(key, copyQuery(query), ttl)
	}

	return query
}

// copyQuery returns a defensive copy of q for cache storage and hit serving.
// figo.SQLQuery (the only Query shape core figo defines) carries a mutable
// Args slice; returning the cached instance by reference let a caller's
// `q.Args[0] = x` rewrite the cached entry for everyone. Adapter-defined
// Query types (Mongo/Elasticsearch results) pass through as-is — this package
// can't know their shape, so their callers must treat cached results as
// read-only.
func copyQuery(q figo.Query) figo.Query {
	switch v := q.(type) {
	case figo.SQLQuery:
		v.Args = copyArgsSlice(v.Args)
		return v
	case *figo.SQLQuery:
		if v == nil {
			return v
		}
		cp := *v
		cp.Args = copyArgsSlice(cp.Args)
		return &cp
	default:
		return q
	}
}

// copyArgsSlice shallow-copies a query args slice (nil stays nil).
func copyArgsSlice(args []any) []any {
	if args == nil {
		return nil
	}
	out := make([]any, len(args))
	copy(out, args)
	return out
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
		namingFingerprint(f),
		// Contents, not %T%+v: adapter configuration changes the rendered
		// SQL — e.g. RawAdapter{Dialect: PostgresDialect} vs the MySQL
		// default must not share a cache slot — and %+v printed pointer
		// fields (like *SQLDialect) as bare addresses, keying on WHERE the
		// config lived instead of what it said.
		adapterFingerprint(f.GetAdapterObject()),
		figo.GetRegexSQLOperator(),
		fmt.Sprintf("%v", ctx),
		fmt.Sprintf("%v", conditionType),
	}

	// Length-prefix every component before joining: a plain "|" join let a
	// "|" inside a %v-rendered component shift the boundaries, so ctx
	// `users|[where]` with no conditionType collided with ctx `users` plus
	// conditionType `where]|[`.
	var content strings.Builder
	for _, c := range components {
		fmt.Fprintf(&content, "%d:%s|", len(c), c)
	}
	hash := md5.Sum([]byte(content.String()))
	return fmt.Sprintf("figo:%x", hash)
}

// namingFingerprint keys the naming strategy by BEHAVIOR rather than function
// identity. The old %p encoding collided closures created at the same call
// site (e.g. a per-tenant factory `func mk(prefix string) figo.NamingFunc`):
// they share one code pointer regardless of captured state, so two instances
// differing only in captured naming state produced identical keys and
// cross-tenant cache hits returned the wrong columns. Instead, the naming
// func is applied to a canonical probe set — a few fixed strings plus the
// instance's own pre-naming select fields and sort columns — and the outputs
// are folded into the key.
//
// Limitation: behavior can only be sampled, not proven equal. A pathological
// naming func that ignores its input (or its captured state) on every probe
// string while still renaming real clause columns differently could collide.
func namingFingerprint(f figo.Figo) string {
	fn := f.GetNamingFunc()
	if fn == nil {
		return "<nil>"
	}
	probes := []string{"FigoProbe", "user.createdAt", "AbCd_ef"}
	if sel := f.GetSelectFields(); len(sel) > 0 {
		fields := make([]string, 0, len(sel))
		for name := range sel {
			fields = append(fields, name)
		}
		sort.Strings(fields)
		probes = append(probes, fields...)
	}
	if ob := f.GetSort(); ob != nil {
		for _, col := range ob.Columns {
			probes = append(probes, col.Name)
		}
	}
	var b strings.Builder
	for _, probe := range probes {
		out := fn(probe)
		fmt.Fprintf(&b, "%d:%s,", len(out), out)
	}
	return b.String()
}

// adapterFingerprint renders the adapter's configuration by CONTENTS,
// dereferencing pointers. fmt's %+v printed a RawAdapter{Dialect: d} as the
// *SQLDialect's ADDRESS: reconfiguring the dialect in place kept the old key
// (serving stale MySQL-quoted SQL for a Postgres request), and heap address
// reuse across per-request dialect copies (the customization pattern
// adapters/dialect.go recommends) collided fresh configs into stale slots.
func adapterFingerprint(a figo.Adapter) string {
	if a == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%T", a)
	writeValueContents(&b, reflect.ValueOf(a), make(map[uintptr]bool), 0)
	return b.String()
}

// writeValueContents renders v recursively with pointers dereferenced and map
// entries in deterministic order. seen guards against pointer cycles; depth
// bounds pathological nesting. Funcs have no comparable contents and key by
// code pointer (the best identity available).
func writeValueContents(b *strings.Builder, v reflect.Value, seen map[uintptr]bool, depth int) {
	if depth > 10 {
		b.WriteString("<deep>")
		return
	}
	switch v.Kind() {
	case reflect.Invalid:
		b.WriteString("<nil>")
	case reflect.Pointer:
		if v.IsNil() {
			b.WriteString("<nil>")
			return
		}
		if p := v.Pointer(); seen[p] {
			b.WriteString("<cycle>")
			return
		} else {
			seen[p] = true
		}
		b.WriteByte('&')
		writeValueContents(b, v.Elem(), seen, depth+1)
	case reflect.Interface:
		if v.IsNil() {
			b.WriteString("<nil>")
			return
		}
		fmt.Fprintf(b, "(%s)", v.Elem().Type())
		writeValueContents(b, v.Elem(), seen, depth+1)
	case reflect.Struct:
		b.WriteByte('{')
		for i := 0; i < v.NumField(); i++ {
			b.WriteString(v.Type().Field(i).Name)
			b.WriteByte(':')
			writeValueContents(b, v.Field(i), seen, depth+1)
			b.WriteByte(';')
		}
		b.WriteByte('}')
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			writeValueContents(b, v.Index(i), seen, depth+1)
			b.WriteByte(';')
		}
		b.WriteByte(']')
	case reflect.Map:
		entries := make([]string, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			var e strings.Builder
			writeValueContents(&e, iter.Key(), seen, depth+1)
			e.WriteByte('=')
			writeValueContents(&e, iter.Value(), seen, depth+1)
			entries = append(entries, e.String())
		}
		sort.Strings(entries)
		b.WriteString("map[")
		for _, e := range entries {
			b.WriteString(e)
			b.WriteByte(';')
		}
		b.WriteByte(']')
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if v.IsNil() {
			b.WriteString("<nil>")
			return
		}
		fmt.Fprintf(b, "%s@%x", v.Kind(), v.Pointer())
	default:
		// Scalars (and strings). fmt renders a reflect.Value operand as the
		// value it holds, so unexported fields print fine here too.
		fmt.Fprintf(b, "%v", v)
	}
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
