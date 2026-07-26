package plugins_test

import (
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/bi0dread/figo/v4/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------------------
// S1 — ScopePlugin's mandatory filter reached the top-level query only. On GORM
// a preload is a SEPARATE query, so a correctly scoped parent came back holding
// another tenant's children. The ExprFilter hook already ran over preload
// conditions (FieldsPlugin pruned them), but no hook could ADD to them, so the
// gap was not closeable by a caller. figo.PreloadFinalizer closes it.
// ---------------------------------------------------------------------------

type scopeUser struct {
	ID       uint `gorm:"primarykey"`
	TenantID string
	Name     string
	Orders   []scopeOrder `gorm:"foreignKey:ScopeUserID"`
}

type scopeOrder struct {
	ID          uint `gorm:"primarykey"`
	ScopeUserID uint
	TenantID    string
	Secret      string
}

func seedScopeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&scopeUser{}, &scopeOrder{}))
	require.NoError(t, db.Create(&scopeUser{ID: 1, TenantID: "A", Name: "alice"}).Error)
	// Child rows deliberately mis-parented across tenants: the case a mandatory
	// scope exists to catch.
	require.NoError(t, db.Create(&scopeOrder{ID: 10, ScopeUserID: 1, TenantID: "A", Secret: "A-order"}).Error)
	require.NoError(t, db.Create(&scopeOrder{ID: 11, ScopeUserID: 1, TenantID: "B", Secret: "B-SECRET-LEAK"}).Error)
	return db
}

func TestS1_PreloadScopeKeepsAnotherTenantsChildrenOut(t *testing.T) {
	db := seedScopeDB(t)

	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: "A"})
	sp.AddPreloadScope("Orders", figo.EqExpr{Field: "tenant_id", Value: "A"})

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(sp))
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:id>0]`))
	require.NoError(t, f.BuildE(adapters.GormAdapter{}))

	var users []scopeUser
	require.NoError(t, adapters.ApplyGorm(f, db.Model(&scopeUser{})).Find(&users).Error)

	require.Len(t, users, 1)
	assert.Equal(t, "A", users[0].TenantID)
	for _, o := range users[0].Orders {
		assert.Equal(t, "A", o.TenantID, "a preload must not return another tenant's child rows: %+v", o)
	}
	assert.Len(t, users[0].Orders, 1)
}

func TestS1_PreloadScopeAllAppliesToEveryRelation(t *testing.T) {
	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: "A"})
	sp.AddPreloadScopeAll(figo.EqExpr{Field: "tenant_id", Value: "A"})

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(sp))
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:id>0] load=[Invoices:id>0]`))
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	pre := f.GetPreloads()
	for _, rel := range []string{"Orders", "Invoices"} {
		require.Contains(t, pre, rel)
		assert.Contains(t, pre[rel], figo.EqExpr{Field: "tenant_id", Value: "A"},
			"relation %s did not receive the mandatory scope: %+v", rel, pre[rel])
	}
}

func TestS1_UnscopedPreloadIsUnchangedByDefault(t *testing.T) {
	// Default behaviour is deliberately unchanged: which column scopes a child
	// table is a property of that table, so nothing is inferred.
	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenant_id", Value: "A"})

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(sp))
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:id>0]`))
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	assert.Equal(t, []figo.Expr{figo.GtExpr{Field: "id", Value: int64(0)}}, f.GetPreloads()["Orders"])
}

// ---------------------------------------------------------------------------
// A3/A3-1 — the scope column never passed through the NamingFunc, because it
// enters through FinalizeClauses rather than AddFilter.
// ---------------------------------------------------------------------------

func TestA3_1_ScopeColumnGoesThroughTheNamingFunc(t *testing.T) {
	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenantID", Value: "A"})

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(sp))
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	where, args, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Contains(t, where, "tenant_id", "scope must be naming-converted like every other field: %s", where)
	assert.NotContains(t, where, "tenantID")
	assert.Equal(t, []any{"A"}, args)
}

func TestA3_1_PreloadScopeColumnGoesThroughTheNamingFunc(t *testing.T) {
	sp := plugins.NewScopePlugin(figo.EqExpr{Field: "tenantID", Value: "A"})
	sp.AddPreloadScope("Orders", figo.EqExpr{Field: "tenantID", Value: "A"})

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(sp))
	require.NoError(t, f.AddFiltersFromString(`load=[Orders:id>0]`))
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	assert.Contains(t, f.GetPreloads()["Orders"], figo.EqExpr{Field: "tenant_id", Value: "A"})
}

// ---------------------------------------------------------------------------
// H6/P7 — the whitelist projection fallback was sticky: once FieldsPlugin
// narrowed the projection through SetSelectFields (the same public setter the
// caller uses), the caller's request was gone, so widening the policy and
// rebuilding still rendered the narrowed set.
// ---------------------------------------------------------------------------

func TestP7_ProjectionPruningIsNotSticky(t *testing.T) {
	fp := plugins.NewFieldsPlugin()
	fp.SetAllowedFields("id")
	fp.EnableFieldWhitelist()

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(fp))
	f.SetSelectFields("id", "salary")
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	assert.NotContains(t, f.GetSelectFields(), "salary", "the forbidden column must not be projected")

	// Widen the policy and rebuild: the caller's original request must be
	// re-derived rather than the previous render's narrowed set.
	fp.DisableFieldWhitelist()
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))

	assert.Contains(t, f.GetSelectFields(), "salary",
		"the caller's projection request must survive a previous Build's pruning")
	assert.Contains(t, f.GetSelectFields(), "id")
}

func TestP7_PruningStillAppliesOnEveryRebuild(t *testing.T) {
	// The counterpart guarantee: re-deriving the request must not stop the
	// policy applying on later builds.
	fp := plugins.NewFieldsPlugin()
	fp.SetAllowedFields("id")
	fp.EnableFieldWhitelist()

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(fp))
	f.SetSelectFields("id", "salary")

	for i := 0; i < 3; i++ {
		require.NoError(t, f.BuildE(adapters.RawAdapter{}))
		assert.NotContains(t, f.GetSelectFields(), "salary", "build %d leaked the forbidden column", i)
	}
}

// ---------------------------------------------------------------------------
// H6/P4 (core half) — ExecuteBeforeParse short-circuited on the first error, so
// an AuditPlugin registered AFTER a rejecting plugin recorded nothing and the
// audit trail's blind spot was exactly the malformed/probing input.
// ---------------------------------------------------------------------------

type rejectingBeforeParse struct{}

func (rejectingBeforeParse) Name() string                         { return "rejector" }
func (rejectingBeforeParse) Version() string                      { return "1.0.0" }
func (rejectingBeforeParse) Initialize(figo.Figo) error           { return nil }
func (rejectingBeforeParse) BeforeQuery(figo.Figo, any) error     { return nil }
func (rejectingBeforeParse) AfterQuery(figo.Figo, any, any) error { return nil }
func (rejectingBeforeParse) AfterParse(figo.Figo, string) error   { return nil }
func (rejectingBeforeParse) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	return dsl, assert.AnError
}

func TestP4_AuditRecordsARejectedDSLInEitherRegistrationOrder(t *testing.T) {
	for _, auditFirst := range []bool{true, false} {
		ap := plugins.NewAuditPlugin(nil, 32)
		f := figo.New()
		if auditFirst {
			require.NoError(t, f.RegisterPlugin(ap))
			require.NoError(t, f.RegisterPlugin(rejectingBeforeParse{}))
		} else {
			require.NoError(t, f.RegisterPlugin(rejectingBeforeParse{}))
			require.NoError(t, f.RegisterPlugin(ap))
		}

		err := f.AddFiltersFromString(`secret=probe`)
		require.Error(t, err, "the rejection must still reject")

		assert.NotEmpty(t, ap.History(),
			"audit recorded nothing (auditFirst=%v): a rejected DSL is exactly what the trail must show", auditFirst)
	}
}

// ---------------------------------------------------------------------------
// A9/A7-2 (code half) — GetPluginManager handed out a nil *PluginManager on any
// instance with no plugin registered, and every method the docs show on the
// result panicked.
// ---------------------------------------------------------------------------

func TestA7_2_GetPluginManagerIsUsableOnAFreshInstance(t *testing.T) {
	assert.NotPanics(t, func() {
		pm := figo.New().GetPluginManager()
		require.NotNil(t, pm)
		assert.Empty(t, pm.ListPlugins())
		_, ok := pm.GetPlugin("nope")
		assert.False(t, ok)
	})

	assert.NotPanics(t, func() {
		pm := figo.New().Clone().GetPluginManager()
		require.NotNil(t, pm)
		assert.Empty(t, pm.ListPlugins())
	})
}

// ---------------------------------------------------------------------------
// P8-4 (hunt #8) — FieldsPlugin's fail-closed OrExpr{} substitution is correct
// per render, but `f.clauses = finalized` made it the instance's authoritative
// clause list. On a DSL-less instance the never-true pill then became the INPUT
// to the next render, and no public call recovered it (AddFilter only appends,
// so the result stayed 1=0 forever). The clause list is now split into what the
// caller asked for and what this render produced, exactly as the projection is.
// ---------------------------------------------------------------------------

func TestP8_4_FailClosedPillDoesNotBrickTheInstance(t *testing.T) {
	fp := plugins.NewFieldsPlugin()
	fp.AddIgnoreFields("salary")

	f := figo.New()
	require.NoError(t, f.RegisterPlugin(fp))
	f.AddFilter(figo.EqExpr{Field: "tenant_id", Value: 7})
	f.SetSelectFields("salary") // every projected column forbidden

	require.NoError(t, f.BuildE(adapters.RawAdapter{}))
	where, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "1=0", where, "a wholly-forbidden projection must still fail closed for THIS render")

	// Widening the request must recover the instance on the next Build.
	f.SetSelectFields("tenant_id")
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))
	where2, args2, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "`tenant_id` = ?", where2, "the pill must not survive into the next render")
	assert.Equal(t, []any{7}, args2)

	// ...and stay recovered (Build is idempotent).
	require.NoError(t, f.BuildE(adapters.RawAdapter{}))
	where3, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, where2, where3)
}
