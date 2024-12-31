# Go Gorm plugin Library (figo) v2

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
"(id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10"

```
* Supported Operations
* ">, <, >=, <=, =, !=
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
f.AddFiltersFromString("(id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10")

```
Manually 

```go
f.AddFilter(clause.Eq{
    Column: clause.Column{Name: "id"},
    Value:  9,
})

```

* Restricting Fields\
  Prevent certain fields from being queried:
```go
f.AddIgnoreFields("sensitive_field", "internal_use_only")

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
f.AddFiltersFromString("page=skip:0,take:10")
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
	f.AddFiltersFromString("(id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10")

	// Add banned fields
	f.AddIgnoreFields("restricted_field")

	// Add hide fields in results
        f.AddSelectFields("restricted_field")
  
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
field operation value (field operation value field operation value(field operation value))...
```
## DSL Syntax

The DSL syntax allows you to define query filters dynamically:

- **Field Filters**: `field operation value` (id=3)
- **Logical Operations**: Combine filters using `and`, `or`, and `not`.
- **Sorting**: `sort=id:desc`
- **Pagination**: `page=skip:0,take:10`
- **Preloading Relations**: `load=[TestInner1:id=3 or name=test1 | TestInner2:id=4]`

* Examples
```text

Basic Filters

    id=10
    (Where id = 10)
    
Logical Operators

    id=10 or status=active
    (Where id = 10 OR status = 'active')
    
Sorting

    sort=name:asc,created_at=desc
    (Order by name ASC, created_at DESC)
    
Pagination

    page=skip:10,take:5
    (Skip 10 records and take 5)
    
Complex Filters

    (id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10
```
## Supported Operations

| Operation | DSL Example                   | Description             |
|-----------|-------------------------------|-------------------------|
| `=`       | `field=value`                 | Equals                  |
| `>`       | `field>value`                 | Greater Than            |
| `>=`      | `field>=value`                | Greater Than or Equal   |
| `<`       | `field<value`                 | Less Than               |
| `<=`      | `field<=value`                | Less Than or Equal      |
| `!=`      | `field!=value`                | Not Equal               |
| `in`      | `not impl`    | Value in List           |
| `notIn`   | `not impl` | Value not in List   |
| `like`    | `not impl`          | Like (Partial Match)    |
| `notLike` | `not impl`       | Not Like                |
| `between` | `not impl`                    | Between Range           |
| `and`     | `and`                         | Logical AND             |
| `or`      | `or`                          | Logical OR              |
| `not`     | `not`                         | Logical NOT             |

# Extensibility
* You can extend the package to support custom operations or additional parsing logic. Modify the operatorParser method for parsing custom DSL extensions.

## Project Maturity

This project is based on original GORM package

## TODO
* improvement
* more operators

## Contributing

Pull requests are very much welcomed.  Create your pull request on a non-main
branch, make sure a test or example is included that covers your change, and
your commits represent coherent changes that include a reason for the change.

## License

BSD 2 clause, see LICENSE for more details.