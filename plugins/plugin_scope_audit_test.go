package plugins

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopePlugin(t *testing.T) {
	scope := EqExpr{Field: "tenant_id", Value: int64(42)}

	t.Run("ScopeAppendedToFilteredQuery", func(t *testing.T) {
		f := New()
		require.NoError(t, f.RegisterPlugin(NewScopePlugin(scope)))
		require.NoError(t, f.AddFiltersFromString(`name="x"`))
		f.Build(RawAdapter{})

		where, args, _ := BuildRawWhere(f)
		assert.Contains(t, where, "`name` = ?")
		assert.Contains(t, where, "`tenant_id` = ?")
		assert.Contains(t, args, int64(42))
	})

	t.Run("UnfilteredQueryCannotEscapeScope", func(t *testing.T) {
		// The critical case: no DSL at all — the scope must still be there.
		f := New()
		require.NoError(t, f.RegisterPlugin(NewScopePlugin(scope)))
		f.Build(RawAdapter{})

		where, args, _ := BuildRawWhere(f)
		assert.Equal(t, "`tenant_id` = ?", where)
		assert.Equal(t, []any{int64(42)}, args)
	})

	t.Run("RebuildDoesNotDuplicateScope", func(t *testing.T) {
		f := New()
		require.NoError(t, f.RegisterPlugin(NewScopePlugin(scope)))
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		f.Build(RawAdapter{})
		f.Build(nil)
		f.Build(nil)

		_, args, _ := BuildRawWhere(f)
		assert.Len(t, args, 2, "rebuilds must not accumulate scope copies")

		// Empty-DSL rebuilds keep existing clauses; still no duplicates.
		f2 := New()
		require.NoError(t, f2.RegisterPlugin(NewScopePlugin(scope)))
		f2.Build(RawAdapter{})
		f2.Build(nil)
		assert.Len(t, f2.GetClauses(), 1)
	})

	t.Run("WhitelistCannotStripScope", func(t *testing.T) {
		f := New()
		fp := NewFieldsPlugin()
		fp.SetAllowedFields("name") // tenant_id deliberately NOT allowed
		fp.EnableFieldWhitelist()
		require.NoError(t, f.RegisterPlugin(fp))
		require.NoError(t, f.RegisterPlugin(NewScopePlugin(scope)))

		require.NoError(t, f.AddFiltersFromString(`name="x" and tenant_id=999`))
		f.Build(RawAdapter{})

		where, args, _ := BuildRawWhere(f)
		// The caller's own tenant_id=999 is pruned by the whitelist; the
		// server-side scope (injected after pruning) survives.
		assert.Contains(t, where, "`tenant_id` = ?")
		assert.Contains(t, args, int64(42))
		assert.NotContains(t, args, int64(999))
	})

	t.Run("ScopeCoversMongoToo", func(t *testing.T) {
		f := New()
		require.NoError(t, f.RegisterPlugin(NewScopePlugin(scope)))
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		f.Build(nil)

		m, err := BuildMongoFilter(f)
		require.NoError(t, err)
		assert.Contains(t, fmt.Sprint(m), "tenant_id")
	})
}

func TestAuditPlugin(t *testing.T) {
	t.Run("RecordsParseAndRender", func(t *testing.T) {
		ap := NewAuditPlugin(nil, 10)
		f := New()
		require.NoError(t, f.RegisterPlugin(ap))

		require.NoError(t, f.AddFiltersFromString(`id=1`))
		f.Build(RawAdapter{})
		sql := f.GetSqlString(RawContext{Table: "t"})
		q := f.GetQuery(RawContext{Table: "t"}).(SQLQuery)

		h := ap.History()
		// A "parse-attempt" entry (the DSL as received, recorded in
		// BeforeParse) now precedes every "parse" entry.
		require.Len(t, h, 4)
		assert.Equal(t, "parse-attempt", h[0].Kind)
		assert.Equal(t, `id=1`, h[0].DSL)
		assert.Equal(t, "parse", h[1].Kind)
		assert.Equal(t, `id=1`, h[1].DSL)
		assert.Equal(t, "query", h[2].Kind)
		assert.Equal(t, sql, h[2].Result, "GetSqlString render is captured verbatim")
		assert.Equal(t, "query", h[3].Kind)
		assert.Equal(t, q.SQL, h[3].Result, "GetQuery captures the parameterized SQL")
		assert.Equal(t, q.Args, h[3].Args, "the bound values are recorded, not just the shape")
	})

	t.Run("HistoryIsBounded", func(t *testing.T) {
		ap := NewAuditPlugin(nil, 2)
		f := New()
		require.NoError(t, f.RegisterPlugin(ap))

		require.NoError(t, f.AddFiltersFromString(`a=1`))
		require.NoError(t, f.AddFiltersFromString(`b=2`))
		require.NoError(t, f.AddFiltersFromString(`c=3`))

		h := ap.History()
		require.Len(t, h, 2, "history must stay at its bound")
		// Each parse records two entries (attempt + parse), so the bound of 2
		// leaves exactly the last call's pair.
		assert.Equal(t, "parse-attempt", h[0].Kind, "oldest entries evicted first")
		assert.Equal(t, `c=3`, h[0].DSL)
		assert.Equal(t, "parse", h[1].Kind)
		assert.Equal(t, `c=3`, h[1].DSL)

		ap.Clear()
		assert.Empty(t, ap.History())
	})

	t.Run("ZeroHistoryDisablesRecording", func(t *testing.T) {
		ap := NewAuditPlugin(nil, 0)
		f := New()
		require.NoError(t, f.RegisterPlugin(ap))
		require.NoError(t, f.AddFiltersFromString(`a=1`))
		assert.Empty(t, ap.History())
	})
}
