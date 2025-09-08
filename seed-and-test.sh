#!/bin/bash

echo "🌱 Seeding Elasticsearch with test data..."

# Create users index
curl -X PUT "localhost:9200/users" -H "Content-Type: application/json" -d '{
  "mappings": {
    "properties": {
      "id": {"type": "integer"},
      "name": {"type": "text"},
      "email": {"type": "keyword"},
      "age": {"type": "integer"},
      "score": {"type": "float"},
      "status": {"type": "keyword"},
      "category": {"type": "keyword"},
      "price": {"type": "float"},
      "last_login": {"type": "date"},
      "created_at": {"type": "date"}
    }
  }
}'

# Seed some test data
echo "📝 Adding test users..."
curl -X POST "localhost:9200/users/_bulk" -H "Content-Type: application/x-ndjson" --data-binary @- << EOF
{"index":{"_id":1}}
{"id":1,"name":"John Doe","email":"john@example.com","age":30,"score":85.5,"status":"active","category":"tech","price":299.99,"last_login":"2024-01-15T10:30:00Z","created_at":"2023-06-01T09:00:00Z"}
{"index":{"_id":2}}
{"id":2,"name":"Jane Smith","email":"jane@gmail.com","age":25,"score":92.0,"status":"active","category":"business","price":199.99,"last_login":"2024-01-14T14:20:00Z","created_at":"2023-07-15T11:30:00Z"}
{"index":{"_id":3}}
{"id":3,"name":"Bob Johnson","email":"bob@company.com","age":35,"score":78.5,"status":"inactive","category":"finance","price":399.99,"last_login":"2024-01-10T16:45:00Z","created_at":"2023-05-20T08:15:00Z"}
{"index":{"_id":4}}
{"id":4,"name":"Alice Brown","email":"alice@yahoo.com","age":28,"score":88.0,"status":"active","category":"tech","price":249.99,"last_login":"2024-01-16T09:15:00Z","created_at":"2023-08-10T13:45:00Z"}
{"index":{"_id":5}}
{"id":5,"name":"Charlie Wilson","email":"charlie@outlook.com","age":42,"score":95.5,"status":"active","category":"business","price":149.99,"last_login":"2024-01-16T12:00:00Z","created_at":"2023-04-05T10:20:00Z"}
EOF

echo ""
echo "✅ Data seeded successfully!"
echo ""
echo "🧪 Now testing figo Elasticsearch adapter..."
echo ""

# Run the test
go run ./cmd/elasticsearch-test/main.go
