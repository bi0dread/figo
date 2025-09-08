package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bi0dread/figo/v3"
)

func main() {
	fmt.Println("🚀 figo Elasticsearch Adapter Usage Examples")
	fmt.Println(strings.Repeat("=", 50))

	// Example 1: Basic term query
	fmt.Println("\n📝 Example 1: Basic Term Query")
	f1 := figo.New(figo.ElasticsearchAdapter{})
	f1.AddFiltersFromString(`status = "active"`)
	f1.Build()

	jsonStr1, _ := figo.GetElasticsearchQueryString(f1)
	fmt.Printf("DSL: status = \"active\"\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr1)

	// Example 2: Range query with multiple conditions
	fmt.Println("\n📝 Example 2: Range Query")
	f2 := figo.New(figo.ElasticsearchAdapter{})
	f2.AddFiltersFromString(`age > 25 and score >= 80`)
	f2.Build()

	jsonStr2, _ := figo.GetElasticsearchQueryString(f2)
	fmt.Printf("DSL: age > 25 and score >= 80\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr2)

	// Example 3: Wildcard search
	fmt.Println("\n📝 Example 3: Wildcard Search")
	f3 := figo.New(figo.ElasticsearchAdapter{})
	f3.AddFiltersFromString(`email =^ "%gmail%"`)
	f3.Build()

	jsonStr3, _ := figo.GetElasticsearchQueryString(f3)
	fmt.Printf("DSL: email =^ \"%%gmail%%\"\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr3)

	// Example 4: Terms query (IN operation)
	fmt.Println("\n📝 Example 4: Terms Query")
	f4 := figo.New(figo.ElasticsearchAdapter{})
	f4.AddFiltersFromString(`category <in> [tech,business,finance]`)
	f4.Build()

	jsonStr4, _ := figo.GetElasticsearchQueryString(f4)
	fmt.Printf("DSL: category <in> [tech,business,finance]\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr4)

	// Example 5: Between query
	fmt.Println("\n📝 Example 5: Between Query")
	f5 := figo.New(figo.ElasticsearchAdapter{})
	f5.AddFiltersFromString(`price <bet> (100..500)`)
	f5.Build()

	jsonStr5, _ := figo.GetElasticsearchQueryString(f5)
	fmt.Printf("DSL: price <bet> (100..500)\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr5)

	// Example 6: Complex nested query
	fmt.Println("\n📝 Example 6: Complex Nested Query")
	f6 := figo.New(figo.ElasticsearchAdapter{})
	f6.AddFiltersFromString(`((name =^ "%John%" or email =^ "%gmail%") and (age >= 18 and age <= 65)) or (status = "active" and score > 80)`)
	f6.Build()

	jsonStr6, _ := figo.GetElasticsearchQueryString(f6)
	fmt.Printf("DSL: ((name =^ \"%%John%%\" or email =^ \"%%gmail%%\") and (age >= 18 and age <= 65)) or (status = \"active\" and score > 80)\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr6)

	// Example 7: Pagination and sorting
	fmt.Println("\n📝 Example 7: Pagination and Sorting")
	f7 := figo.New(figo.ElasticsearchAdapter{})
	f7.AddFiltersFromString(`status = "active" sort=score:desc,age:asc page=skip:0,take:5`)
	f7.Build()

	jsonStr7, _ := figo.GetElasticsearchQueryString(f7)
	fmt.Printf("DSL: status = \"active\" sort=score:desc,age:asc page=skip:0,take:5\n")
	fmt.Printf("Generated Query:\n%s\n", jsonStr7)

	// Example 8: Using the fluent builder
	fmt.Println("\n📝 Example 8: Fluent Builder")
	builder := figo.NewElasticsearchQueryBuilder()
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
	f9 := figo.New(figo.ElasticsearchAdapter{})
	f9.AddSelectFields("id", "name", "email", "score")
	f9.AddFiltersFromString(`status = "active"`)
	f9.Build()

	jsonStr9, _ := figo.GetElasticsearchQueryString(f9)
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
