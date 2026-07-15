package plugins

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Run these with: go test -race -run TestConcurrency .

// #7: concurrent Get must not race on the hit/miss counters.
func TestConcurrentCacheGet(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 100})
	defer c.Close()
	c.Set("k", "v", time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Get("k")
				c.Get("missing")
			}
		}()
	}
	wg.Wait()
	stats := c.Stats()
	assert.Equal(t, int64(8*200), stats.Hits)
	assert.Equal(t, int64(8*200), stats.Misses)
}

// #8 / #15: Build (writer) concurrent with the cached-query path and GetPreloads
// (readers) must not race on f.clauses / f.preloads / f.dsl.
func TestConcurrentBuildAndRead(t *testing.T) {
	f := New()
	cp := NewCachePlugin(CacheConfig{Enabled: true, MaxSize: 100, TTL: time.Minute})
	defer cp.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				f.AddFiltersFromString(`a=1 and b=2 load=[T:id=3]`)
				f.Build(RawAdapter{})
			}
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				cp.GetCachedSqlString(f, "t")
				_ = f.GetPreloads()
				_ = f.GetClauses()
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// #13: Close then Stop (and repeated calls) must not panic on a double close.
func TestCacheCloseIsIdempotent(t *testing.T) {
	assert.NotPanics(t, func() {
		c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 10, CleanupInterval: time.Hour})
		c.Close()
		c.Stop()
		c.Close()
	})
}

// #19: eviction is LRU (by last access), not FIFO (by creation).
func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 2})
	c.Set("a", 1, time.Minute)
	c.Set("b", 2, time.Minute)
	c.Get("a")                 // "a" is now more recently used than "b"
	c.Set("c", 3, time.Minute) // must evict "b", the least recently used
	_, aOK := c.Get("a")
	_, bOK := c.Get("b")
	_, cOK := c.Get("c")
	assert.True(t, aOK, "recently-used 'a' should survive")
	assert.False(t, bOK, "least-recently-used 'b' should be evicted")
	assert.True(t, cOK)
}

// #20: SQL and Query results must not share a cache key.
func TestCacheKeyDistinguishesSqlAndQuery(t *testing.T) {
	f := New()
	cp := NewCachePlugin(CacheConfig{Enabled: true, MaxSize: 100, TTL: time.Minute})
	defer cp.Close()
	f.AddFiltersFromString(`id=1`)
	f.Build(RawAdapter{})

	sql := cp.GetCachedSqlString(f, "t")
	q := cp.GetCachedQuery(f, "t")
	assert.NotEmpty(t, sql)
	assert.NotNil(t, q, "GetCachedQuery must not receive the string cached under the same key")
	_, ok := q.(SQLQuery)
	assert.True(t, ok, "cached query must be a real SQLQuery, not a mistyped string entry")
}
