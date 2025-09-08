package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

// User represents a sample user document
type User struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Email       string     `json:"email"`
	Age         int        `json:"age"`
	Score       float64    `json:"score"`
	Status      string     `json:"status"`
	Category    string     `json:"category"`
	Tags        []string   `json:"tags"`
	Price       float64    `json:"price"`
	Discount    int        `json:"discount"`
	Phone       string     `json:"phone"`
	Country     string     `json:"country"`
	LastLogin   time.Time  `json:"last_login"`
	LoginCount  int        `json:"login_count"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	Archived    bool       `json:"archived"`
	Rating      float64    `json:"rating"`
	Description string     `json:"description"`
}

// Product represents a sample product document
type Product struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Category    string    `json:"category"`
	Tags        []string  `json:"tags"`
	InStock     bool      `json:"in_stock"`
	StockCount  int       `json:"stock_count"`
	Rating      float64   `json:"rating"`
	Reviews     int       `json:"reviews"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Order represents a sample order document
type Order struct {
	ID        int       `json:"id"`
	UserID    int       `json:"user_id"`
	ProductID int       `json:"product_id"`
	Quantity  int       `json:"quantity"`
	Total     float64   `json:"total"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func main() {
	elasticsearchURL := os.Getenv("ELASTICSEARCH_URL")
	if elasticsearchURL == "" {
		elasticsearchURL = "http://localhost:9200"
	}

	log.Printf("Connecting to Elasticsearch at %s", elasticsearchURL)

	// Wait for Elasticsearch to be ready
	if err := waitForElasticsearch(elasticsearchURL); err != nil {
		log.Fatalf("Failed to connect to Elasticsearch: %v", err)
	}

	// Create indices
	if err := createIndices(elasticsearchURL); err != nil {
		log.Fatalf("Failed to create indices: %v", err)
	}

	// Seed data
	if err := seedUsers(elasticsearchURL); err != nil {
		log.Fatalf("Failed to seed users: %v", err)
	}

	if err := seedProducts(elasticsearchURL); err != nil {
		log.Fatalf("Failed to seed products: %v", err)
	}

	if err := seedOrders(elasticsearchURL); err != nil {
		log.Fatalf("Failed to seed orders: %v", err)
	}

	log.Println("Data seeding completed successfully!")
}

func waitForElasticsearch(url string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	for i := 0; i < 30; i++ {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		log.Printf("Waiting for Elasticsearch... (attempt %d/30)", i+1)
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timeout waiting for Elasticsearch")
}

func createIndices(url string) error {
	indices := []string{"users", "products", "orders"}

	for _, index := range indices {
		if err := createIndex(url, index); err != nil {
			return fmt.Errorf("failed to create index %s: %w", index, err)
		}
	}

	return nil
}

func createIndex(url, indexName string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	// Check if index exists
	resp, err := client.Head(url + "/" + indexName)
	if err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		log.Printf("Index %s already exists", indexName)
		return nil
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Create index
	req, err := http.NewRequest("PUT", url+"/"+indexName, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to create index %s: status %d", indexName, resp.StatusCode)
	}

	log.Printf("Created index: %s", indexName)
	return nil
}

func seedUsers(url string) error {
	users := generateUsers(100)
	return bulkIndex(url, "users", users)
}

func seedProducts(url string) error {
	products := generateProducts(50)
	return bulkIndex(url, "products", products)
}

func seedOrders(url string) error {
	orders := generateOrders(200)
	return bulkIndex(url, "orders", orders)
}

func generateUsers(count int) []User {
	users := make([]User, count)
	rand.Seed(time.Now().UnixNano())

	names := []string{"John", "Jane", "Bob", "Alice", "Charlie", "Diana", "Eve", "Frank", "Grace", "Henry"}
	emails := []string{"gmail.com", "yahoo.com", "hotmail.com", "outlook.com", "company.com"}
	statuses := []string{"active", "inactive", "pending", "suspended"}
	categories := []string{"tech", "business", "finance", "health", "education"}
	tags := []string{"premium", "basic", "enterprise", "trial", "deprecated", "legacy"}
	countries := []string{"US", "CA", "UK", "DE", "FR", "JP", "AU"}

	for i := 0; i < count; i++ {
		name := names[rand.Intn(len(names))]
		email := fmt.Sprintf("%s%d@%s", name, i, emails[rand.Intn(len(emails))])

		// Some users have deleted_at set
		var deletedAt *time.Time
		if rand.Float32() < 0.1 { // 10% chance
			t := time.Now().Add(-time.Duration(rand.Intn(30)) * 24 * time.Hour)
			deletedAt = &t
		}

		users[i] = User{
			ID:          i + 1,
			Name:        fmt.Sprintf("%s %d", name, i+1),
			Email:       email,
			Age:         18 + rand.Intn(50),
			Score:       rand.Float64() * 100,
			Status:      statuses[rand.Intn(len(statuses))],
			Category:    categories[rand.Intn(len(categories))],
			Tags:        []string{tags[rand.Intn(len(tags))]},
			Price:       rand.Float64() * 1000,
			Discount:    rand.Intn(50),
			Phone:       fmt.Sprintf("+1%d", 1000000000+rand.Intn(9000000000)),
			Country:     countries[rand.Intn(len(countries))],
			LastLogin:   time.Now().Add(-time.Duration(rand.Intn(30)) * 24 * time.Hour),
			LoginCount:  rand.Intn(1000),
			CreatedAt:   time.Now().Add(-time.Duration(rand.Intn(365)) * 24 * time.Hour),
			UpdatedAt:   time.Now().Add(-time.Duration(rand.Intn(30)) * 24 * time.Hour),
			DeletedAt:   deletedAt,
			Archived:    rand.Float32() < 0.05, // 5% chance
			Rating:      rand.Float64() * 5,
			Description: fmt.Sprintf("User description for %s", name),
		}
	}

	return users
}

func generateProducts(count int) []Product {
	products := make([]Product, count)
	rand.Seed(time.Now().UnixNano())

	names := []string{"Laptop", "Phone", "Tablet", "Monitor", "Keyboard", "Mouse", "Headphones", "Camera", "Speaker", "Charger"}
	categories := []string{"Electronics", "Computers", "Accessories", "Audio", "Photography"}
	tags := []string{"new", "sale", "popular", "limited", "premium"}

	for i := 0; i < count; i++ {
		name := names[rand.Intn(len(names))]

		products[i] = Product{
			ID:          i + 1,
			Name:        fmt.Sprintf("%s %d", name, i+1),
			Description: fmt.Sprintf("High-quality %s with advanced features", name),
			Price:       rand.Float64() * 2000,
			Category:    categories[rand.Intn(len(categories))],
			Tags:        []string{tags[rand.Intn(len(tags))]},
			InStock:     rand.Float32() < 0.8, // 80% in stock
			StockCount:  rand.Intn(100),
			Rating:      rand.Float64() * 5,
			Reviews:     rand.Intn(500),
			CreatedAt:   time.Now().Add(-time.Duration(rand.Intn(365)) * 24 * time.Hour),
			UpdatedAt:   time.Now().Add(-time.Duration(rand.Intn(30)) * 24 * time.Hour),
		}
	}

	return products
}

func generateOrders(count int) []Order {
	orders := make([]Order, count)
	rand.Seed(time.Now().UnixNano())

	statuses := []string{"pending", "processing", "shipped", "delivered", "cancelled"}

	for i := 0; i < count; i++ {
		orders[i] = Order{
			ID:        i + 1,
			UserID:    1 + rand.Intn(100), // Reference to users
			ProductID: 1 + rand.Intn(50),  // Reference to products
			Quantity:  1 + rand.Intn(10),
			Total:     rand.Float64() * 1000,
			Status:    statuses[rand.Intn(len(statuses))],
			CreatedAt: time.Now().Add(-time.Duration(rand.Intn(90)) * 24 * time.Hour),
			UpdatedAt: time.Now().Add(-time.Duration(rand.Intn(30)) * 24 * time.Hour),
		}
	}

	return orders
}

func bulkIndex(url, indexName string, documents interface{}) error {
	client := &http.Client{Timeout: 60 * time.Second}

	// Create bulk request
	var bulkBody bytes.Buffer

	// Handle different document types
	switch docs := documents.(type) {
	case []User:
		for i, doc := range docs {
			indexAction := map[string]interface{}{
				"index": map[string]interface{}{
					"_index": indexName,
					"_id":    i + 1,
				},
			}

			actionJSON, _ := json.Marshal(indexAction)
			bulkBody.Write(actionJSON)
			bulkBody.WriteString("\n")

			docJSON, _ := json.Marshal(doc)
			bulkBody.Write(docJSON)
			bulkBody.WriteString("\n")
		}
		log.Printf("Indexed %d users to %s", len(docs), indexName)

	case []Product:
		for i, doc := range docs {
			indexAction := map[string]interface{}{
				"index": map[string]interface{}{
					"_index": indexName,
					"_id":    i + 1,
				},
			}

			actionJSON, _ := json.Marshal(indexAction)
			bulkBody.Write(actionJSON)
			bulkBody.WriteString("\n")

			docJSON, _ := json.Marshal(doc)
			bulkBody.Write(docJSON)
			bulkBody.WriteString("\n")
		}
		log.Printf("Indexed %d products to %s", len(docs), indexName)

	case []Order:
		for i, doc := range docs {
			indexAction := map[string]interface{}{
				"index": map[string]interface{}{
					"_index": indexName,
					"_id":    i + 1,
				},
			}

			actionJSON, _ := json.Marshal(indexAction)
			bulkBody.Write(actionJSON)
			bulkBody.WriteString("\n")

			docJSON, _ := json.Marshal(doc)
			bulkBody.Write(docJSON)
			bulkBody.WriteString("\n")
		}
		log.Printf("Indexed %d orders to %s", len(docs), indexName)

	default:
		return fmt.Errorf("unsupported document type")
	}

	// Send bulk request
	resp, err := client.Post(url+"/_bulk", "application/x-ndjson", &bulkBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("bulk index failed: status %d", resp.StatusCode)
	}

	return nil
}
