package figo_test

import (
	"testing"

	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A load= segment's content is parsed as a full DSL expression, and the
// recursive parse used to share the outer instance's directive state: a
// sort= written inside load=[...] became the MAIN query's ORDER BY, a page=
// the main LIMIT/OFFSET, and a nested load= a top-level preload. The content
// is now parsed on a scratch instance — directives inside a preload filter
// are dropped with a BuildE diagnostic and never touch the outer query.

func TestPreloadSortDoesNotLeakToMainQuery(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=x load=[Phone:sort=id:desc and id=1]`))
	err := f.BuildE(RawAdapter{})

	assert.Nil(t, f.GetSort(), "preload sort= must not set the main query's sort")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sort= inside load=")

	// The preload's actual filter survives.
	preloads := f.GetPreloads()
	require.Len(t, preloads["Phone"], 1)
	assert.Equal(t, EqExpr{Field: "id", Value: int64(1)}, preloads["Phone"][0])
}

func TestPreloadPageDoesNotLeakToMainQuery(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=x load=[Phone:page=skip:90,take:5 and id=1]`))
	err := f.BuildE(RawAdapter{})

	assert.Equal(t, Page{Skip: 0, Take: 20}, f.GetPage(), "preload page= must not change the main query's paging")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "page= inside load=")
}

func TestNestedLoadDoesNotFlattenToMainQuery(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=x load=[A:load=[B:z=1] and y=2]`))
	err := f.BuildE(RawAdapter{})

	preloads := f.GetPreloads()
	assert.NotContains(t, preloads, "B", "a nested load= must not become a top-level preload")
	require.Len(t, preloads["A"], 1)
	assert.Equal(t, EqExpr{Field: "y", Value: int64(2)}, preloads["A"][0])

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested load= inside load=")
}

// A directive-free preload filter keeps parsing without diagnostics.
func TestPreloadWithoutDirectivesStillClean(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id>0 load=[Orders:total>100]`))
	require.NoError(t, f.BuildE(RawAdapter{}))

	preloads := f.GetPreloads()
	require.Len(t, preloads["Orders"], 1)
	assert.Equal(t, GtExpr{Field: "total", Value: int64(100)}, preloads["Orders"][0])
}
