package figo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestElasticsearchIntegration tests the figo Elasticsearch adapter against a real Elasticsearch instance
func TestElasticsearchIntegration(t *testing.T) {
	// Skip if Elasticsearch is not available
	if !isElasticsearchAvailable() {
		t.Skip("Elasticsearch not available, skipping integration test")
	}

	// Setup test data
	setupTestData(t)

	t.Run("BasicTermQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`status = "active"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		if results.Hits.Total.Value == 0 {
			t.Error("Expected to find active users, but found none")
		}

		// Verify the query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "term") {
			t.Error("Expected term query in generated JSON")
		}
		if !contains(jsonStr, "active") {
			t.Error("Expected 'active' value in generated JSON")
		}

		t.Logf("Found %d active users", results.Hits.Total.Value)
	})

	t.Run("RangeQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`age > 25 and score >= 80`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "range") {
			t.Error("Expected range query in generated JSON")
		}
		if !contains(jsonStr, "gt") {
			t.Error("Expected 'gt' operator in generated JSON")
		}
		if !contains(jsonStr, "gte") {
			t.Error("Expected 'gte' operator in generated JSON")
		}

		t.Logf("Found %d users with age > 25 and score >= 80", results.Hits.Total.Value)
	})

	t.Run("WildcardQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`email =^ "%gmail%"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "wildcard") {
			t.Error("Expected wildcard query in generated JSON")
		}
		if !contains(jsonStr, "*gmail*") {
			t.Error("Expected wildcard pattern in generated JSON")
		}

		t.Logf("Found %d users with gmail email", results.Hits.Total.Value)
	})

	t.Run("TermsQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`category <in> [tech,business,finance]`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "terms") {
			t.Error("Expected terms query in generated JSON")
		}
		if !contains(jsonStr, "tech") {
			t.Error("Expected 'tech' in terms array")
		}

		t.Logf("Found %d users in specified categories", results.Hits.Total.Value)
	})

	t.Run("BetweenQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`price <bet> (100..500)`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "range") {
			t.Error("Expected range query in generated JSON")
		}
		if !contains(jsonStr, "gte") {
			t.Error("Expected 'gte' in range query")
		}
		if !contains(jsonStr, "lte") {
			t.Error("Expected 'lte' in range query")
		}

		t.Logf("Found %d users with price between 100 and 500", results.Hits.Total.Value)
	})

	t.Run("ExistsQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`last_login <notnull>`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "exists") {
			t.Error("Expected exists query in generated JSON")
		}

		t.Logf("Found %d users with last_login field", results.Hits.Total.Value)
	})

	t.Run("ComplexNestedQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "bool") {
			t.Error("Expected bool query in generated JSON")
		}
		if !contains(jsonStr, "should") {
			t.Error("Expected 'should' clause in bool query")
		}
		if !contains(jsonStr, "must") {
			t.Error("Expected 'must' clause in bool query")
		}

		t.Logf("Found %d users matching complex criteria", results.Hits.Total.Value)
	})

	t.Run("PaginationAndSorting", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`status = "active" sort=score:desc,age:asc page=skip:0,take:3`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify pagination
		if query.Size != 3 {
			t.Errorf("Expected size 3, got %d", query.Size)
		}

		// Verify sorting
		if len(query.Sort) != 2 {
			t.Errorf("Expected 2 sort fields, got %d", len(query.Sort))
		}

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "sort") {
			t.Error("Expected sort clause in generated JSON")
		}
		if !contains(jsonStr, "desc") {
			t.Error("Expected 'desc' in sort order")
		}
		if !contains(jsonStr, "asc") {
			t.Error("Expected 'asc' in sort order")
		}

		t.Logf("Found %d active users (limited to 3, sorted by score desc, age asc)", results.Hits.Total.Value)

		// Show first result if available
		if len(results.Hits.Hits) > 0 {
			t.Logf("First result: %s", results.Hits.Hits[0].Source)
		}
	})

	t.Run("FieldSelection", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddSelectFields("id", "name", "email", "score")
		f.AddFiltersFromString(`status = "active"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify field selection
		if len(query.Source) != 4 {
			t.Errorf("Expected 4 source fields, got %d", len(query.Source))
		}

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "_source") {
			t.Error("Expected _source clause in generated JSON")
		}

		t.Logf("Found %d active users with field selection", results.Hits.Total.Value)
	})

	t.Run("FluentBuilder", func(t *testing.T) {
		builder := NewElasticsearchQueryBuilder()
		query := builder.
			AddSort("score", false). // desc
			AddSort("age", true).    // asc
			SetPagination(0, 5).
			SetSource("id", "name", "score").
			Build()

		results := executeElasticsearchQuery(t, "users", query)

		// Verify builder results
		if query.Size != 5 {
			t.Errorf("Expected size 5, got %d", query.Size)
		}
		if len(query.Sort) != 2 {
			t.Errorf("Expected 2 sort fields, got %d", len(query.Sort))
		}
		if len(query.Source) != 3 {
			t.Errorf("Expected 3 source fields, got %d", len(query.Source))
		}

		t.Logf("Fluent builder found %d users", results.Hits.Total.Value)
	})

	t.Run("RegexQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`phone =~ "^\\+1[0-9]{10}$"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "regexp") {
			t.Error("Expected regexp query in generated JSON")
		}

		t.Logf("Found %d users with valid phone format", results.Hits.Total.Value)
	})

	t.Run("NotQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`status != "inactive"`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "must_not") {
			t.Error("Expected must_not clause in generated JSON")
		}

		t.Logf("Found %d users that are not inactive", results.Hits.Total.Value)
	})

	t.Run("NullQuery", func(t *testing.T) {
		f := New(ElasticsearchAdapter{})
		f.AddFiltersFromString(`deleted_at <null>`)
		f.Build()

		query := BuildElasticsearchQuery(f)
		results := executeElasticsearchQuery(t, "users", query)

		// Verify query structure
		jsonStr, _ := GetElasticsearchQueryString(f)
		if !contains(jsonStr, "must_not") {
			t.Error("Expected must_not clause for null query")
		}
		if !contains(jsonStr, "exists") {
			t.Error("Expected exists clause for null query")
		}

		t.Logf("Found %d users with null deleted_at", results.Hits.Total.Value)
	})
}

// Helper functions for integration testing

func isElasticsearchAvailable() bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:9200/_cluster/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func setupTestData(t *testing.T) {
	// Create users index if it doesn't exist
	createIndex(t, "users")

	// Seed test data
	seedTestData(t)
}

func createIndex(t *testing.T, indexName string) {
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
		t.Fatalf("Failed to create index: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		t.Fatalf("Failed to create index: status %d", resp.StatusCode)
	}
}

func seedTestData(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}

	// Test data
	users := []map[string]interface{}{
		{
			"id":         1,
			"name":       "John Doe",
			"email":      "john@example.com",
			"age":        30,
			"score":      85.5,
			"status":     "active",
			"category":   "tech",
			"price":      299.99,
			"phone":      "+1234567890",
			"last_login": "2024-01-15T10:30:00Z",
			"created_at": "2023-06-01T09:00:00Z",
			"deleted_at": nil,
		},
		{
			"id":         2,
			"name":       "Jane Smith",
			"email":      "jane@gmail.com",
			"age":        25,
			"score":      92.0,
			"status":     "active",
			"category":   "business",
			"price":      199.99,
			"phone":      "+1987654321",
			"last_login": "2024-01-14T14:20:00Z",
			"created_at": "2023-07-15T11:30:00Z",
			"deleted_at": nil,
		},
		{
			"id":         3,
			"name":       "Bob Johnson",
			"email":      "bob@company.com",
			"age":        35,
			"score":      78.5,
			"status":     "inactive",
			"category":   "finance",
			"price":      399.99,
			"phone":      "+1555123456",
			"last_login": "2024-01-10T16:45:00Z",
			"created_at": "2023-05-20T08:15:00Z",
			"deleted_at": "2024-01-01T00:00:00Z",
		},
		{
			"id":         4,
			"name":       "Alice Brown",
			"email":      "alice@yahoo.com",
			"age":        28,
			"score":      88.0,
			"status":     "active",
			"category":   "tech",
			"price":      249.99,
			"phone":      "+1444987654",
			"last_login": "2024-01-16T09:15:00Z",
			"created_at": "2023-08-10T13:45:00Z",
			"deleted_at": nil,
		},
		{
			"id":         5,
			"name":       "Charlie Wilson",
			"email":      "charlie@outlook.com",
			"age":        42,
			"score":      95.5,
			"status":     "active",
			"category":   "business",
			"price":      149.99,
			"phone":      "+1333222111",
			"last_login": "2024-01-16T12:00:00Z",
			"created_at": "2023-04-05T10:20:00Z",
			"deleted_at": nil,
		},
	}

	// Bulk index the data
	var bulkBody bytes.Buffer
	for i, user := range users {
		// Index action
		indexAction := map[string]interface{}{
			"index": map[string]interface{}{
				"_index": "users",
				"_id":    i + 1,
			},
		}

		actionJSON, _ := json.Marshal(indexAction)
		bulkBody.Write(actionJSON)
		bulkBody.WriteString("\n")

		// Document
		docJSON, _ := json.Marshal(user)
		bulkBody.Write(docJSON)
		bulkBody.WriteString("\n")
	}

	// Send bulk request
	resp, err := client.Post("http://localhost:9200/_bulk", "application/x-ndjson", &bulkBody)
	if err != nil {
		t.Fatalf("Failed to seed data: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		t.Fatalf("Failed to seed data: status %d", resp.StatusCode)
	}

	// Wait for indexing to complete
	time.Sleep(1 * time.Second)
}

func executeElasticsearchQuery(t *testing.T, index string, query ElasticsearchQuery) ElasticsearchResponse {
	client := &http.Client{Timeout: 30 * time.Second}

	// Convert query to JSON
	jsonData, err := json.Marshal(query)
	if err != nil {
		t.Fatalf("Failed to marshal query: %v", err)
	}

	// Execute search
	resp, err := client.Post("http://localhost:9200/"+index+"/_search", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatalf("Failed to execute query: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		t.Fatalf("Query failed with status: %d", resp.StatusCode)
	}

	var result ElasticsearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	return result
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			containsSubstring(s, substr))))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ElasticsearchResponse represents the structure of an Elasticsearch search response
type ElasticsearchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			Source json.RawMessage `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}
