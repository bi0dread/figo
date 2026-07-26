package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strconv"
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
// MaxSize <= 0 means unlimited. CleanupInterval <= 0 disables the periodic
// sweep goroutine; expired entries are still reclaimed without it, on Get, on
// Set and on Stats — the old note that they were "dropped lazily on Get" was
// reassuring but wrong, because Get only ever drops the key it was handed and a
// key space driven by distinct filter strings is never requested twice.
// MaxSize remains the only bound on a cache of LIVE entries.
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

	// Intrusive bookkeeping owned by InMemoryCache. The recency order used to
	// be recovered by scanning the whole map on every eviction (O(MaxSize) per
	// insert, under the exclusive lock); these links make it O(1).
	key      string
	prev     *CacheEntry // towards the most recently used end
	next     *CacheEntry // towards the least recently used end
	heapIdx  int         // position in the expiry heap, -1 when not queued
	memBytes int64       // this entry's contribution to Stats().MemoryUsage
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
//
// The key includes the render CONTEXT, fingerprinted by contents. A context
// whose contents cannot be fingerprinted — notably a *gorm.DB, which is a large
// graph of mutable connection state rather than a description of the query —
// makes the render bypass the cache entirely: nothing is stored and nothing is
// served. Keying such a context by its pointer address instead used to serve
// one table's SQL for another table's request.
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

	var key string
	if cache != nil && enabled {
		key = generateCacheKey(f, "sql", ctx, conditionType...)
	}

	// An empty key means the render context cannot be fingerprinted by
	// contents (see writeCtxFingerprint) — bypass the cache rather than serve
	// another context's SQL.
	if key == "" {
		sql := f.GetSqlString(ctx, conditionType...)
		if sql == "" && !emptyRenderSucceeded(f, ctx, conditionType...) {
			renderErr = errRenderFailed
		}
		return sql
	}

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
	// A FAILED render is never cached: "" from a veto, a missing adapter or an
	// unsupported expression must not be served as a hit for the whole TTL —
	// not even after a transient veto lifts. An empty render that SUCCEEDED
	// (an ORDERBY segment with no sort=, a JOIN segment with no load=) is a
	// perfectly good value and is cached like any other; it used to be
	// re-rendered forever and counted as an error on every call.
	sql := f.GetSqlString(ctx, conditionType...)
	cacheable := sql != ""
	if sql == "" {
		if emptyRenderSucceeded(f, ctx, conditionType...) {
			cacheable = true
		} else {
			// A failed render is observable in the monitor's ErrorCount
			// instead of counting as an ordinary miss (mirrors GetCachedQuery).
			renderErr = errRenderFailed
		}
	}
	if cacheable && generateCacheKey(f, "sql", ctx, conditionType...) == key {
		cache.Set(key, sql, ttl)
	}

	return sql
}

// emptyRenderSucceeded reports whether an empty SQL render was a SUCCESSFUL
// render of a legitimately empty segment rather than a failure.
//
// figo.GetSqlString collapses both into "": it discards the adapter's ok flag,
// so the plugin cannot tell "the ORDERBY segment of a query with no sort= is
// empty" (ok=true) from "the render failed" (ok=false). Conflating them charged
// one ErrorCount per call for a healthy query — an operator alarming on that
// counter saw a 100% error rate for a working endpoint — and made the segment
// permanently uncacheable on the sql path while GetCachedQuery cached the
// identical state happily.
//
// The probe asks the adapter directly, which runs no hooks. That is exactly why
// it is skipped whenever any plugin is registered: a BeforeQuery veto also
// renders "", the hookless probe would report success, and caching "" would
// then serve the vetoed result for the whole TTL. With no plugin able to veto,
// "" from GetSqlString can only have come from the adapter, so the probe's ok
// flag is the answer. (A figo.Figo that surfaced the flag directly would make
// the probe unnecessary.)
func emptyRenderSucceeded(f figo.Figo, ctx any, conditionType ...string) bool {
	if pm := f.GetPluginManager(); pm != nil && len(pm.ListPlugins()) > 0 {
		return false
	}
	adapter := f.GetAdapterObject()
	if adapter == nil {
		return false
	}
	sql, ok := adapter.GetSqlString(f, ctx, conditionType...)
	return ok && sql == ""
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

	var key string
	if cache != nil && enabled {
		key = generateCacheKey(f, "query", ctx, conditionType...)
	}

	// See GetCachedSqlString: an unfingerprintable ctx bypasses the cache.
	if key == "" {
		q := f.GetQuery(ctx, conditionType...)
		if q == nil {
			renderErr = errRenderFailed
		}
		return q
	}

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

// byteWriter is the sink the key/fingerprint writers render into: both
// *strings.Builder and *cacheKeyBuf satisfy it, so a fingerprint can be
// streamed straight into the key buffer instead of being materialized as an
// intermediate string first.
type byteWriter interface {
	io.Writer
	WriteString(string) (int, error)
	WriteByte(byte) error
}

// cacheKeyBuf accumulates the cache key's components. Every component is
// terminated with "|<len>;" — a SUFFIX length delimiter. It is exactly as
// unambiguous as the old "<len>:<component>|" prefix form (decode from the
// right instead of the left), but the length is only known after the component
// has been written, which is what lets components be written STREAMING
// (fmt.Fprintf directly into this buffer) rather than being built as a dozen
// separate strings first. Key generation runs on every cached render and used
// to cost more than the render it replaces.
type cacheKeyBuf struct {
	buf   []byte
	start int // offset of the component currently being written
}

func (k *cacheKeyBuf) Write(p []byte) (int, error) {
	k.buf = append(k.buf, p...)
	return len(p), nil
}

func (k *cacheKeyBuf) WriteString(s string) (int, error) {
	k.buf = append(k.buf, s...)
	return len(s), nil
}

func (k *cacheKeyBuf) WriteByte(c byte) error {
	k.buf = append(k.buf, c)
	return nil
}

// end closes the component currently being written.
func (k *cacheKeyBuf) end() {
	n := len(k.buf) - k.start
	k.buf = append(k.buf, '|')
	k.buf = strconv.AppendInt(k.buf, int64(n), 10)
	k.buf = append(k.buf, ';')
	k.start = len(k.buf)
}

// addString writes a whole component in one go.
func (k *cacheKeyBuf) addString(s string) {
	k.buf = append(k.buf, s...)
	k.end()
}

func (k *cacheKeyBuf) reset() {
	k.buf = k.buf[:0]
	k.start = 0
}

var keyBufPool = sync.Pool{New: func() any { return &cacheKeyBuf{buf: make([]byte, 0, 512)} }}

// cacheKeyPrefix namespaces the key; an empty key means "not cacheable".
const cacheKeyPrefix = "figo:"

// cacheKeyHashBytes is how much of the digest lands in the key.
const cacheKeyHashBytes = 16

// generateCacheKey creates a unique cache key for a query. kind ("sql"/"query")
// keeps the two result types in separate slots — otherwise GetCachedSqlString and
// GetCachedQuery share a key and clobber each other's entries. The key covers
// everything that changes the rendered output: the adapter type, the custom
// naming func, the registered plugin set, the render context and the global
// regex SQL operator. It reads the instance through its public getters, so the
// snapshot is not atomic under concurrent mutation — the key re-check in the
// render wrappers guards against caching a torn result.
//
// It returns "" when the render CONTEXT cannot be keyed by contents (see
// writeCtxFingerprint); the render wrappers then bypass the cache entirely
// rather than key on a pointer address.
func generateCacheKey(f figo.Figo, kind string, ctx any, conditionType ...string) string {
	k := keyBufPool.Get().(*cacheKeyBuf)
	defer func() {
		if cap(k.buf) > 64<<10 {
			k.buf = make([]byte, 0, 512) // don't pool a pathological buffer
		}
		k.reset()
		keyBufPool.Put(k)
	}()

	// ctx first: an unkeyable ctx aborts before the expensive components.
	if !writeCtxFingerprint(k, ctx) {
		return ""
	}
	k.end()

	clauses := f.GetClauses()
	preloads := f.GetPreloads()

	k.addString(kind)
	k.addString(f.GetDSL())
	// The clause and preload trees are rendered by CONTENTS (see
	// writeClauseFingerprint), not with %#v: %#v printed a clause payload's
	// POINTER as its heap address and a CustomExpr.Handler as its CODE
	// pointer, which is shared by every closure minted from one function
	// literal — so two per-tenant handlers rendering different WHEREs shared
	// a cache slot and one tenant was served the other's SQL. An unkeyable
	// payload now makes the whole key "" and the render bypasses the cache.
	// The value-type signature stays: the content walk renders int64(1) and
	// float64(1) alike when they are not held in an interface.
	if !writeClauseFingerprint(k, clauses) {
		return ""
	}
	k.end()
	writeValueTypeSignature(k, clauses)
	k.end()
	if !writeClauseFingerprint(k, preloads) {
		return ""
	}
	k.end()
	writePreloadValueTypeSignature(k, preloads)
	k.end()
	page := f.GetPage()
	fmt.Fprintf(k, "%d/%d", page.Skip, page.Take)
	k.end()
	fmt.Fprintf(k, "%v", f.GetSort())
	k.end()
	// Ignore/whitelist policy needs no key component: FieldsPlugin prunes
	// expressions before they enter the clause tree, so the clauses
	// component above already reflects it.
	fmt.Fprintf(k, "%v", f.GetSelectFields())
	k.end()
	writeNamingFingerprint(k, f)
	k.end()
	// Contents, not %T%+v: adapter configuration changes the rendered
	// SQL — e.g. RawAdapter{Dialect: PostgresDialect} vs the MySQL
	// default must not share a cache slot — and %+v printed pointer
	// fields (like *SQLDialect) as bare addresses, keying on WHERE the
	// config lived instead of what it said.
	writeAdapterFingerprint(k, f.GetAdapterObject())
	k.end()
	k.addString(figo.GetRegexSQLOperator())
	// The plugin set changes the rendered output too: a BeforeQuery hook can
	// veto the render outright. Without this component an entry warmed by a
	// plugin-free instance was served to an instance whose authorization hook
	// denies — and the hook never ran at all, because hits skip it.
	writePluginFingerprint(k, f)
	k.end()
	// conditionType: the element COUNT plus a length delimiter per element.
	// A single %v of the slice erased the element boundaries, so
	// []string{"where","select"} and []string{"where select"} shared a key.
	k.addString(strconv.Itoa(len(conditionType)))
	for _, c := range conditionType {
		k.addString(c)
	}

	// SHA-256 truncated to 128 bits: same key length as the previous md5, but
	// ~3.5x faster here (hardware-accelerated) and without md5's practical
	// collision construction — the DSL that feeds this content is untrusted.
	hash := sha256.Sum256(k.buf)
	out := make([]byte, len(cacheKeyPrefix)+hex.EncodedLen(cacheKeyHashBytes))
	copy(out, cacheKeyPrefix)
	hex.Encode(out[len(cacheKeyPrefix):], hash[:cacheKeyHashBytes])
	return string(out)
}

// ctxFingerprintBudget bounds the node count of the ctx content walk. Exceeding
// the budget means "cannot be keyed by contents", which fails safe (no caching)
// instead of falling back to an address.
//
// It was 64, which is a handful of nodes: enough for the small contexts the raw
// adapter documents (nil, a table-name string, a RawContext) but NOT for
// map[string]adapters.MongoJoin, the documented ctx of the Mongo aggregate path
// — a MongoJoin entry costs 6 nodes, so 11 joins already went over and the
// whole aggregate path silently stopped caching. 512 admits ~85 joins while
// still aborting a *gorm.DB (a graph of thousands of nodes, most of it live
// connection-pool state that differs between identical requests) after a few
// dozen.
const ctxFingerprintBudget = 512

// NOTE for future passes: there used to be a sync.Map memo here that remembered
// ctx TYPES whose walk came back unsound, so the aborted walk ran once per type
// instead of once per render. It was removed because unsoundness is a property
// of the VALUE, not of the type: one oversized (or too deeply nested) context
// value permanently and silently switched the cache off for every later value
// of that type — process-wide, including for CachePlugins constructed
// afterwards with their own InMemoryCache — and a bypassed render reports
// neither a hit nor a miss, so Stats() showed an IDLE cache rather than a
// disabled one. Re-adding a per-type memo re-opens that: the budget itself is
// what bounds the cost of an unkeyable context (at most ctxFingerprintBudget
// nodes per render), and a *gorm.DB aborts after ~23 of them.

// writeCtxFingerprint renders the render context by CONTENTS with its dynamic
// type, exactly as writeAdapterFingerprint does for the adapter. It reports
// whether the fingerprint is sound.
//
// The old encoding was fmt.Sprintf("%v", ctx), which printed a pointer ctx as
// its ADDRESS. On the GORM path ctx is a *gorm.DB whose fields are all
// pointers, so the model/table, the WHERE scopes and the session options never
// reached the key — and GormAdapter{} is an empty struct, so ctx was the only
// per-request discriminator. An in-place Model switch served the previous
// table's SQL, a tenant scope added with Where was silently dropped, and heap
// address reuse across per-request handles served the wrong table for 5-24% of
// cached renders. This is the same defect round 3 fixed for the adapter
// component; the contents treatment simply was never applied to ctx.
//
// The type prefix matters on its own: "{users}" (a string) and
// RawContext{Table:"users"} print identically under %v and shared a key.
func writeCtxFingerprint(w byteWriter, ctx any) bool {
	if ctx == nil {
		_, _ = w.WriteString("<nil>")
		return true
	}
	_, _ = w.WriteString(reflect.TypeOf(ctx).String())
	vw := valueWalker{b: w, budget: ctxFingerprintBudget, strict: true}
	vw.write(reflect.ValueOf(ctx), 0)
	// Decided per VALUE, never remembered per type: see the note above
	// ctxFingerprintBudget. A big context value must not disable caching for
	// the small ones that follow it.
	return !vw.unsound
}

// clauseFingerprintBudget / clauseFingerprintDepth bound the clause-tree content
// walk. They are far more generous than the ctx budget because the clause tree
// is the query: LimitsPlugin's default ceiling is 200 expression nodes and a
// simple leaf costs ~5 walked nodes, so 16384 nodes leaves three orders of
// magnitude of headroom, and 64 levels of depth cover ~20 nested groups (the
// parser emits flat n-ary and/or nodes, so nesting only comes from explicit
// parentheses). Exceeding either bound fails safe — no caching — rather than
// truncating the fingerprint, which would let two different trees share a key.
const (
	clauseFingerprintBudget = 1 << 14
	clauseFingerprintDepth  = 64
)

// writeClauseFingerprint renders a clause tree (or the preload map) by CONTENTS
// and reports whether the fingerprint is sound.
//
// The old encoding was fmt.Fprintf("%#v", clauses), which prints what a payload
// IS only for scalars. For a pointer it prints the heap ADDRESS, so two
// instances holding equal-valued but separately allocated payloads never shared
// a slot (a 0% hit rate) while address reuse across requests collided unrelated
// payloads into one. Worse, for a func it prints the CODE pointer, which every
// closure minted from the same function literal shares regardless of what it
// captured: a per-tenant figo.CustomExpr.Handler factory produced byte-identical
// keys for handlers rendering `tenant_id = 'acme'` and `tenant_id = 'globex'`,
// and one shared CachePlugin then served acme's WHERE to globex. That is the
// same defect hunt #6's H0 fixed for the render context and round 3 fixed for
// the adapter and naming components; this is the clause component.
//
// A func (or chan, or unsafe.Pointer) cannot be fingerprinted by contents at
// all, so a clause carrying a live one is NOT CACHEABLE: strict mode marks the
// walk unsound, generateCacheKey returns "" and the render bypasses the cache
// entirely. Sampling a Handler's behaviour the way writeNamingFingerprint
// samples a naming func is not an option here — the probe would have to call
// application code with fabricated arguments, and the handler is free to have
// side effects.
func writeClauseFingerprint(w byteWriter, v any) bool {
	vw := valueWalker{
		b:        w,
		budget:   clauseFingerprintBudget,
		maxDepth: clauseFingerprintDepth,
		strict:   true,
	}
	vw.write(reflect.ValueOf(v), 0)
	return !vw.unsound
}

// writePluginFingerprint keys the registered plugin set by name+version in
// registration order (the order every hook dispatch uses).
//
// Limitation, mirroring writeNamingFingerprint's: two instances holding
// DIFFERENTLY CONFIGURED plugins of the same name and version still share a
// key. Fingerprinting plugin state itself is not an option — a MetricsPlugin's
// counters change on every call, which would make every key unique — and that
// case stays covered by the documented caveat that a cache hit skips
// BeforeQuery. What this component fixes is the stronger failure: a DIFFERENT
// plugin set (notably "no plugins at all" vs "a plugin that vetoes") sharing
// one slot.
func writePluginFingerprint(w byteWriter, f figo.Figo) {
	pm := f.GetPluginManager()
	if pm == nil {
		_, _ = w.WriteString("<nil>")
		return
	}
	for _, p := range pm.ListPlugins() {
		if p == nil {
			_, _ = w.WriteString("<nil>,")
			continue
		}
		name, version := p.Name(), p.Version()
		_, _ = w.WriteString(strconv.Itoa(len(name)))
		_ = w.WriteByte(':')
		_, _ = w.WriteString(name)
		_ = w.WriteByte('@')
		_, _ = w.WriteString(strconv.Itoa(len(version)))
		_ = w.WriteByte(':')
		_, _ = w.WriteString(version)
		_ = w.WriteByte(',')
	}
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
func writeNamingFingerprint(w byteWriter, f figo.Figo) {
	fn := f.GetNamingFunc()
	if fn == nil {
		_, _ = w.WriteString("<nil>")
		return
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
	for _, probe := range probes {
		out := fn(probe)
		_, _ = w.WriteString(strconv.Itoa(len(out)))
		_ = w.WriteByte(':')
		_, _ = w.WriteString(out)
		_ = w.WriteByte(',')
	}
}

// adapterFingerprint renders the adapter's configuration by CONTENTS,
// dereferencing pointers. fmt's %+v printed a RawAdapter{Dialect: d} as the
// *SQLDialect's ADDRESS: reconfiguring the dialect in place kept the old key
// (serving stale MySQL-quoted SQL for a Postgres request), and heap address
// reuse across per-request dialect copies (the customization pattern
// adapters/dialect.go recommends) collided fresh configs into stale slots.
func adapterFingerprint(a figo.Adapter) string {
	var b strings.Builder
	writeAdapterFingerprint(&b, a)
	return b.String()
}

func writeAdapterFingerprint(w byteWriter, a figo.Adapter) {
	if a == nil {
		_, _ = w.WriteString("<nil>")
		return
	}
	_, _ = w.WriteString(reflect.TypeOf(a).String())
	writeValueContents(w, reflect.ValueOf(a), 0)
}

// writeValueContents renders v recursively with pointers dereferenced and map
// entries in deterministic order, with no bound on the graph size (the adapter
// path: adapters are small configuration structs).
func writeValueContents(b byteWriter, v reflect.Value, depth int) {
	vw := valueWalker{b: b, budget: -1}
	vw.write(v, depth)
}

// valueWalker renders a value by CONTENTS. seen guards against pointer cycles;
// depth bounds pathological nesting (maxDepth, or defaultWalkDepth when 0);
// budget bounds the total node count when >= 0 (-1 is unlimited).
//
// strict mode is for values whose fingerprint must be SOUND or not used at all
// (the render context). In strict mode anything that can only be keyed by an
// address (a non-nil func, chan or unsafe pointer) and any truncation (depth or
// budget) marks the walk unsound, so the caller can bypass the cache instead of
// keying on WHERE a value lives rather than what it says. Outside strict mode
// those cases keep the historical behavior: funcs key by code pointer (the best
// identity available) and deep graphs truncate with a marker.
type valueWalker struct {
	b        byteWriter
	seen     map[uintptr]bool // lazily allocated: most values contain no pointers
	budget   int
	maxDepth int // 0 = defaultWalkDepth
	strict   bool
	unsound  bool
}

// defaultWalkDepth is the historical bound, kept for the adapter and context
// walks (small configuration structs). The clause tree needs a deeper one — see
// clauseFingerprintDepth.
const defaultWalkDepth = 10

func (w *valueWalker) write(v reflect.Value, depth int) {
	if w.unsound {
		return
	}
	if w.budget >= 0 {
		if w.budget == 0 {
			w.unsound = true
			return
		}
		w.budget--
	}
	limit := w.maxDepth
	if limit == 0 {
		limit = defaultWalkDepth
	}
	if depth > limit {
		if w.strict {
			w.unsound = true
			return
		}
		_, _ = w.b.WriteString("<deep>")
		return
	}
	b := w.b
	switch v.Kind() {
	case reflect.Invalid:
		_, _ = b.WriteString("<nil>")
	case reflect.Pointer:
		if v.IsNil() {
			_, _ = b.WriteString("<nil>")
			return
		}
		p := v.Pointer()
		if w.seen == nil {
			w.seen = make(map[uintptr]bool)
		}
		if w.seen[p] {
			_, _ = b.WriteString("<cycle>")
			return
		}
		w.seen[p] = true
		_ = b.WriteByte('&')
		w.write(v.Elem(), depth+1)
	case reflect.Interface:
		if v.IsNil() {
			_, _ = b.WriteString("<nil>")
			return
		}
		_ = b.WriteByte('(')
		_, _ = b.WriteString(v.Elem().Type().String())
		_ = b.WriteByte(')')
		w.write(v.Elem(), depth+1)
	case reflect.Struct:
		_ = b.WriteByte('{')
		for i := 0; i < v.NumField(); i++ {
			if w.unsound {
				return
			}
			_, _ = b.WriteString(v.Type().Field(i).Name)
			_ = b.WriteByte(':')
			w.write(v.Field(i), depth+1)
			_ = b.WriteByte(';')
		}
		_ = b.WriteByte('}')
	case reflect.Slice, reflect.Array:
		_ = b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if w.unsound {
				return
			}
			w.write(v.Index(i), depth+1)
			_ = b.WriteByte(';')
		}
		_ = b.WriteByte(']')
	case reflect.Map:
		entries := make([]string, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			if w.unsound {
				return
			}
			var e strings.Builder
			// maxDepth must travel with the sub-walker: the preload map's
			// values are clause trees, and a default-depth sub-walk would
			// truncate them (and, in strict mode, refuse to key them).
			sub := valueWalker{b: &e, seen: w.seen, budget: w.budget, maxDepth: w.maxDepth, strict: w.strict}
			sub.write(iter.Key(), depth+1)
			// The key's rendered LENGTH is appended below, because "key=value"
			// alone is ambiguous when a key can contain '=':
			// map{"a=b":"c"} and map{"a":"b=c"} both rendered "a=b=c" and
			// shared a cache slot. The preload map's keys are relation names
			// and a ctx map's keys are application-supplied, so this is a
			// wrong-serve path, not a theoretical one.
			klen := e.Len()
			e.WriteByte('=')
			sub.write(iter.Value(), depth+1)
			e.WriteByte('|')
			e.WriteString(strconv.Itoa(klen))
			w.seen, w.budget, w.unsound = sub.seen, sub.budget, sub.unsound
			entries = append(entries, e.String())
		}
		sort.Strings(entries)
		_, _ = b.WriteString("map[")
		for _, e := range entries {
			_, _ = b.WriteString(e)
			_ = b.WriteByte(';')
		}
		_ = b.WriteByte(']')
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if v.IsNil() {
			_, _ = b.WriteString("<nil>")
			return
		}
		if w.strict {
			// Only an address is available, which is exactly what this
			// fingerprint exists to avoid.
			w.unsound = true
			return
		}
		fmt.Fprintf(b, "%s@%x", v.Kind(), v.Pointer())
	case reflect.String:
		// Scalar fast paths: these render identically to %v but skip fmt's
		// reflection dispatch, which dominated the fingerprint cost.
		_, _ = b.WriteString(v.String())
	case reflect.Bool:
		if v.Bool() {
			_, _ = b.WriteString("true")
		} else {
			_, _ = b.WriteString("false")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, _ = b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		_, _ = b.WriteString(strconv.FormatUint(v.Uint(), 10))
	default:
		// Remaining scalars. fmt renders a reflect.Value operand as the
		// value it holds, so unexported fields print fine here too.
		fmt.Fprintf(b, "%v", v)
	}
}

// valueTypeSignature renders the Go type of every value carried by the given
// expression trees, in order. It exists because the cache key's %#v encoding
// collapses int(1)/int64(1)/float64(1) to the same text; appending this makes
// two clauses that differ only in a value's numeric type produce distinct keys.
func writeValueTypeSignature(b byteWriter, exprs []figo.Expr) {
	for _, e := range exprs {
		appendValueTypes(b, e)
		_ = b.WriteByte('|')
	}
}

// writePreloadValueTypeSignature does the same across all preload expression
// lists, keyed by relation name so two preloads can't swap type signatures
// unnoticed.
func writePreloadValueTypeSignature(b byteWriter, preloads map[string][]figo.Expr) {
	for _, name := range sortedKeys2(preloads) {
		_, _ = b.WriteString(name)
		_ = b.WriteByte(':')
		writeValueTypeSignature(b, preloads[name])
	}
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

func appendValueTypes(b byteWriter, e figo.Expr) {
	writeT := func(v any) { fmt.Fprintf(b, "%T,", v) }
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

// InMemoryCache implements QueryCache using in-memory storage.
//
// Besides the map it keeps two indexes, both maintained incrementally so that
// no operation scans the whole cache while holding the lock:
//   - an intrusive LRU list (mru/lru) so eviction picks its victim in O(1)
//     instead of scanning every entry on every insert at capacity;
//   - a min-heap of the entries that have an expiry, so expired entries are
//     reclaimed in O(log n) each without a sweep — TTL used to free nothing at
//     all unless a CleanupInterval goroutine was configured, because Get only
//     drops the key it was handed and an attacker-chosen key space is never
//     requested twice.
type InMemoryCache struct {
	mu       sync.RWMutex
	entries  map[string]*CacheEntry
	mru      *CacheEntry // most recently used (head of the LRU list)
	lru      *CacheEntry // least recently used (tail) — the eviction victim
	expiry   expiryHeap
	memBytes int64 // sum of the live entries' estimates; Stats() is O(1)
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

// expiryHeap orders entries by ExpiresAt, soonest first. Only entries with a
// non-zero ExpiresAt are queued (TTL <= 0 never expires).
type expiryHeap []*CacheEntry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].ExpiresAt.Before(h[j].ExpiresAt) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].heapIdx = i; h[j].heapIdx = j }
func (h *expiryHeap) Push(x interface{}) {
	e := x.(*CacheEntry)
	e.heapIdx = len(*h)
	*h = append(*h, e)
}
func (h *expiryHeap) Pop() interface{} {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.heapIdx = -1
	*h = old[:n-1]
	return e
}

// lruPushFront makes e the most recently used entry. Caller holds c.mu.
func (c *InMemoryCache) lruPushFront(e *CacheEntry) {
	e.prev = nil
	e.next = c.mru
	if c.mru != nil {
		c.mru.prev = e
	}
	c.mru = e
	if c.lru == nil {
		c.lru = e
	}
}

// lruUnlink removes e from the recency list. Caller holds c.mu.
func (c *InMemoryCache) lruUnlink(e *CacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if c.mru == e {
		c.mru = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if c.lru == e {
		c.lru = e.prev
	}
	e.prev, e.next = nil, nil
}

// removeEntryLocked drops e from the map and both indexes. Caller holds c.mu.
func (c *InMemoryCache) removeEntryLocked(e *CacheEntry) {
	delete(c.entries, e.key)
	c.lruUnlink(e)
	if e.heapIdx >= 0 && e.heapIdx < len(c.expiry) {
		heap.Remove(&c.expiry, e.heapIdx)
		e.heapIdx = -1
	}
	c.memBytes -= e.memBytes
}

// reapExpiredLocked drops entries whose TTL has lapsed, oldest expiry first.
// max <= 0 reaps everything due; a positive max bounds the work one call can do
// so a mass expiry can't stall a single Set inside the exclusive lock (the
// remainder is reaped by the next call — the amortized cost is O(1) per insert
// either way). Caller holds c.mu.
func (c *InMemoryCache) reapExpiredLocked(now time.Time, max int) {
	for n := 0; len(c.expiry) > 0 && (max <= 0 || n < max); n++ {
		top := c.expiry[0]
		if top.ExpiresAt.IsZero() || !now.After(top.ExpiresAt) {
			return
		}
		c.removeEntryLocked(top)
	}
}

// entryMemEstimate is the rough per-entry accounting Stats().MemoryUsage
// reports (keys + string payloads + fixed overhead), computed once at insert
// so Stats does not have to walk the map.
func entryMemEstimate(key string, value interface{}) int64 {
	mem := int64(len(key)) + 64 // fixed bookkeeping estimate per entry
	if s, ok := value.(string); ok {
		return mem + int64(len(s))
	}
	return mem + 100 // opaque non-string payload estimate
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
	now := time.Now()
	if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
		c.removeEntryLocked(entry)
		c.misses++
		return nil, false
	}

	entry.HitCount++
	entry.LastAccessedAt = now
	c.lruUnlink(entry)
	c.lruPushFront(entry)
	c.hits++
	return entry.Data, true
}

// Set stores a value in the cache.
//
// It does NOT consult config.Enabled: the flag only ever made an injected cache
// whose config omitted Enabled silently write-dead (every Set a no-op, Get
// still counting misses, nothing ever erroring), and CachePlugin already gates
// on its own Enabled before calling here.
func (c *InMemoryCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	// Reclaim what the TTL has already invalidated. Without this, expired
	// entries were freed only by a Get on the SAME key or by the optional
	// cleanup goroutine, so a workload of distinct keys (one per untrusted
	// filter string) grew the map for the life of the process while Stats()
	// reported the cache as empty.
	c.reapExpiredLocked(now, 32)

	if old, exists := c.entries[key]; exists {
		// Overwriting an existing key doesn't grow the map, so it must never
		// evict an unrelated entry.
		c.removeEntryLocked(old)
	}
	// Check size limit. MaxSize <= 0 means unlimited; without this guard a
	// zero MaxSize would evict on every Set, pinning the cache to one entry.
	if c.config.MaxSize > 0 && len(c.entries) >= c.config.MaxSize {
		c.evictLRU()
	}

	entry := &CacheEntry{
		Data:           value,
		CreatedAt:      now,
		LastAccessedAt: now,
		HitCount:       0,
		key:            key,
		heapIdx:        -1,
		memBytes:       entryMemEstimate(key, value),
	}
	// TTL <= 0 means "never expires" (zero ExpiresAt). Storing now.Add(0)
	// created entries that were already expired on arrival, so an enabled
	// cache with no explicit TTL could never produce a hit.
	if ttl > 0 {
		entry.ExpiresAt = now.Add(ttl)
	}
	c.entries[key] = entry
	c.lruPushFront(entry)
	c.memBytes += entry.memBytes
	if !entry.ExpiresAt.IsZero() {
		heap.Push(&c.expiry, entry)
	}
}

// Delete removes a value from the cache
func (c *InMemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.removeEntryLocked(e)
	}
}

// Clear removes all entries from the cache
func (c *InMemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CacheEntry)
	c.mru, c.lru = nil, nil
	c.expiry = nil
	c.memBytes = 0
}

// Stats returns cache performance statistics. Size counts only LIVE entries:
// expired entries that no Get has touched are reclaimed here first, so the
// numbers describe what callers can actually hit. MemoryUsage stays a rough
// estimate (keys + string payloads + a fixed per-entry overhead), not an exact
// accounting.
//
// It takes the exclusive lock (Get needs one anyway) and both figures are
// maintained incrementally, so a metrics loop no longer walks every entry
// inside the lock — that scan cost milliseconds on a large cache and blocked
// every concurrent Get for its duration.
func (c *InMemoryCache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reapExpiredLocked(time.Now(), 0)

	total := c.hits + c.misses
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(c.hits) / float64(total)
	}

	return CacheStats{
		Hits:        c.hits,
		Misses:      c.misses,
		Size:        len(c.entries),
		HitRate:     hitRate,
		MemoryUsage: c.memBytes,
	}
}

// evictLRU removes the least recently used entry (by last access time, not
// creation time — otherwise a hot early entry would be evicted before a cold
// later one, i.e. FIFO rather than LRU).
//
// The victim is the tail of the recency list. The previous implementation
// scanned the entire map on every insert at capacity — O(MaxSize) of CPU inside
// the exclusive lock per cache miss, so configuring MORE cache made every miss
// slower — and it used the zero value of the key as its "nothing chosen yet"
// sentinel, which made an empty-string key either shield the true victim or
// skip the eviction altogether (the cache then grew past MaxSize without
// bound). Caller holds c.mu.
func (c *InMemoryCache) evictLRU() {
	if c.lru != nil {
		c.removeEntryLocked(c.lru)
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

// cleanupExpired removes expired entries. It walks the expiry heap rather than
// the whole map, so the lock is held for O(expired) work instead of O(size).
func (c *InMemoryCache) cleanupExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reapExpiredLocked(time.Now(), 0)
}

// Stop stops the cleanup goroutine. Safe to call multiple times (and alongside
// Close, which delegates here) — the underlying channel is closed at most once.
func (c *InMemoryCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
}
