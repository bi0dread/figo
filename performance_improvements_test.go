package figo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestQueryCaching(t *testing.T) {
	t.Run("CacheConfiguration", func(t *testing.T) {
		f := New(RawAdapter{})

		// Test default cache config
		config := f.GetCacheConfig()
		assert.False(t, config.Enabled)

		// Set cache config
		cacheConfig := CacheConfig{
			Enabled:         true,
			TTL:             5 * time.Minute,
			MaxSize:         100,
			CleanupInterval: 1 * time.Minute,
		}
		f.SetCacheConfig(cacheConfig)

		// Verify config was set
		updatedConfig := f.GetCacheConfig()
		assert.True(t, updatedConfig.Enabled)
		assert.Equal(t, 5*time.Minute, updatedConfig.TTL)
		assert.Equal(t, 100, updatedConfig.MaxSize)
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
		f := New(RawAdapter{})
		f.SetCacheConfig(CacheConfig{
			Enabled: true,
			TTL:     1 * time.Minute,
			MaxSize: 100,
		})

		// Build a query
		f.AddFiltersFromString(`id=1 and name="test"`)
		f.Build()

		// First execution (cache miss)
		start := time.Now()
		sql1 := f.GetCachedSqlString(RawContext{Table: "GG"})
		latency1 := time.Since(start)

		// Second execution (cache hit)
		start = time.Now()
		sql2 := f.GetCachedSqlString(RawContext{Table: "GG"})
		latency2 := time.Since(start)

		// Results should be identical
		assert.Equal(t, sql1, sql2)

		// Cache hit should be faster
		assert.True(t, latency2 < latency1)

		// Verify cache stats
		stats := f.GetCacheStats()
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

func TestBatchOperations(t *testing.T) {
	t.Run("BatchProcessor", func(t *testing.T) {
		processor := NewInMemoryBatchProcessor(2, 5*time.Second)

		// Create test queries
		f1 := New(RawAdapter{})
		f1.AddFiltersFromString(`id=1`)
		f1.Build()

		f2 := New(RawAdapter{})
		f2.AddFiltersFromString(`name="test"`)
		f2.Build()

		f3 := New(RawAdapter{})
		f3.AddFiltersFromString(`age>18`)
		f3.Build()

		// Create batch operations
		operations := []BatchOperation{
			{ID: "op1", Query: f1, Context: RawContext{Table: "GG"}, Type: "sql"},
			{ID: "op2", Query: f2, Context: RawContext{Table: "GG"}, Type: "sql"},
			{ID: "op3", Query: f3, Context: RawContext{Table: "GG"}, Type: "sql"},
		}

		// Process batch
		results := processor.Process(operations)

		// Verify results
		assert.Len(t, results, 3)

		for i, result := range results {
			assert.True(t, result.Success)
			assert.NoError(t, result.Error)
			assert.Equal(t, operations[i].ID, result.ID)
			assert.NotNil(t, result.Result)
		}
	})

	t.Run("AsyncBatchProcessing", func(t *testing.T) {
		processor := NewInMemoryBatchProcessor(2, 5*time.Second)

		// Create test queries
		f1 := New(RawAdapter{})
		f1.AddFiltersFromString(`id=1`)
		f1.Build()

		f2 := New(RawAdapter{})
		f2.AddFiltersFromString(`name="test"`)
		f2.Build()

		// Create batch operations
		operations := []BatchOperation{
			{ID: "op1", Query: f1, Context: nil, Type: "sql"},
			{ID: "op2", Query: f2, Context: nil, Type: "sql"},
		}

		// Process async
		resultChan := processor.ProcessAsync(operations)

		// Collect results
		var results []BatchResult
		for result := range resultChan {
			results = append(results, result)
		}

		// Verify results
		assert.Len(t, results, 2)

		for _, result := range results {
			assert.True(t, result.Success)
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.Result)
		}
	})

	t.Run("BatchWithDifferentTypes", func(t *testing.T) {
		processor := NewInMemoryBatchProcessor(2, 5*time.Second)

		f := New(RawAdapter{})
		f.AddFiltersFromString(`id=1`)
		f.Build()

		// Test different operation types
		operations := []BatchOperation{
			{ID: "sql", Query: f, Context: nil, Type: "sql"},
			{ID: "query", Query: f, Context: nil, Type: "query"},
			{ID: "cached_sql", Query: f, Context: nil, Type: "cached_sql"},
			{ID: "cached_query", Query: f, Context: nil, Type: "cached_query"},
		}

		results := processor.Process(operations)

		assert.Len(t, results, 4)
		for _, result := range results {
			assert.True(t, result.Success)
			assert.NoError(t, result.Error)
		}
	})

	t.Run("BatchTimeout", func(t *testing.T) {
		// Create processor with very short timeout
		processor := NewInMemoryBatchProcessor(1, 1*time.Millisecond)

		f := New(RawAdapter{})
		f.AddFiltersFromString(`id=1`)
		f.Build()

		operations := []BatchOperation{
			{ID: "op1", Query: f, Context: nil, Type: "sql"},
		}

		results := processor.Process(operations)

		// Should complete successfully despite short timeout
		assert.Len(t, results, 1)
		assert.True(t, results[0].Success)
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

	t.Run("FigoWithMonitoring", func(t *testing.T) {
		f := New(RawAdapter{})
		monitor := NewPerformanceMonitor(true)
		f.SetPerformanceMonitor(monitor)

		// Build and execute query
		f.AddFiltersFromString(`id=1`)
		f.Build()

		// Execute query (this would normally record metrics)
		_ = f.GetSqlString(nil)

		// Get metrics
		metrics := f.GetMetrics()
		assert.NotNil(t, f.GetPerformanceMonitor())

		// Reset metrics
		f.ResetMetrics()
		metrics = f.GetMetrics()
		assert.Equal(t, int64(0), metrics.QueryCount)
	})
}

func TestPerformanceImprovementsIntegration(t *testing.T) {
	t.Run("CompletePerformanceWorkflow", func(t *testing.T) {
		// Create figo instance with all performance features
		f := New(RawAdapter{})

		// Set up caching
		f.SetCacheConfig(CacheConfig{
			Enabled:         true,
			TTL:             1 * time.Minute,
			MaxSize:         100,
			CleanupInterval: 30 * time.Second,
		})

		// Set up monitoring
		monitor := NewPerformanceMonitor(true)
		f.SetPerformanceMonitor(monitor)

		// Build query
		f.AddFiltersFromString(`id=1 and name="test" and age>18`)
		f.Build()

		// Execute multiple times to test caching
		for i := 0; i < 5; i++ {
			_ = f.GetCachedSqlString(nil)
		}

		// Verify cache stats
		cacheStats := f.GetCacheStats()
		assert.True(t, cacheStats.Hits > 0)
		assert.True(t, cacheStats.HitRate > 0)

		// Verify performance metrics
		metrics := f.GetMetrics()
		assert.True(t, metrics.QueryCount > 0)
	})

	t.Run("BatchProcessingWithCaching", func(t *testing.T) {
		// Create queries with caching enabled
		f1 := New(RawAdapter{})
		f1.SetCacheConfig(CacheConfig{
			Enabled: true,
			TTL:     1 * time.Minute,
			MaxSize: 100,
		})
		f1.AddFiltersFromString(`id=1`)
		f1.Build()

		f2 := New(RawAdapter{})
		f2.SetCacheConfig(CacheConfig{
			Enabled: true,
			TTL:     1 * time.Minute,
			MaxSize: 100,
		})
		f2.AddFiltersFromString(`name="test"`)
		f2.Build()

		// Create batch operations
		processor := NewInMemoryBatchProcessor(2, 5*time.Second)
		operations := []BatchOperation{
			{ID: "op1", Query: f1, Context: nil, Type: "cached_sql"},
			{ID: "op2", Query: f2, Context: nil, Type: "cached_sql"},
		}

		// Process batch
		results := processor.Process(operations)

		// Verify results
		assert.Len(t, results, 2)
		for _, result := range results {
			assert.True(t, result.Success)
			assert.NoError(t, result.Error)
		}
	})
}
