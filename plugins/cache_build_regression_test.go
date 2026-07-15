package plugins

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Build must be idempotent: preloads must not accumulate and a previous DSL's
// sort must not survive a rebuild.
func TestBuildIsIdempotent(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 load=[Orders:price>10]`))
	f.Build(RawAdapter{})
	f.Build(nil)
	f.Build(nil)
	assert.Len(t, f.GetPreloads()["Orders"], 1, "preloads must not accumulate across Build calls")

	require.NoError(t, f.AddFiltersFromString(`id=2 sort=name:desc`))
	f.Build(nil)
	require.NotNil(t, f.GetSort())

	require.NoError(t, f.AddFiltersFromString(`id=3`))
	f.Build(nil)
	assert.Nil(t, f.GetSort(), "stale sort from a previous DSL must not survive rebuild")
}

// Two instances sharing a cache (one CachePlugin serving both) must not
// collide when their values differ only in type (a = int64(1) vs a = "1").
func TestCacheKeyDistinguishesValueTypes(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 100})
	defer cp.Close()

	f1 := New()
	f1.AddFilter(EqExpr{Field: "a", Value: int64(1)})
	f1.SetAdapterObject(RawAdapter{})

	f2 := New()
	f2.AddFilter(EqExpr{Field: "a", Value: "1"})
	f2.SetAdapterObject(RawAdapter{})

	q1, ok := cp.GetCachedQuery(f1, RawContext{Table: "t"}).(SQLQuery)
	require.True(t, ok)
	q2, ok := cp.GetCachedQuery(f2, RawContext{Table: "t"}).(SQLQuery)
	require.True(t, ok)

	assert.Equal(t, []any{int64(1)}, q1.Args)
	assert.Equal(t, []any{"1"}, q2.Args, "typed values must not share a cache slot")
}

// Overwriting an existing key at MaxSize must not evict an unrelated entry.
func TestCacheSetExistingKeyDoesNotEvict(t *testing.T) {
	c := NewInMemoryCache(CacheConfig{Enabled: true, MaxSize: 2, TTL: time.Minute})
	defer c.Stop()

	c.Set("a", 1, time.Minute)
	c.Set("b", 2, time.Minute)
	c.Set("a", 3, time.Minute) // overwrite at capacity

	_, aOK := c.Get("a")
	_, bOK := c.Get("b")
	assert.True(t, aOK, "overwritten key must remain")
	assert.True(t, bOK, "unrelated key must not be evicted by an overwrite")
}

// Replacing a plugin-created cache must stop its cleanup goroutine.
func TestOwnedCacheStoppedOnReplace(t *testing.T) {
	cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, CleanupInterval: time.Hour})
	owned, ok := cp.GetCache().(*InMemoryCache)
	require.True(t, ok)

	replacement := NewInMemoryCache(CacheConfig{Enabled: true, TTL: time.Minute})
	defer replacement.Stop()
	cp.SetCache(replacement)

	// Stop is idempotent; a second Stop on an already-stopped cache returns
	// immediately, while an un-stopped one would leave its goroutine behind.
	done := make(chan struct{})
	go func() {
		owned.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("owned cache was not stopped on replacement")
	}
}
