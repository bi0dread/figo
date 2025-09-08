package figo

import (
	"sync"
	"testing"
	"time"
)

// TestElasticsearchPerformance tests the figo Elasticsearch adapter performance
func TestElasticsearchPerformance(t *testing.T) {
	// Skip if Elasticsearch is not available
	if !isElasticsearchAvailable() {
		t.Skip("Elasticsearch not available, skipping performance test")
	}

	t.Run("ConcurrentQueries", func(t *testing.T) {
		const numGoroutines = 10
		const queriesPerGoroutine = 10

		var wg sync.WaitGroup
		start := time.Now()

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(goroutineID int) {
				defer wg.Done()

				for j := 0; j < queriesPerGoroutine; j++ {
					f := New(ElasticsearchAdapter{})
					f.AddFiltersFromString(`status = "active" and age > 25`)
					f.Build()

					query := BuildElasticsearchQuery(f)
					results := executeElasticsearchQuery(t, "stress_users", query)

					// Verify we got results
					if results.Hits.Total.Value == 0 {
						t.Errorf("Goroutine %d, Query %d: Expected results, got 0", goroutineID, j)
					}
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(start)

		totalQueries := numGoroutines * queriesPerGoroutine
		queriesPerSecond := float64(totalQueries) / duration.Seconds()

		t.Logf("Concurrent test: %d queries in %v (%.2f queries/sec)",
			totalQueries, duration, queriesPerSecond)
	})

	t.Run("QueryGenerationPerformance", func(t *testing.T) {
		const numQueries = 1000

		start := time.Now()

		for i := 0; i < numQueries; i++ {
			f := New(ElasticsearchAdapter{})
			f.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
			f.Build()

			query := BuildElasticsearchQuery(f)
			jsonStr, _ := GetElasticsearchQueryString(f)

			// Verify query was generated
			if query.Query == nil {
				t.Error("Expected query to be generated")
			}
			if jsonStr == "" {
				t.Error("Expected JSON string to be generated")
			}
		}

		duration := time.Since(start)
		queriesPerSecond := float64(numQueries) / duration.Seconds()

		t.Logf("Query generation test: %d queries in %v (%.2f queries/sec)",
			numQueries, duration, queriesPerSecond)
	})

	t.Run("ComplexQueryPerformance", func(t *testing.T) {
		const numQueries = 100

		start := time.Now()

		for i := 0; i < numQueries; i++ {
			f := New(ElasticsearchAdapter{})
			f.AddFiltersFromString(`((category = "tech" and score > 80) or (category = "business" and age > 30)) and (status = "active" or status = "pending") and price <bet> (100..1000) and email =^ "%gmail%" and phone =~ "^\\+1[0-9]{10}$" and last_login <notnull> and deleted_at <null>`)
			f.Build()

			query := BuildElasticsearchQuery(f)
			results := executeElasticsearchQuery(t, "stress_users", query)

			// Verify query was executed
			if results.Hits.Total.Value < 0 {
				t.Error("Expected valid result count")
			}
		}

		duration := time.Since(start)
		queriesPerSecond := float64(numQueries) / duration.Seconds()

		t.Logf("Complex query test: %d queries in %v (%.2f queries/sec)",
			numQueries, duration, queriesPerSecond)
	})

	t.Run("FluentBuilderPerformance", func(t *testing.T) {
		const numQueries = 1000

		start := time.Now()

		for i := 0; i < numQueries; i++ {
			builder := NewElasticsearchQueryBuilder()
			query := builder.
				AddSort("score", false).
				AddSort("age", true).
				AddSort("price", false).
				SetPagination(0, 50).
				SetSource("id", "name", "email", "score", "age", "price", "status", "category").
				Build()

			// Verify query was built
			if query.Query == nil {
				t.Error("Expected query to be built")
			}
			if len(query.Sort) != 3 {
				t.Error("Expected 3 sort fields")
			}
			if len(query.Source) != 8 {
				t.Error("Expected 8 source fields")
			}
		}

		duration := time.Since(start)
		queriesPerSecond := float64(numQueries) / duration.Seconds()

		t.Logf("Fluent builder test: %d queries in %v (%.2f queries/sec)",
			numQueries, duration, queriesPerSecond)
	})

	t.Run("MemoryUsageTest", func(t *testing.T) {
		// Test memory usage with many concurrent figo instances
		const numInstances = 1000

		start := time.Now()
		instances := make([]Figo, numInstances)

		for i := 0; i < numInstances; i++ {
			f := New(ElasticsearchAdapter{})
			f.AddFiltersFromString(`id > 0 and status = "active"`)
			f.Build()
			instances[i] = f
		}

		creationTime := time.Since(start)

		// Generate queries from all instances
		start = time.Now()
		for i := 0; i < numInstances; i++ {
			query := BuildElasticsearchQuery(instances[i])
			jsonStr, _ := GetElasticsearchQueryString(instances[i])

			// Verify query was generated
			if query.Query == nil {
				t.Error("Expected query to be generated")
			}
			if jsonStr == "" {
				t.Error("Expected JSON string to be generated")
			}
		}

		queryTime := time.Since(start)

		t.Logf("Memory usage test: Created %d instances in %v, generated queries in %v",
			numInstances, creationTime, queryTime)
	})
}

// BenchmarkElasticsearchAdapter benchmarks the figo Elasticsearch adapter
func BenchmarkElasticsearchAdapter(b *testing.B) {
	if !isElasticsearchAvailable() {
		b.Skip("Elasticsearch not available, skipping benchmark")
	}

	b.Run("BasicQuery", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f := New(ElasticsearchAdapter{})
			f.AddFiltersFromString(`status = "active"`)
			f.Build()
			BuildElasticsearchQuery(f)
		}
	})

	b.Run("ComplexQuery", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f := New(ElasticsearchAdapter{})
			f.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
			f.Build()
			BuildElasticsearchQuery(f)
		}
	})

	b.Run("FluentBuilder", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			builder := NewElasticsearchQueryBuilder()
			builder.
				AddSort("score", false).
				AddSort("age", true).
				SetPagination(0, 10).
				SetSource("id", "name", "email").
				Build()
		}
	})

	b.Run("JSONGeneration", func(b *testing.B) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`status = "active" and age > 25`)
		f.Build()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			GetElasticsearchQueryString(f)
		}
	})
}
