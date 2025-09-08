#!/bin/bash

echo "🚀 Starting Elasticsearch with figo test environment..."

# Create elasticsearch directory if it doesn't exist
mkdir -p elasticsearch

# Start the services
echo "📦 Starting Docker Compose services..."
docker-compose up -d

echo "⏳ Waiting for Elasticsearch to be ready..."
sleep 30

# Check if Elasticsearch is ready
echo "🔍 Checking Elasticsearch health..."
curl -f http://localhost:9200/_cluster/health || {
    echo "❌ Elasticsearch is not ready yet. Please wait a bit more and try again."
    echo "You can check the logs with: docker-compose logs elasticsearch"
    exit 1
}

echo "✅ Elasticsearch is ready!"
echo ""
echo "🌐 Services available:"
echo "  - Elasticsearch: http://localhost:9200"
echo "  - Kibana: http://localhost:5601"
echo ""
echo "📊 To view the data in Kibana:"
echo "  1. Open http://localhost:5601"
echo "  2. Go to 'Stack Management' > 'Index Patterns'"
echo "  3. Create index patterns for: users, products, orders"
echo ""
echo "🧪 To test the figo Elasticsearch adapter:"
echo "  go run ./cmd/elasticsearch-test/main.go"
echo ""
echo "📝 To view logs:"
echo "  docker-compose logs -f"
echo ""
echo "🛑 To stop everything:"
echo "  docker-compose down"
