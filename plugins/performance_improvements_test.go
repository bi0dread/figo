package plugins

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestQueryCaching(t *testing.T) {
	t.Run("CacheConfiguration", func(t *testing.T) {
		// A disabled plugin has no cache and reports its config as-is
		cp := NewCachePlugin(CacheConfig{})
		assert.False(t, cp.GetConfig().Enabled)
		assert.Nil(t, cp.GetCache())

		// Enabling via SetConfig creates an owned cache
		cp.SetConfig(CacheConfig{
			Enabled:         true,
			TTL:             5 * time.Minute,
			MaxSize:         100,
			CleanupInterval: 1 * time.Minute,
		})
		defer cp.Close()

		updatedConfig := cp.GetConfig()
		assert.True(t, updatedConfig.Enabled)
		assert.Equal(t, 5*time.Minute, updatedConfig.TTL)
		assert.Equal(t, 100, updatedConfig.MaxSize)
		assert.NotNil(t, cp.GetCache())
	})

	t.Run("InMemoryCache", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			Enabled:         true,
			TTL:             1 * time.Minute,
			MaxSize:         10,
			CleanupInterval: 30 * time.Second,
		})

		// Test basic operations
		cache.Set("key1", "value1", 1*time.Minute)
		cache.Set("key2", "value2", 1*time.Minute)

		// Test get
		value, found := cache.Get("key1")
		assert.True(t, found)
		assert.Equal(t, "value1", value)

		// Test miss
		_, found = cache.Get("nonexistent")
		assert.False(t, found)

		// Test stats
		stats := cache.Stats()
		assert.Equal(t, int64(1), stats.Hits)
		assert.Equal(t, int64(1), stats.Misses)
		assert.Equal(t, 2, stats.Size)
		assert.Equal(t, 0.5, stats.HitRate)

		// Test clear
		cache.Clear()
		stats = cache.Stats()
		assert.Equal(t, 0, stats.Size)
	})

	t.Run("CachedQueryExecution", func(t *testing.T) {
		f := New()
		cp := NewCachePlugin(CacheConfig{
			Enabled: true,
			TTL:     1 * time.Minute,
			MaxSize: 100,
		})
		defer cp.Close()

		// Build a query
		f.AddFiltersFromString(`id=1 and name="test"`)
		f.Build(RawAdapter{})

		// First execution (cache miss)
		sql1 := cp.GetCachedSqlString(f, RawContext{Table: "GG"})

		// Second execution (cache hit)
		sql2 := cp.GetCachedSqlString(f, RawContext{Table: "GG"})

		// Results should be identical
		assert.Equal(t, sql1, sql2)

		// Verify cache stats
		stats := cp.Stats()
		assert.Equal(t, int64(1), stats.Hits)
		assert.Equal(t, int64(1), stats.Misses)
	})

	t.Run("CacheExpiration", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			Enabled: true,
			TTL:     100 * time.Millisecond,
			MaxSize: 10,
		})

		// Set with short TTL
		cache.Set("key1", "value1", 100*time.Millisecond)

		// Should be available immediately
		_, found := cache.Get("key1")
		assert.True(t, found)

		// Wait for expiration
		time.Sleep(150 * time.Millisecond)

		// Should be expired
		_, found = cache.Get("key1")
		assert.False(t, found)
	})

	t.Run("CacheSizeLimit", func(t *testing.T) {
		cache := NewInMemoryCache(CacheConfig{
			Enabled: true,
			TTL:     1 * time.Minute,
			MaxSize: 2,
		})

		// Fill cache beyond limit
		cache.Set("key1", "value1", 1*time.Minute)
		cache.Set("key2", "value2", 1*time.Minute)
		cache.Set("key3", "value3", 1*time.Minute)

		// Should have evicted oldest entry
		stats := cache.Stats()
		assert.Equal(t, 2, stats.Size)

		// key1 should be evicted (oldest)
		_, found := cache.Get("key1")
		assert.False(t, found)

		// key2 and key3 should still be there
		_, found = cache.Get("key2")
		assert.True(t, found)
		_, found = cache.Get("key3")
		assert.True(t, found)
	})
}

func TestPerformanceMonitoring(t *testing.T) {
	t.Run("PerformanceMonitor", func(t *testing.T) {
		monitor := NewPerformanceMonitor(true)

		// Record some queries
		monitor.RecordQuery(100*time.Millisecond, true, nil)
		monitor.RecordQuery(200*time.Millisecond, false, nil)
		monitor.RecordQuery(150*time.Millisecond, true, nil)
		monitor.RecordQuery(50*time.Millisecond, false, assert.AnError)

		// Get metrics
		metrics := monitor.GetMetrics()

		assert.Equal(t, int64(4), metrics.QueryCount)
		assert.Equal(t, int64(2), metrics.CacheHits)
		assert.Equal(t, int64(2), metrics.CacheMisses)
		assert.Equal(t, int64(1), metrics.ErrorCount)
		assert.Equal(t, 125*time.Millisecond, metrics.AverageLatency)
		assert.Equal(t, 500*time.Millisecond, metrics.TotalLatency)
		assert.False(t, metrics.LastQueryTime.IsZero())
	})

	t.Run("DisabledMonitor", func(t *testing.T) {
		monitor := NewPerformanceMonitor(false)

		// Record queries
		monitor.RecordQuery(100*time.Millisecond, true, nil)
		monitor.RecordQuery(200*time.Millisecond, false, nil)

		// Metrics should be empty
		metrics := monitor.GetMetrics()
		assert.Equal(t, int64(0), metrics.QueryCount)
	})

	t.Run("ResetMetrics", func(t *testing.T) {
		monitor := NewPerformanceMonitor(true)

		// Record some queries
		monitor.RecordQuery(100*time.Millisecond, true, nil)
		monitor.RecordQuery(200*time.Millisecond, false, nil)

		// Verify metrics exist
		metrics := monitor.GetMetrics()
		assert.Equal(t, int64(2), metrics.QueryCount)

		// Reset metrics
		monitor.Reset()

		// Verify metrics are reset
		metrics = monitor.GetMetrics()
		assert.Equal(t, int64(0), metrics.QueryCount)
		assert.Equal(t, int64(0), metrics.CacheHits)
		assert.Equal(t, int64(0), metrics.CacheMisses)
	})

	t.Run("CachePluginWithMonitoring", func(t *testing.T) {
		f := New()
		monitor := NewMetricsPlugin(true)

		cp := NewCachePlugin(CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 10})
		defer cp.Close()
		cp.SetPerformanceMonitor(monitor.PerformanceMonitor)
		assert.NotNil(t, cp.GetPerformanceMonitor())

		// Build and execute query
		f.AddFiltersFromString(`id=1`)
		f.Build(RawAdapter{})

		// Rendering through the cache plugin records metrics
		_ = cp.GetCachedSqlString(f, nil)
		metrics := monitor.GetMetrics()
		assert.Equal(t, int64(1), metrics.QueryCount)

		// Reset metrics
		monitor.Reset()
		metrics = monitor.GetMetrics()
		assert.Equal(t, int64(0), metrics.QueryCount)
	})
}

func TestPerformanceImprovementsIntegration(t *testing.T) {
	t.Run("CompletePerformanceWorkflow", func(t *testing.T) {
		// Create figo instance with all performance features
		f := New()

		// Set up caching as a plugin
		cp := NewCachePlugin(CacheConfig{
			Enabled:         true,
			TTL:             1 * time.Minute,
			MaxSize:         100,
			CleanupInterval: 30 * time.Second,
		})
		defer cp.Close()

		// Set up monitoring on the cache plugin
		monitor := NewPerformanceMonitor(true)
		cp.SetPerformanceMonitor(monitor)

		// Build query
		f.AddFiltersFromString(`id=1 and name="test" and age>18`)
		f.Build(RawAdapter{})

		// Execute multiple times to test caching. The ctx must be a real
		// table name: a nil ctx fails the raw render, and failed (empty)
		// renders are deliberately never cached.
		for i := 0; i < 5; i++ {
			_ = cp.GetCachedSqlString(f, "users")
		}

		// Verify cache stats
		cacheStats := cp.Stats()
		assert.True(t, cacheStats.Hits > 0)
		assert.True(t, cacheStats.HitRate > 0)

		// Verify performance metrics
		metrics := monitor.GetMetrics()
		assert.True(t, metrics.QueryCount > 0)
	})
}
