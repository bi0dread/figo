package figo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The adapter may be supplied to New(), to Build(), or deferred and set later;
// Build's adapter (if any) takes precedence. These forms must all keep working.
func TestAdapterCanBeSetOnNewOrBuild(t *testing.T) {
	t.Run("AdapterOnBuild", func(t *testing.T) {
		f := New()
		f.AddFiltersFromString(`id=1`)
		f.Build(RawAdapter{})
		_, ok := f.GetAdapterObject().(RawAdapter)
		assert.True(t, ok)
	})

	t.Run("AdapterOnNew", func(t *testing.T) {
		f := New(RawAdapter{})
		f.AddFiltersFromString(`id=1`)
		f.Build()
		_, ok := f.GetAdapterObject().(RawAdapter)
		assert.True(t, ok)
	})

	t.Run("BuildOverridesNew", func(t *testing.T) {
		f := New(MongoAdapter{})
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
