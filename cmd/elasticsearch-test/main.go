package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/bi0dread/figo/v3"
)

func main() {
	elasticsearchURL := "http://localhost:9200"

	log.Println("Testing figo Elasticsearch adapter with real data...")

	// Test 1: Basic term query
	log.Println("\n=== Test 1: Basic Term Query ===")
	testBasicTermQuery(elasticsearchURL)

	// Test 2: Range query
	log.Println("\n=== Test 2: Range Query ===")
	testRangeQuery(elasticsearchURL)

	// Test 3: Wildcard query
	log.Println("\n=== Test 3: Wildcard Query ===")
	testWildcardQuery(elasticsearchURL)

	// Test 4: Terms query
	log.Println("\n=== Test 4: Terms Query ===")
	testTermsQuery(elasticsearchURL)

	// Test 5: Between query
	log.Println("\n=== Test 5: Between Query ===")
	testBetweenQuery(elasticsearchURL)

	// Test 6: Exists query
	log.Println("\n=== Test 6: Exists Query ===")
	testExistsQuery(elasticsearchURL)

	// Test 7: Complex nested query
	log.Println("\n=== Test 7: Complex Nested Query ===")
	testComplexQuery(elasticsearchURL)

	// Test 8: Pagination and sorting
	log.Println("\n=== Test 8: Pagination and Sorting ===")
	testPaginationAndSorting(elasticsearchURL)

	log.Println("\n=== All tests completed! ===")
}

func testBasicTermQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`status = "active"`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: status = \"active\"")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d active users", results.Hits.Total.Value)
}

func testRangeQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`age > 25 and score >= 80`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: age > 25 and score >= 80")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users with age > 25 and score >= 80", results.Hits.Total.Value)
}

func testWildcardQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`name =^ "%John%"`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: name =^ \"%%John%%\"")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users with name containing 'John'", results.Hits.Total.Value)
}

func testTermsQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`category <in> [tech,business,finance]`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: category <in> [tech,business,finance]")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users in tech, business, or finance categories", results.Hits.Total.Value)
}

func testBetweenQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`price <bet> (100..500)`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: price <bet> (100..500)")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users with price between 100 and 500", results.Hits.Total.Value)
}

func testExistsQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`last_login <notnull>`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: last_login <notnull>")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users with last_login field", results.Hits.Total.Value)
}

func testComplexQuery(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: ((name =^ \"%%John%%\" or email =^ \"%%gmail%%\") and (age >= 18 and age <= 65)) or (status = \"active\" and score > 80)")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d users matching complex criteria", results.Hits.Total.Value)
}

func testPaginationAndSorting(url string) {
	f := figo.New(figo.ElasticsearchAdapter{})
	f.AddFiltersFromString(`status = "active" sort=score:desc,age:asc page=skip:0,take:5`)
	f.Build()

	query := figo.BuildElasticsearchQuery(f)
	jsonStr, _ := figo.GetElasticsearchQueryString(f)

	log.Printf("DSL: status = \"active\" sort=score:desc,age:asc page=skip:0,take:5")
	log.Printf("Generated Query: %s", jsonStr)

	// Execute query
	results := executeQuery(url, "users", query)
	log.Printf("Found %d active users (top 5 by score desc, age asc)", results.Hits.Total.Value)

	// Show first few results
	if len(results.Hits.Hits) > 0 {
		log.Printf("First result: %s", results.Hits.Hits[0].Source)
	}
}

// Elasticsearch response structures
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

func executeQuery(url, index string, query figo.ElasticsearchQuery) ElasticsearchResponse {
	client := &http.Client{Timeout: 30 * time.Second}

	// Convert query to JSON
	jsonData, err := json.Marshal(query)
	if err != nil {
		log.Printf("Error marshaling query: %v", err)
		return ElasticsearchResponse{}
	}

	// Execute search
	resp, err := client.Post(url+"/"+index+"/_search", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Error executing query: %v", err)
		return ElasticsearchResponse{}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("Query failed with status: %d", resp.StatusCode)
		return ElasticsearchResponse{}
	}

	var result ElasticsearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Error decoding response: %v", err)
		return ElasticsearchResponse{}
	}

	return result
}
