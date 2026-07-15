package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

func main() {
	fmt.Println("🚀 figo Elasticsearch Adapter Usage Examples")
	fmt.Println(strings.Repeat("=", 50))

	// Example 1: Basic term query
	fmt.Println("\n📝 Example 1: Basic Term Query")
	f1 := figo.New()
	f1.AddFiltersFromString(`status = "active"`)
	f1.Build(adapters.ElasticsearchAdapter{})

	jsonStr1, _ := adapters.GetElasticsearchQueryString(f1)
	fmt.Printf("DSL: status = \"active\"\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr1)

	// Example 2: Range query with multiple conditions
	fmt.Println("\n📝 Example 2: Range Query")
	f2 := figo.New()
	f2.AddFiltersFromString(`age > 25 and score >= 80`)
	f2.Build(adapters.ElasticsearchAdapter{})

	jsonStr2, _ := adapters.GetElasticsearchQueryString(f2)
	fmt.Printf("DSL: age > 25 and score >= 80\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr2)

	// Example 3: Wildcard search
	fmt.Println("\n📝 Example 3: Wildcard Search")
	f3 := figo.New()
	f3.AddFiltersFromString(`email =^ "%gmail%"`)
	f3.Build(adapters.ElasticsearchAdapter{})

	jsonStr3, _ := adapters.GetElasticsearchQueryString(f3)
	fmt.Printf("DSL: email =^ \"%%gmail%%\"\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr3)

	// Example 4: Terms query (IN operation)
	fmt.Println("\n📝 Example 4: Terms Query")
	f4 := figo.New()
	f4.AddFiltersFromString(`category <in> [tech,business,finance]`)
	f4.Build(adapters.ElasticsearchAdapter{})

	jsonStr4, _ := adapters.GetElasticsearchQueryString(f4)
	fmt.Printf("DSL: category <in> [tech,business,finance]\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr4)

	// Example 5: Between query
	fmt.Println("\n📝 Example 5: Between Query")
	f5 := figo.New()
	f5.AddFiltersFromString(`price <bet> (100..500)`)
	f5.Build(adapters.ElasticsearchAdapter{})

	jsonStr5, _ := adapters.GetElasticsearchQueryString(f5)
	fmt.Printf("DSL: price <bet> (100..500)\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr5)

	// Example 6: Complex nested query
	fmt.Println("\n📝 Example 6: Complex Nested Query")
	f6 := figo.New()
	f6.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
	f6.Build(adapters.ElasticsearchAdapter{})

	jsonStr6, _ := adapters.GetElasticsearchQueryString(f6)
	fmt.Printf("DSL: ((name =^ \"%%John%%\" or email =^ \"%%gmail%%\") and (age >= 18 and age <= 65)) or (status = \"active\" and score > 80)\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr6)

	// Example 7: Pagination and sorting
	fmt.Println("\n📝 Example 7: Pagination and Sorting")
	f7 := figo.New()
	f7.AddFiltersFromString(`status = "active" sort=score:desc,age:asc page=skip:0,take:5`)
	f7.Build(adapters.ElasticsearchAdapter{})

	jsonStr7, _ := adapters.GetElasticsearchQueryString(f7)
	fmt.Printf("DSL: status = \"active\" sort=score:desc,age:asc page=skip:0,take:5\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr7)

	// Example 8: Using the fluent builder
	fmt.Println("\n📝 Example 8: Fluent Builder")
	builder := adapters.NewElasticsearchQueryBuilder()
	query8 := builder.
		AddSort("score", false). // desc
		AddSort("age", true).    // asc
		SetPagination(0, 10).
		SetSource("id", "name", "email", "score").
		Build()

	jsonStr8, _ := json.MarshalIndent(query8, "", "  ")
	fmt.Printf("Fluent Builder Query:\n%s\n", jsonStr8)

	// Example 9: Field selection
	fmt.Println("\n📝 Example 9: Field Selection")
	f9 := figo.New()
	f9.AddSelectFields("id", "name", "email", "score")
	f9.AddFiltersFromString(`status = "active"`)
	f9.Build(adapters.ElasticsearchAdapter{})

	jsonStr9, _ := adapters.GetElasticsearchQueryString(f9)
	fmt.Printf("DSL: status = \"active\" with field selection\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr9)

	fmt.Println("\n✅ All examples completed!")
	fmt.Println("\n💡 Tips:")
	fmt.Println("  - Use the DSL syntax for simple queries")
	fmt.Println("  - Use the fluent builder for complex query construction")
	fmt.Println("  - All queries are type-safe and validated")
	fmt.Println("  - Supports all Elasticsearch query types")
	fmt.Println("  - Works with pagination, sorting, and field selection")
}
