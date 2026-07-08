package figo

import (
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
		f.Build()
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
		f.Build()
		assert.Nil(t, f.GetAdapterObject())
		// Still parses clauses fine without an adapter.
		assert.Len(t, f.GetClauses(), 1)
	})
}
