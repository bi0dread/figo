# Go Gorm plugin Library (figo) v3

The figo package provides a robust mechanism for building dynamic filters for SQL queries in applications that use the GORM ORM library. It simplifies the process of defining filters through a domain-specific language (DSL) and converting them into GORM clauses, offering a powerful tool for creating flexible and complex queries.

## Differences from gorm package

Just makes gorm clauses from string - no more complex query building!

### Package Name

figo

### Installation
```bash
go get github.com/bi0dread/figo/v3
```

## Features

* **DSL-Based Filter Parsing** - Easily construct complex filters using a concise DSL format
* **Rich Operations** - Support for all common database operations
* **Multiple Adapters** - GORM, Raw SQL, MongoDB, and Elasticsearch support
* **Type-Safe Parsing** - Automatic type detection for numbers, booleans, and strings
* **Complex Expressions** - Nested parentheses and logical operators
* **Production Ready** - Comprehensive test coverage and bug-free implementation

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/bi0dread/figo/v3"
    "gorm.io/driver/sqlite"
    "gorm.io/gorm"
)

func main() {
    // Initialize GORM
    db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{})
    if err != nil {
        panic("failed to connect database")
    }

    // Create a Figo instance
    f := figo.New(figo.GormAdapter{})

    // Add filters from DSL
    f.AddFiltersFromString(`(id=1 and vendorId=22) and bank_id>11 or expedition_type="eq" load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10`)

    // Build and apply filters
    f.Build()
    db = figo.ApplyGorm(f, db)

    // Execute query
    var results []map[string]any
    db.Find(&results)
    fmt.Println("Query Results:", results)
}
```

## Supported Operations

The figo package supports a comprehensive set of database operations across all adapters (GORM, Raw SQL, MongoDB, Elasticsearch). All operations are fully tested and production-ready.

### Basic Comparison Operators

| Operation | DSL Example | SQL Result | MongoDB Result | Elasticsearch Result | Description |
|-----------|-------------|------------|----------------|---------------------|-------------|
| `=` | `id=10` | `WHERE id = 10` | `{"id": 10}` | `{"term": {"id": 10}}` | Equals |
| `>` | `age>18` | `WHERE age > 18` | `{"age": {"$gt": 18}}` | `{"range": {"age": {"gt": 18}}}` | Greater Than |
| `>=` | `score>=80` | `WHERE score >= 80` | `{"score": {"$gte": 80}}` | `{"range": {"score": {"gte": 80}}}` | Greater Than or Equal |
| `<` | `price<100` | `WHERE price < 100` | `{"price": {"$lt": 100}}` | `{"range": {"price": {"lt": 100}}}` | Less Than |
| `<=` | `count<=5` | `WHERE count <= 5` | `{"count": {"$lte": 5}}` | `{"range": {"count": {"lte": 5}}}` | Less Than or Equal |
| `!=` | `status!="deleted"` | `WHERE status != 'deleted'` | `{"status": {"$ne": "deleted"}}` | `{"bool": {"must_not": {"term": {"status": "deleted"}}}}` | Not Equal |

### String Pattern Matching

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `=^` | `name=^"%john%"` | `WHERE name LIKE '%john%'` | `{"name": {"$regex": "john", "$options": "i"}}` | LIKE (Case-insensitive) |
| `!=^` | `name!=^"%admin%"` | `WHERE name NOT LIKE '%admin%'` | `{"name": {"$not": {"$regex": "admin", "$options": "i"}}}` | NOT LIKE |
| `=~` | `email=~"^[a-z]+@gmail\.com$"` | `WHERE email REGEXP '^[a-z]+@gmail\.com$'` | `{"email": {"$regex": "^[a-z]+@gmail\\.com$"}}` | Regex Match |
| `!=~` | `phone!=~"^\+1"` | `WHERE phone NOT REGEXP '^\+1'` | `{"phone": {"$not": {"$regex": "^\\+1"}}}` | Regex Not Match |

### Set Operations

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `<in>` | `id<in>[1,2,3,4,5]` | `WHERE id IN (1,2,3,4,5)` | `{"id": {"$in": [1,2,3,4,5]}}` | Value in List |
| `<nin>` | `status<nin>["deleted","archived"]` | `WHERE status NOT IN ('deleted','archived')` | `{"status": {"$nin": ["deleted","archived"]}}` | Value not in List |

### Range Operations

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `<bet>` | `price<bet>(10..100)` | `WHERE price BETWEEN 10 AND 100` | `{"price": {"$gte": 10, "$lte": 100}}` | Between Range (inclusive) |

### Null Operations

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `<null>` | `deleted_at<null>` | `WHERE deleted_at IS NULL` | `{"deleted_at": null}` | Is Null |
| `<notnull>` | `updated_at<notnull>` | `WHERE updated_at IS NOT NULL` | `{"updated_at": {"$ne": null}}` | Is Not Null |

### Logical Operators

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `and` | `id=1 and status="active"` | `WHERE id = 1 AND status = 'active'` | `{"$and": [{"id": 1}, {"status": "active"}]}` | Logical AND |
| `or` | `name="john" or name="jane"` | `WHERE name = 'john' OR name = 'jane'` | `{"$or": [{"name": "john"}, {"name": "jane"}]}` | Logical OR |
| `not` | `not (deleted=true)` | `WHERE NOT (deleted = true)` | `{"$nor": [{"deleted": true}]}` | Logical NOT |

### Special Operations

| Operation | DSL Example | SQL Result | MongoDB Result | Description |
|-----------|-------------|------------|----------------|-------------|
| `sort=` | `sort=name:asc,age:desc` | `ORDER BY name ASC, age DESC` | `{"name": 1, "age": -1}` | Sorting |
| `page=` | `page=skip:10,take:5` | `LIMIT 5 OFFSET 10` | `{"limit": 5, "skip": 10}` | Pagination |
| `load=` | `load=[User:name="john" \| Profile:bio=^"%dev%"]` | `JOIN users ON ... JOIN profiles ON ...` | `{"$lookup": {...}}` | Preloading/Joins |

### Data Type Support

| Type | DSL Example | Parsed Value | Description |
|------|-------------|--------------|-------------|
| **Integer** | `id=123` | `int64(123)` | Unquoted numbers |
| **Float** | `price=99.99` | `float64(99.99)` | Decimal numbers |
| **Boolean** | `active=true` | `bool(true)` | Unquoted true/false |
| **String (Quoted)** | `name="john"` | `string("john")` | Quoted strings |
| **String (Unquoted)** | `status=active` | `string("active")` | Unquoted strings |
| **Null** | `deleted_at<null>` | `nil` | Null values |
| **Array** | `id<in>[1,2,3]` | `[]any{1,2,3}` | Comma-separated lists |

### Regex Configuration

The regex operators (`=~`, `!=~`) can be configured for different SQL dialects:

```go
// MySQL (default)
f.SetRegexSQLOperator("REGEXP")

// PostgreSQL
f.SetRegexSQLOperator("~")      // Case-sensitive
f.SetRegexSQLOperator("~*")     // Case-insensitive

// SQLite
f.SetRegexSQLOperator("REGEXP")
```

## Complex Filter Examples

### Nested Parentheses

```go
// Complex nested expression
f.AddFiltersFromString(`((name > "a" and age < 30) or (status = "active" and score > 80)) and (deleted_at <null> or updated_at > "2023-01-01")`)
// SQL: WHERE (((name > 'a' AND age < 30) OR (status = 'active' AND score > 80)) AND (deleted_at IS NULL OR updated_at > '2023-01-01'))
```

### Mixed Data Types

```go
// Numbers, strings, booleans, and dates
f.AddFiltersFromString(`id > 100 and name = "test" and price < 99.99 and active = true and created_at > "2023-01-01"`)
// SQL: WHERE id > 100 AND name = 'test' AND price < 99.99 AND active = true AND created_at > '2023-01-01'
```

### Field Names with Underscores

```go
// Complex field names
f.AddFiltersFromString(`user_id > 1 and user_name = "john" and user_email_address =^ "%@gmail.com"`)
// SQL: WHERE user_id > 1 AND user_name = 'john' AND user_email_address LIKE '%@gmail.com'
```

### Special Characters and Unicode

```go
// Unicode and special characters
f.AddFiltersFromString(`name = "O'Connor" and description =^ "%test%"`)
// SQL: WHERE name = 'O''Connor' AND description LIKE '%test%'
```

### Numeric Edge Cases

```go
// Zero values and negative numbers
f.AddFiltersFromString(`id = 0 and price = 0.0 and discount = -10.5`)
// SQL: WHERE id = 0 AND price = 0.0 AND discount = -10.5
```

### Complex Operators with Spaces

```go
// All complex operators in one expression
f.AddFiltersFromString(`name =^ "%test%" and id <in> [1,2,3,4,5] and status <nin> ["inactive","deleted"] and price <bet> (10..100)`)
// SQL: WHERE name LIKE '%test%' AND id IN (1,2,3,4,5) AND status NOT IN ('inactive','deleted') AND price BETWEEN 10 AND 100
```

### Null and Not Null Operations

```go
// Null checks
f.AddFiltersFromString(`deleted_at <null> and updated_at <notnull>`)
// SQL: WHERE deleted_at IS NULL AND updated_at IS NOT NULL
```

### Regex Operations

```go
// Regex patterns
f.AddFiltersFromString(`email =~ "^[a-z]+@gmail\.com$" and phone !=~ "^\+1"`)
// SQL: WHERE email REGEXP '^[a-z]+@gmail\.com$' AND phone NOT REGEXP '^\+1'
```

## Multiple Adapters

### GORM Adapter
```go
f := figo.New(figo.GormAdapter{})
f.AddFiltersFromString(`id=1 and name="test"`)
f.Build()
db = figo.ApplyGorm(f, db)
```

### Raw SQL Adapter
```go
f := figo.New(figo.RawAdapter{})
f.AddFiltersFromString(`id=1 and name="test"`)
f.Build()
sql, args := figo.BuildRawSelect(f, "users")
// sql: "SELECT * FROM `users` WHERE `id` = ? AND `name` = ? LIMIT 20"
// args: [1, "test"]
```

### MongoDB Adapter
```go
f := figo.New(figo.MongoAdapter{})
f.AddFiltersFromString(`id=1 and name="test"`)
f.Build()
query := f.GetQuery(nil)
// Returns MongoFindQuery or MongoAggregateQuery
```

### Elasticsearch Adapter
```go
f := figo.New(figo.ElasticsearchAdapter{})
f.AddFiltersFromString(`name = "john" and age > 25`)
f.Build()
query := figo.BuildElasticsearchQuery(f)
// Returns ElasticsearchQuery with JSON structure

// Get as JSON string
jsonStr, err := figo.GetElasticsearchQueryString(f)
// Returns: {"query":{"bool":{"must":[{"term":{"name":"john"}},{"range":{"age":{"gt":25}}}]}}}

// Using fluent builder
builder := figo.NewElasticsearchQueryBuilder()
query := builder.FromFigo(f).AddSort("name", true).SetPagination(0, 10).Build()
```

## Advanced Features

### Pagination
```go
// DSL pagination
f.AddFiltersFromString(`id>0 page=skip:10,take:5`)
// Or programmatically
f.GetPage().Skip = 10
f.GetPage().Take = 5
```

### Sorting
```go
// Multiple field sorting
f.AddFiltersFromString(`id>0 sort=name:asc,created_at:desc`)
```

### Field Selection
```go
// Select specific fields
f.AddSelectFields("id", "name", "email")
```

### Field Restrictions
```go
// Prevent certain fields from being queried
f.AddIgnoreFields("password", "secret_key")
```

### Preloading Relations
```go
// Complex preloading with filters
f.AddFiltersFromString(`id>0 load=[User:name="john" and age>18 | Profile:bio=^"%developer%" | Posts:title=^"%golang%" and published=true]`)
```

### Regex Configuration
```go
// Configure regex operator for different SQL dialects
f.SetRegexSQLOperator("REGEXP")  // MySQL
f.SetRegexSQLOperator("~")       // PostgreSQL
f.SetRegexSQLOperator("~*")      // PostgreSQL (case-insensitive)
```

## Type Parsing

The package automatically detects and parses different data types:

```go
// Numbers (unquoted)
f.AddFiltersFromString(`id=123`)           // int64(123)
f.AddFiltersFromString(`price=99.99`)      // float64(99.99)

// Booleans (unquoted)
f.AddFiltersFromString(`active=true`)      // bool(true)
f.AddFiltersFromString(`deleted=false`)    // bool(false)

// Strings (quoted)
f.AddFiltersFromString(`name="john"`)      // string("john")

// Strings (unquoted - treated as strings)
f.AddFiltersFromString(`status=active`)    // string("active")
```

## Error Handling

```go
f := figo.New(figo.GormAdapter{})
err := f.AddFiltersFromString(`invalid syntax`)
if err != nil {
    // Handle parsing errors
    log.Printf("Filter parsing error: %v", err)
}
```

## Performance Considerations

- The package is optimized for performance with minimal memory allocations
- Token combination logic handles complex expressions efficiently
- All operations are tested for edge cases and error conditions

## Production Ready

✅ **All 20 tests passing**  
✅ **No panics or crashes**  
✅ **Comprehensive error handling**  
✅ **Type-safe parsing**  
✅ **Full operator coverage**  
✅ **Complex expression support**  

## Contributing

Pull requests are welcome! Please:
1. Create your pull request on a non-main branch
2. Include tests that cover your changes
3. Ensure all existing tests pass
4. Update documentation as needed

## License

BSD 2 clause, see LICENSE for more details.