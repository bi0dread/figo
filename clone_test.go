package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCloneProducesEqualQuery(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`id<in>[1,2,3] and (age>20 or active=true) sort=id:desc page=skip:5,take:10`)
	f.Build(RawAdapter{})

	c := f.Clone()

	// Same rendered output.
	assert.Equal(t, f.Explain(), c.Explain())
	fw, fa := BuildRawWhere(f)
	cw, ca := BuildRawWhere(c)
	assert.Equal(t, fw, cw)
	assert.Equal(t, fa, ca)
	assert.Equal(t, f.GetPage(), c.GetPage())
	assert.Equal(t, f.GetSort(), c.GetSort())
}

func TestCloneIsIndependent(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`id=1`)
	f.Build(RawAdapter{})
	f.AddSelectFields("id")

	c := f.Clone()

	// Mutate the clone in several ways.
	c.AddFilter(EqExpr{Field: "extra", Value: int64(9)})
	c.SetPage(100, 100)
	c.AddSelectFields("clone_only")

	// Original is untouched.
	assert.Len(t, f.GetClauses(), 1, "clone AddFilter must not grow the original")
	assert.Equal(t, Page{Skip: 0, Take: 20}, f.GetPage(), "clone SetPage must not affect the original")
	assert.NotContains(t, f.GetSelectFields(), "clone_only", "clone select-field must not leak to original")

	// Clone has its own changes.
	assert.Len(t, c.GetClauses(), 2)
	assert.Equal(t, Page{Skip: 100, Take: 100}, c.GetPage())
	assert.Contains(t, c.GetSelectFields(), "clone_only")
	// Field sets copied at clone time are present.
	assert.Contains(t, c.GetSelectFields(), "id")

	// Mutating the original after cloning must not affect the clone.
	f.AddFilter(EqExpr{Field: "late", Value: int64(1)})
	assert.Len(t, c.GetClauses(), 2, "original AddFilter must not grow the clone")
}

func TestCloneDeepCopiesNestedSlices(t *testing.T) {
	f := New()
	f.AddFilter(InExpr{Field: "id", Values: []any{1, 2, 3}})
	f.Build(RawAdapter{})

	c := f.Clone()

	// Mutate the original's underlying slice in place; clone must be unaffected.
	orig := f.GetClauses()[0].(InExpr)
	orig.Values[0] = 999

	cloned := c.GetClauses()[0].(InExpr)
	assert.Equal(t, []any{1, 2, 3}, cloned.Values, "clone must own an independent Values slice")
}

func TestCloneEmptyInstance(t *testing.T) {
	f := New()
	c := f.Clone()
	assert.Equal(t, "(no filters)", c.Explain())
	// Clone of an empty instance is still usable.
	c.AddFilter(EqExpr{Field: "x", Value: int64(1)})
	assert.Len(t, c.GetClauses(), 1)
	assert.Empty(t, f.GetClauses())
}
