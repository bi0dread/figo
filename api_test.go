package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"testing"

	"github.com/stretchr/testify/assert"
)

// The adapter is supplied at Build (or via SetAdapterObject) — New() takes no
// adapter. These forms must all keep working.
func TestAdapterCanBeSetOnBuild(t *testing.T) {
	t.Run("AdapterOnBuild", func(t *testing.T) {
		f := New()
		f.AddFiltersFromString(`id=1`)
		f.Build(RawAdapter{})
		_, ok := f.GetAdapterObject().(RawAdapter)
		assert.True(t, ok)
	})

	t.Run("AdapterViaSetter", func(t *testing.T) {
		f := New()
		f.SetAdapterObject(RawAdapter{})
		f.AddFiltersFromString(`id=1`)
		f.Build(nil)
		_, ok := f.GetAdapterObject().(RawAdapter)
		assert.True(t, ok)
	})

	t.Run("BuildAdapterOverridesSetter", func(t *testing.T) {
		f := New()
		f.SetAdapterObject(MongoAdapter{})
		f.AddFiltersFromString(`id=1`)
		f.Build(RawAdapter{})
		_, ok := f.GetAdapterObject().(RawAdapter)
		assert.True(t, ok, "the adapter passed to Build should win")
	})

	t.Run("NoAdapter", func(t *testing.T) {
		f := New()
		f.AddFiltersFromString(`id=1`)
		f.Build(nil)
		assert.Nil(t, f.GetAdapterObject())
		// Still parses clauses fine without an adapter.
		assert.Len(t, f.GetClauses(), 1)
	})
}

// GetSort must return a copy (as GetClauses/GetPreloads do) — the internal
// pointer would let a caller mutate the sort columns while adapters render on
// other goroutines.
func TestGetSortReturnsIndependentCopy(t *testing.T) {
	f := New()
	f.AddFiltersFromString(`id=1 sort=id:desc`)
	f.Build(RawAdapter{})

	s := f.GetSort()
	assert.NotNil(t, s)
	s.Columns[0].Name = "mutated"
	s.Columns[0].Desc = false

	fresh := f.GetSort()
	assert.Equal(t, "id", fresh.Columns[0].Name)
	assert.True(t, fresh.Columns[0].Desc)
}
