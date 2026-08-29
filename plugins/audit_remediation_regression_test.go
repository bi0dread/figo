package plugins

import (
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression pins for the plugin half of the security audit.

// F6: the latch rotated on ENTRY COUNT only, and an entry is a copy of a
// violation list whose Reasons quote the caller's own tokens. 8192 entries of
// arbitrary size bounded nothing: 2000 refused 30 KB requests retained ~1 GB
// while the count sat at a quarter of its budget — and this is the traffic the
// plugin exists to absorb.
//
// TestLatchTableIsBounded uses the 4-byte DSL "1=1", so per-entry size was
// unpinned.
func TestF6_LatchIsBoundedInBytesNotJustEntries(t *testing.T) {
	junk := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		junk = append(junk, "tok"+strings.Repeat("x", 8)+string(rune('a'+i%26)))
	}
	dsl := strings.Join(junk, " ")

	g := NewInjectionGuardPlugin()
	for i := 0; i < 200; i++ {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(g))
		_ = f.AddFiltersFromString(dsl)
		f.Build(adapters.RawAdapter{})
	}

	g.lmu.Lock()
	cur, prev := g.curBytes, g.prevBytes
	g.lmu.Unlock()
	assert.LessOrEqual(t, cur, maxLatchBytes,
		"the live generation must respect the byte budget")
	assert.LessOrEqual(t, cur+prev, 2*maxLatchBytes,
		"at most two generations are live, so this is the retained ceiling")
}

// F6, the per-entry half: a violation Reason quotes the caller's token, and the
// count of violations scales with the number of junk tokens. Both are capped.
func TestF6_ViolationsAreCappedPerRequest(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("tok")
		b.WriteString(strings.Repeat("y", 30))
		b.WriteByte(' ')
	}
	g := NewInjectionGuardPlugin()
	f := figo.New()
	require.NoError(t, f.RegisterPlugin(g))
	_ = f.AddFiltersFromString(b.String())
	f.Build(adapters.RawAdapter{})

	vs, rejected := g.Rejected(f)
	require.True(t, rejected, "junk tokens must still be a rejection")
	assert.LessOrEqual(t, len(vs), maxViolationsPerRequest+1,
		"the report is capped (+1 for the suppression notice)")
	for _, v := range vs {
		assert.LessOrEqual(t, len(v.Reason), maxViolationReasonBytes+len("… (truncated)"),
			"each Reason is capped")
	}
}

// F11: the audit hooks run inside AddFiltersFromString, which applies no length
// check — the parser's 1 MiB cap lives in BuildE and fires later — so the
// plugin retained precisely the input the parser was about to refuse, and
// trimmed by entry count only.
func TestF11_AuditHistoryIsBoundedInBytes(t *testing.T) {
	big := strings.Repeat("a", 2<<20) // 2 MiB, over the parser's own cap
	p := NewAuditPlugin(nil, 100)
	for i := 0; i < 40; i++ {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(p))
		_ = f.AddFiltersFromString(big)
		_ = f.BuildE(adapters.RawAdapter{})
	}

	p.mu.RLock()
	bytes := p.bytes
	p.mu.RUnlock()
	assert.LessOrEqual(t, bytes, maxAuditHistoryBytes,
		"history must respect the byte budget, not only the entry count")

	for _, e := range p.History() {
		assert.LessOrEqual(t, len(e.DSL), maxAuditEntryBytes+64,
			"a recorded DSL is truncated to a prefix")
	}
}

// F11, the eviction half: the trim resliced without clearing evicted elements,
// so they stayed reachable from the backing array well past the documented
// window.
func TestF11_EvictedAuditEntriesAreCleared(t *testing.T) {
	p := NewAuditPlugin(nil, 4)
	for i := 0; i < 40; i++ {
		p.record(AuditEntry{Kind: "parse", DSL: strings.Repeat("z", 1000)})
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	require.Len(t, p.history, 4, "the entry-count bound still holds")
	// Everything past the live window in the backing array must be zeroed.
	full := p.history[:cap(p.history)]
	for i := len(p.history); i < len(full); i++ {
		assert.Empty(t, full[i].DSL, "evicted entry %d still reachable from the backing array", i)
	}
}

// F12: AfterParse measured only GetClauses and GetPreloads. A sort= directive
// lands in f.sort, reachable only through GetSort, so no ORDER BY column ever
// entered the measured field set: under MaxFieldCount=2 a DSL naming fifty
// thousand sort columns was reported within policy.
func TestF12_LimitsMeasureSortColumns(t *testing.T) {
	cols := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		cols = append(cols, "c"+string(rune('0'+i))+":asc")
	}
	dsl := "a=1 sort=" + strings.Join(cols, ",")

	// The control proves the plugin is live on the filter path.
	{
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxFieldCount: 2})))
		err := f.AddFiltersFromString("a=1 and b=2 and c=3 and d=4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MaxFieldCount")
	}

	// Sort columns are NOT folded into MaxFieldCount: that would silently lower
	// every existing DefaultQueryLimits user's filter budget, which is a
	// breaking change for no security gain. The gap was that sort columns were
	// bounded by nothing at all.
	{
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxFieldCount: 2})))
		assert.NoError(t, f.AddFiltersFromString(dsl),
			"ORDER BY columns must not consume the filter-field budget")
	}

	// They have a bound of their own, which is what closes the gap.
	{
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxSortColumns: 3})))
		err := f.AddFiltersFromString(dsl)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MaxSortColumns")
	}

	// A query inside both bounds is untouched.
	{
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxFieldCount: 10, MaxSortColumns: 5})))
		assert.NoError(t, f.AddFiltersFromString("a=1 sort=b:asc,c:desc"))
	}

	// MaxExpressionCount deliberately still excludes sort columns: sorting is
	// not filter complexity, which is what that limit counts.
	{
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(NewLimitsPlugin(QueryLimits{MaxExpressionCount: 2})))
		assert.NoError(t, f.AddFiltersFromString(dsl),
			"sort columns must not be counted as expression nodes")
	}
}

// F5, end to end: a destructive diagnostic carrying the phrase the old
// substring classifier looked for must still fail closed. The guard's contract
// is that a caller who IGNORES the error runs a query matching nothing rather
// than an unfiltered scan.
func TestF5_LaunderedDiagnosticStillFailsClosed(t *testing.T) {
	for _, dsl := range []string{
		`load=[X:empty element in list`,
		`name="empty element in list and tenant_id=42`,
		`load=[X:produced no conditions; relation preloaded unfiltered`,
	} {
		t.Run(dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), dsl)
			assert.Error(t, err, "the rejection signal must survive")
			assert.Equal(t, "1=0", where, "and the query must fail closed")
		})
	}

	// The genuinely informational cases are still not rejections.
	for _, dsl := range []string{`id<in>[1,2,]`, `id=1 load=[Orders:]`} {
		t.Run("informational/"+dsl, func(t *testing.T) {
			err, where := guarded(t, NewInjectionGuardPlugin(), dsl)
			assert.NoError(t, err, "an informational diagnostic must not become a rejection")
			assert.NotEqual(t, "1=0", where)
		})
	}
}
