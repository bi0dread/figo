# Elasticsearch Testing with figo

This directory contains a complete Docker Compose setup for testing the figo Elasticsearch adapter with real data.

## 🚀 Quick Start

### 1. Start the Environment
```bash
./start-elasticsearch.sh
```

This will:
- Start Elasticsearch (port 9200)
- Start Kibana (port 5601) 
- Seed the database with dummy data
- Wait for services to be ready

### 2. Test the figo Elasticsearch Adapter
```bash
go run ./cmd/elasticsearch-test/main.go
```

## 📊 Available Data

The seeder creates three indices with realistic dummy data:

### Users Index (`users`)
- **100 users** with fields: id, name, email, age, score, status, category, tags, price, discount, phone, country, last_login, login_count, created_at, updated_at, deleted_at, archived, rating, description

### Products Index (`products`)
- **50 products** with fields: id, name, description, price, category, tags, in_stock, stock_count, rating, reviews, created_at, updated_at

### Orders Index (`orders`)
- **200 orders** with fields: id, user_id, product_id, quantity, total, status, created_at, updated_at

## 🧪 Test Examples

The test script demonstrates all figo Elasticsearch adapter features:

### Basic Queries
```go
// Term query
f.AddFiltersFromString(`status = "active"`)

// Range query  
f.AddFiltersFromString(`age > 25 and score >= 80`)

// Wildcard query
f.AddFiltersFromString(`name =^ "%John%"`)

// Terms query
f.AddFiltersFromString(`category <in> [tech,business,finance]`)

// Between query
f.AddFiltersFromString(`price <bet> (100..500)`)

// Exists query
f.AddFiltersFromString(`last_login <notnull>`)
```

### Complex Queries
```go
// Nested boolean query
f.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
```

### Pagination and Sorting
```go
// With pagination and sorting
f.AddFiltersFromString(`status = "active" sort=score:desc,age:asc page=skip:0,take:5`)
```

## 🌐 Web Interfaces

### Elasticsearch
- **URL**: http://localhost:9200
- **Health Check**: http://localhost:9200/_cluster/health
- **Indices**: http://localhost:9200/_cat/indices

### Kibana
- **URL**: http://localhost:5601
- **Setup**: Create index patterns for `users`, `products`, `orders`

## 🔧 Manual Testing

### Using curl
```bash
# Get cluster health
curl http://localhost:9200/_cluster/health

# List indices
curl http://localhost:9200/_cat/indices

# Search users
curl -X POST http://localhost:9200/users/_search \
  -H "Content-Type: application/json" \
  -d '{"query":{"term":{"status":"active"}}}'

# Count documents
curl http://localhost:9200/users/_count
```

### Using the figo Go API
```go
package main

import (
    "fmt"
    "github.com/bi0dread/figo/v3"
)

func main() {
    // Create figo instance with Elasticsearch adapter
    f := figo.New(figo.ElasticsearchAdapter{})
    
    // Add your DSL filters
    f.AddFiltersFromString(`name = "John" and age > 25`)
    
    // Build the query
    f.Build()
    
    // Get the Elasticsearch query
    query := figo.BuildElasticsearchQuery(f)
    
    // Convert to JSON
    jsonStr, _ := figo.GetElasticsearchQueryString(f)
    fmt.Println(jsonStr)
}
```

## 🛠️ Development

### Rebuilding the Seeder
```bash
docker-compose build data-seeder
docker-compose up data-seeder
```

### Viewing Logs
```bash
# All services
docker-compose logs -f

# Specific service
docker-compose logs -f elasticsearch
docker-compose logs -f kibana
docker-compose logs -f data-seeder
```

### Stopping Everything
```bash
docker-compose down
```

### Clean Restart
```bash
docker-compose down -v  # Removes volumes (data)
docker-compose up -d
```

## 📁 File Structure

```
├── docker-compose.yml              # Docker Compose configuration
├── Dockerfile.seeder              # Seeder container definition
├── elasticsearch/
│   └── elasticsearch.yml          # Elasticsearch configuration
├── cmd/
│   ├── seeder/
│   │   └── main.go               # Data seeding script
│   └── elasticsearch-test/
│       └── main.go               # figo adapter test script
├── start-elasticsearch.sh         # Quick start script
└── ELASTICSEARCH_TESTING.md       # This file
```

## 🐛 Troubleshooting

### Elasticsearch not starting
- Check if port 9200 is available
- Increase Docker memory limits
- Check logs: `docker-compose logs elasticsearch`

### Data not seeding
- Wait for Elasticsearch to be fully ready
- Check seeder logs: `docker-compose logs data-seeder`
- Manually run seeder: `docker-compose up data-seeder`

### Kibana not accessible
- Wait for Kibana to start (takes longer than Elasticsearch)
- Check logs: `docker-compose logs kibana`
- Access via http://localhost:5601

## 🎯 Next Steps

1. **Explore the data** in Kibana
2. **Run the test script** to see figo in action
3. **Modify the test script** to try your own queries
4. **Add more data** by modifying the seeder
5. **Create custom indices** for your use case

Happy testing! 🚀
