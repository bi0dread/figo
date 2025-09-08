package figo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestElasticsearchStressTest tests the figo Elasticsearch adapter with stress scenarios
func TestElasticsearchStressTest(t *testing.T) {
	// Skip if Elasticsearch is not available
	if !isElasticsearchAvailable() {
		t.Skip("Elasticsearch not available, skipping stress test")
	}

	// Setup test data
	setupStressTestData(t)

	t.Run("LargeDatasetQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`id > 0 and id <= 1000`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Should find all 1000 users
		if results.Hits.Total.Value != 1000 {
			t.Errorf("Expected 1000 users, found %d", results.Hits.Total.Value)
		}

		t.Logf("Successfully queried %d users from large dataset", results.Hits.Total.Value)
	})

	t.Run("ComplexNestedQueryWithLargeDataset", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`((category = "tech" and score > 80) or (category = "business" and age > 30)) and (status = "active" or status = "pending") and price <bet> (100..1000)`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "bool") {
			t.Error("Expected bool query in generated JSON")
		}

		t.Logf("Complex query on large dataset found %d matching users", results.Hits.Total.Value)
	})

	t.Run("PaginationStressTest", func(t *testing.T) {
		// Test pagination with large dataset
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`id > 0 sort=id:asc page=skip:500,take:100`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify pagination
		if query.From != 500 {
			t.Errorf("Expected from=500, got %d", query.From)
		}
		if query.Size != 100 {
			t.Errorf("Expected size=100, got %d", query.Size)
		}

		// Should return exactly 100 results
		if len(results.Hits.Hits) != 100 {
			t.Errorf("Expected 100 results, got %d", len(results.Hits.Hits))
		}

		t.Logf("Pagination test: returned %d results starting from offset 500", len(results.Hits.Hits))
	})

	t.Run("MultipleSortFields", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`id > 0 sort=category:asc,score:desc,age:asc,price:desc page=skip:0,take:50`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify sorting
		if len(query.Sort) != 4 {
			t.Errorf("Expected 4 sort fields, got %d", len(query.Sort))
		}

		t.Logf("Multi-field sorting test: returned %d results with 4 sort fields", len(results.Hits.Hits))
	})

	t.Run("FieldSelectionStressTest", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddSelectFields("id", "name", "email", "category", "score", "age", "price", "status")
		f.AddFiltersFromString(`id > 0`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify field selection
		if len(query.Source) != 8 {
			t.Errorf("Expected 8 source fields, got %d", len(query.Source))
		}

		t.Logf("Field selection test: returned %d results with 8 selected fields", results.Hits.Total.Value)
	})

	t.Run("RegexStressTest", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`email =~ ".*@(gmail|yahoo|outlook)\\.com$"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "regexp") {
			t.Error("Expected regexp query in generated JSON")
		}

		t.Logf("Regex stress test: found %d users with matching email patterns", results.Hits.Total.Value)
	})

	t.Run("TermsStressTest", func(t *testing.T) {
		// Test with many terms
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`category <in> [tech,business,finance,health,education,entertainment,sports,travel,food,automotive]`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "terms") {
			t.Error("Expected terms query in generated JSON")
		}

		t.Logf("Terms stress test: found %d users in multiple categories", results.Hits.Total.Value)
	})

	t.Run("RangeStressTest", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`age >= 18 and age <= 65 and score >= 50 and score <= 100 and price >= 100 and price <= 1000`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "range") {
			t.Error("Expected range query in generated JSON")
		}

		t.Logf("Range stress test: found %d users within specified ranges", results.Hits.Total.Value)
	})

	t.Run("NotStressTest", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`status != "inactive" and category != "deprecated" and score != 0`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "must_not") {
			t.Error("Expected must_not clause in generated JSON")
		}

		t.Logf("Not stress test: found %d users matching exclusion criteria", results.Hits.Total.Value)
	})

	t.Run("NullStressTest", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`deleted_at <null> and last_login <notnull> and phone <notnull>`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "stress_users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "exists") {
			t.Error("Expected exists clause in generated JSON")
		}

		t.Logf("Null stress test: found %d users with proper null/not-null field handling", results.Hits.Total.Value)
	})
}

func setupStressTestData(t *testing.T) {
	// Create stress_users index
	createStressIndex(t, "stress_users")

	// Seed large dataset
	seedStressTestData(t)
}

func createStressIndex(t *testing.T, indexName string) {
	client := &http.Client{Timeout: 10 * time.Second}

	// Check if index exists
	resp, err := client.Head("http://localhost:9200/" + indexName)
	if err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		return // Index already exists
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Create index with mapping
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"id":         map[string]string{"type": "integer"},
				"name":       map[string]string{"type": "text"},
				"email":      map[string]string{"type": "keyword"},
				"age":        map[string]string{"type": "integer"},
				"score":      map[string]string{"type": "float"},
				"status":     map[string]string{"type": "keyword"},
				"category":   map[string]string{"type": "keyword"},
				"price":      map[string]string{"type": "float"},
				"phone":      map[string]string{"type": "keyword"},
				"last_login": map[string]string{"type": "date"},
				"created_at": map[string]string{"type": "date"},
				"deleted_at": map[string]string{"type": "date"},
			},
		},
	}

	jsonData, _ := json.Marshal(mapping)
	req, _ := http.NewRequest("PUT", "http://localhost:9200/"+indexName, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Failed to create stress index: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		t.Fatalf("Failed to create stress index: status %d", resp.StatusCode)
	}
}

func seedStressTestData(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}

	// Generate 1000 test users
	users := make([]map[string]interface{}, 1000)
	categories := []string{"tech", "business", "finance", "health", "education", "entertainment", "sports", "travel", "food", "automotive"}
	statuses := []string{"active", "inactive", "pending", "suspended"}
	emailDomains := []string{"gmail.com", "yahoo.com", "outlook.com", "company.com", "example.com"}

	for i := 0; i < 1000; i++ {
		category := categories[i%len(categories)]
		status := statuses[i%len(statuses)]
		emailDomain := emailDomains[i%len(emailDomains)]

		// Some users have deleted_at set
		var deletedAt interface{}
		if i%10 == 0 { // 10% have deleted_at
			deletedAt = "2024-01-01T00:00:00Z"
		}

		users[i] = map[string]interface{}{
			"id":         i + 1,
			"name":       "User " + string(rune(i+1)),
			"email":      "user" + string(rune(i+1)) + "@" + emailDomain,
			"age":        18 + (i % 50),
			"score":      float64(50 + (i % 50)),
			"status":     status,
			"category":   category,
			"price":      float64(100 + (i % 900)),
			"phone":      "+1" + string(rune(1000000000+i)),
			"last_login": "2024-01-15T10:30:00Z",
			"created_at": "2023-06-01T09:00:00Z",
			"deleted_at": deletedAt,
		}
	}

	// Bulk index in batches of 100
	batchSize := 100
	for i := 0; i < len(users); i += batchSize {
		end := i + batchSize
		if end > len(users) {
			end = len(users)
		}

		var bulkBody bytes.Buffer
		for j := i; j < end; j++ {
			// Index action
			indexAction := map[string]interface{}{
				"index": map[string]interface{}{
					"_index": "stress_users",
					"_id":    j + 1,
				},
			}

			actionJSON, _ := json.Marshal(indexAction)
			bulkBody.Write(actionJSON)
			bulkBody.WriteString("\n")

			// Document
			docJSON, _ := json.Marshal(users[j])
			bulkBody.Write(docJSON)
			bulkBody.WriteString("\n")
		}

		// Send bulk request
		resp, err := client.Post("http://localhost:9200/_bulk", "application/x-ndjson", &bulkBody)
		if err != nil {
			t.Fatalf("Failed to seed stress data batch %d: %v", i/batchSize+1, err)
		}
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			t.Fatalf("Failed to seed stress data batch %d: status %d", i/batchSize+1, resp.StatusCode)
		}
	}

	// Wait for indexing to complete
	time.Sleep(2 * time.Second)
	t.Logf("Successfully seeded %d users for stress testing", len(users))
}
