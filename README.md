# Go Gorm plugin Library (figo)

The figo package provides a robust mechanism for building dynamic filters for SQL queries in applications that use the GORM ORM library. It simplifies the process of defining filters through a domain-specific language (DSL) and converting them into GORM clauses, offering a powerful tool for creating flexible and complex queries.
## Differences from gorm package

just makes gorm clauses from string

### Package Name

figo


### Installation
``` bash
go get github.com/bi0dread/figo
```

# Features
* DSL-Based Filter Parsing \
Easily construct complex filters using a concise DSL format like:
```go
"id:[eq:9,or,eq:10]|or|vendorId:[eq:22]|and|bank_id:[gt:11]|or|expedition_type:[eq:eq]"

```
* Supported Operations
* eq, gt, gte, lt, lte, ne, like, notLike, between, in, notIn
* Logical operations: and, or, not
* Additional features: sort, load, page

### GORM Integration
The figo package converts filters into GORM-compatible clause.Expression objects, which can be directly applied to database queries.

### Pagination Support
Manage result limits and offsets with the page operation.

### Field Restrictions
Prevent specific fields from being included in queries.

# Usage
* Creating a Figo Instance 
```go
f := figo.New()
```
* Adding Filters
```go
f.AddFiltersFromString("id:[eq:9,or,eq:10]|or|vendorId:[eq:22]|and|bank_id:[gt:11]|or|expedition_type:[eq:eq]")

```
Manually 

```go
f.AddFilter(figo.OperationEq, clause.Eq{
    Column: clause.Column{Name: "id"},
    Value:  9,
})

```

* Restricting Fields\
  Prevent certain fields from being queried:
```go
f.AddBanFields("sensitive_field", "internal_use_only")

```

* Building Filters\
  After adding filters, invoke Build to process the clauses:
```go
f.Build()
```
* Applying Filters to a GORM Query\
  Use the Apply method to integrate filters into your GORM query:
```go
db := f.Apply(db)
```
* Pagination\
  Control the result set's skip and take values via the DSL:
```go
f.AddFiltersFromString("page:[skip=10&take=20]")
```
 Or programmatically:
```go
f.GetPage().Skip = 10
f.GetPage().Take = 20
```
* Retrieving Preloads\
  If you’ve specified relationships to preload, retrieve them as follows:
```go
preloads := f.GetPreloads()
```

# Example: Full Workflow
```go
package main

import (
	"fmt"
	"github.com/bi0dread/figo"
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
	f := figo.New()

	// Add filters from DSL
	f.AddFiltersFromString("id:[eq:9,or,eq:10]|and|status:[eq:active]|page:[skip=0&take=5]")

	// Add banned fields
	f.AddBanFields("restricted_field")

	// Build the filters
	f.Build()

	// Apply to GORM query
	db = f.Apply(db)

	// Execute query
	var results []map[string]any
	db.Find(&results)

	fmt.Println("Query Results:", results)
}
```
# Domain-Specific Language (DSL) Syntax
* Syntax Format
```text
field:[operation:value,operation:value]|operator|field:[operation:value]|...
```
## DSL Syntax

The DSL syntax allows you to define query filters dynamically:

- **Field Filters**: `field:[operation:value]`
- **Logical Operations**: Combine filters using `and`, `or`, and `not`.
- **Sorting**: `sort:[field=desc&field2=asc]`
- **Pagination**: `page:[skip=0&take=10]`
- **Preloading Relations**: `load:[relation1&relation2]`

* Examples
```text

Basic Filters

    id:[eq:10]
    (Where id = 10)
    
Logical Operators

    id:[eq:10]|or|status:[eq:active]
    (Where id = 10 OR status = 'active')
    
Sorting

    sort:[name=asc,created_at=desc]
    (Order by name ASC, created_at DESC)
    
Pagination

    page:[skip=10&take=5]
    (Skip 10 records and take 5)
    
Complex Filters

    id:[eq:9,or,eq:10]|or|status:[eq:active]|and|price:[gte:100]|or|category:[in:electronics&home_appliances]
```
## Supported Operations

| Operation     | DSL Example                | Description             |
|---------------|----------------------------|-------------------------|
| `eq`          | `field:[eq:value]`         | Equals                  |
| `gt`          | `field:[gt:value]`         | Greater Than            |
| `gte`         | `field:[gte:value]`        | Greater Than or Equal   |
| `lt`          | `field:[lt:value]`         | Less Than               |
| `lte`         | `field:[lte:value]`        | Less Than or Equal      |
| `neq`         | `field:[neq:value]`        | Not Equal               |
| `in`          | `field:[in:value1&value2]` | Value in List           |
| `notIn`       | `field:[notIn:value1&value2]` | Value not in List   |
| `like`        | `field:[like:value]`       | Like (Partial Match)    |
| `notLike`     | `field:[notLike:value]`    | Not Like                |
| `between`     | `field:[between:val1&val2]`| Between Range           |
| `and`         | `and`                      | Logical AND             |
| `or`          | `or`                       | Logical OR              |
| `not`         | `not`                      | Logical NOT             |

# Extensibility
* You can extend the package to support custom operations or additional parsing logic. Modify the operatorParser method for parsing custom DSL extensions.

## Project Maturity

This project is based on original GORM package

## TODO
* improvement

## Contributing

Pull requests are very much welcomed.  Create your pull request on a non-main
branch, make sure a test or example is included that covers your change, and
your commits represent coherent changes that include a reason for the change.

## License

BSD 2 clause, see LICENSE for more details.