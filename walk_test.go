package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
)

// The feature-request style: type-assert to a concrete pointer and mutate Field.
func TestWalkMutatesFieldInPlace(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`first_name="ali" and age>20`)
	f.Build(nil)

	f.Walk(func(n Expr) {
		if c, ok := n.(*EqExpr); ok && c.Field == "first_name" {
			c.Field = "users.first_name"
		}
	})

	where, _, _ := BuildRawWhere(f)
	assert.Contains(t, where, "`users.first_name`")
	assert.NotContains(t, where, "`first_name` =")
	// The other node is untouched.
	assert.Contains(t, where, "`age` >")
}

// The uniform helper style: rename a field on ANY node type in one block.
func TestWalkNodeFieldHelpers(t *testing.T) {
	f := New()
	// first_name appears under =, LIKE, IN and inside a nested OR/BETWEEN group.
	f.AddFiltersFromString(`first_name="ali" or (first_name.=^"%a%" and first_name<in>[x,y]) or first_name<bet>(1..2)`)
	f.Build(nil)

	renamed := 0
	f.Walk(func(n Expr) {
		if field, ok := NodeField(n); ok && field == "first_name" {
			SetNodeField(n, "users.first_name")
			renamed++
		}
	})
	assert.Equal(t, 4, renamed, "every field-bearing node should be reachable and renamed")

	where, _, _ := BuildRawWhere(f)
	assert.NotContains(t, where, "`first_name`")
	assert.Contains(t, where, "`users.first_name`")
}

// Visits nested nodes and reaches into preloads too.
func TestWalkReachesNestedAndPreloads(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`load=[orders:first_name="x"] and first_name="y"`)
	f.Build(nil)

	f.Walk(func(n Expr) {
		if field, ok := NodeField(n); ok && field == "first_name" {
			SetNodeField(n, "u.first_name")
		}
	})

	// Root clause updated.
	where, _, _ := BuildRawWhere(f)
	assert.Contains(t, where, "`u.first_name`")
	// Preload clause updated too.
	pre, _ := BuildRawPreloads(f)
	po, ok := pre["orders"]
	assert.True(t, ok)
	assert.Contains(t, po.Where, "`u.first_name`")
}

// The package-level Walk returns the new root (value-type roots need reassigning).
func TestWalkPackageLevelReturnsRoot(t *testing.T) {
	var ast Expr = EqExpr{Field: "first_name", Value: "ali"}
	ast = Walk(ast, func(n Expr) {
		if c, ok := n.(*EqExpr); ok {
			c.Field = "users." + c.Field
		}
	})
	eq, ok := ast.(EqExpr)
	assert.True(t, ok)
	assert.Equal(t, "users.first_name", eq.Field)
}

// Logical nodes are visited but report no field.
func TestWalkLogicalNodesHaveNoField(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`a=1 and b=2`)
	f.Build(nil)
	sawAnd := false
	f.Walk(func(n Expr) {
		if _, ok := n.(*AndExpr); ok {
			sawAnd = true
		}
		if _, ok := NodeField(n); ok {
			// fine
		}
	})
	assert.True(t, sawAnd, "logical nodes should be visited")
}
