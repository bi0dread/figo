package figo_test

// Regressions found by bug hunt #8 in the core parser/lifecycle (figo.go).
// Every test is named for its finding id and fails on ef28331.

import (
	"fmt"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func h8New(dsl string) figo.Figo {
	f := figo.New()
	f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
	_ = f.AddFiltersFromString(dsl)
	return f
}

func h8DB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// CORE-1: the load= bracket rescan was quote-BLIND while the tokenizer that
// produced the token was not, so a '[' or ']' inside a quoted value truncated
// the directive and DELETED the rest of the WHERE clause — the tenant predicate
// in `load=[Orders:name="a]b"] and tenant_id=7` vanished and the statement
// matched every row. Must be closed WITHOUT reopening hunt-#6 H6, whose fix
// (rescan from the directive's own '[') is what opened this door: the glued
// `load=[Orders:id=1]and tenant_id=7` has to keep parsing correctly.
func TestCORE1_LoadDirectiveEndIsQuoteAware(t *testing.T) {
	cases := []struct {
		dsl      string
		where    string
		relation string
		cond     string
	}{
		{`load=[Orders:name="a]b"] and tenant_id=7`, "`tenant_id` = ?", "Orders", `a]b`},
		{`load=[Orders:name="a[b"] and tenant_id=7`, "`tenant_id` = ?", "Orders", `a[b`},
		{`tenant_id=7 and load=[Orders:name="x]y"] and role="admin"`, "(`tenant_id` = ? AND `role` = ?)", "Orders", `x]y`},
		// H6's own shape: a keyword glued to the closing bracket.
		{`load=[Orders:id=1]and tenant_id=7`, "`tenant_id` = ?", "Orders", ""},
		{`load=[Orders:id=1] and tenant_id=7`, "`tenant_id` = ?", "Orders", ""},
	}
	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			f := h8New(c.dsl)
			require.NoError(t, f.BuildE(nil), "valid DSL must not report a diagnostic")
			where, args, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Equal(t, c.where, where, "the main WHERE must survive the load= directive")
			assert.NotEmpty(t, args)
			preloads := f.GetPreloads()
			require.Contains(t, preloads, c.relation)
			if c.cond != "" {
				require.Len(t, preloads[c.relation], 1)
				eq, ok := preloads[c.relation][0].(figo.EqExpr)
				require.True(t, ok, "got %#v", preloads[c.relation][0])
				assert.Equal(t, c.cond, eq.Value, "the quoted value keeps its bracket")
			}
		})
	}

	t.Run("bracketInsideQuotedListMember", func(t *testing.T) {
		f := h8New(`load=[Orders:tags<in>["a]b","c"]] and tenant_id=7`)
		require.NoError(t, f.BuildE(nil))
		where, _, err := adapters.BuildRawWhere(f)
		require.NoError(t, err)
		assert.Equal(t, "`tenant_id` = ?", where)
		in, ok := f.GetPreloads()["Orders"][0].(figo.InExpr)
		require.True(t, ok)
		assert.Equal(t, []any{"a]b", "c"}, in.Values)
	})

	// Row-level proof: the dropped predicate meant every tenant's rows came back.
	t.Run("executedAgainstSQLite", func(t *testing.T) {
		db := h8DB(t, "core1")
		require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS docs (id integer primary key, tenant_id int, name text)`).Error)
		db.Exec(`DELETE FROM docs`)
		db.Exec(`INSERT INTO docs (tenant_id, name) VALUES (7,'mine'),(99,'theirs'),(42,'third')`)

		f := figo.New()
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
		_ = f.AddFiltersFromString(`load=[Orders:name="a]b"] and tenant_id=7`)
		require.NoError(t, f.BuildE(nil))
		sql, args, err := adapters.BuildRawSelect(f, "docs")
		require.NoError(t, err)

		var tenants []int
		rows, err := db.Raw(sql, args...).Rows()
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id, tenant int
			var name string
			require.NoError(t, rows.Scan(&id, &tenant, &name))
			tenants = append(tenants, tenant)
		}
		assert.Equal(t, []int{7}, tenants, "sql=%s args=%v", sql, args)
	})
}

// CORE-3: a directive absorbed the PRECEDING connector unconditionally, so when
// it was the user's ONLY connector the disjunction silently became a
// conjunction — a narrower query, straight from untrusted DSL, with BuildE
// reporting nothing.
func TestCORE3_DirectiveKeepsTheUsersOnlyConnector(t *testing.T) {
	for _, dir := range []string{"sort=id:asc", "page=skip:1,take:2", "load=[Orders:id=1]", "page=garbage"} {
		for _, shape := range []string{"a=1 or %s b=2", "a=1 %s or b=2", "a=1 or %s and b=2"} {
			dsl := fmt.Sprintf(shape, dir)
			t.Run(dsl, func(t *testing.T) {
				f := h8New(dsl)
				_ = f.BuildE(nil)
				where, args, err := adapters.BuildRawWhere(f)
				require.NoError(t, err)
				assert.Equal(t, "(`a` = ? OR `b` = ?)", where)
				assert.Equal(t, []any{int64(1), int64(2)}, args)
			})
		}
	}

	// M12's own shape — a directive BETWEEN two connectors — still absorbs one,
	// with no spurious diagnostic. Do not reopen it.
	for _, dsl := range []string{
		"a=1 or sort=id:asc or b=2",
		"a=1 or page=skip:1,take:2 or b=2",
		"a=1 or load=[Orders:id=1] or b=2",
	} {
		t.Run("M12 "+dsl, func(t *testing.T) {
			f := h8New(dsl)
			assert.NoError(t, f.BuildE(nil))
			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Equal(t, "(`a` = ? OR `b` = ?)", where)
		})
	}
	// A leading/trailing directive's connector has nothing to pair with, so it
	// is still absorbed silently (M12), and L5's "not" drain still applies.
	for _, tc := range []struct{ dsl, where string }{
		{"sort=id:asc and a=1", "`a` = ?"},
		{"a=1 or sort=id:asc", "`a` = ?"},
		{"a=1 and load=[Orders:id=1]", "`a` = ?"},
	} {
		t.Run("edge "+tc.dsl, func(t *testing.T) {
			f := h8New(tc.dsl)
			assert.NoError(t, f.BuildE(nil))
			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Equal(t, tc.where, where)
		})
	}
	t.Run("L5 not before a directive is dropped, not moved", func(t *testing.T) {
		f := h8New("a=1 or not sort=id:asc b=2")
		err := f.BuildE(nil)
		assert.Error(t, err, "dropping the 'not' is reported")
		where, _, rerr := adapters.BuildRawWhere(f)
		require.NoError(t, rerr)
		assert.Equal(t, "(`a` = ? OR `b` = ?)", where, "the 'not' must not jump onto b, and the OR must survive")
	})
}

// CORE-6: the same reconciliation was missing on the empty-GROUP branch, so a
// parenthesized directive left a connector unpaired and BuildE reported a
// "dangling connector" error for valid DSL — M12's symptom in another spelling.
func TestCORE6_ParenthesizedDirectiveHasNoDanglingConnectorError(t *testing.T) {
	for _, tc := range []struct{ dsl, where string }{
		{"a=1 or (sort=id:asc) or b=2", "(`a` = ? OR `b` = ?)"},
		{"a=1 and (sort=id:asc) and b=2", "(`a` = ? AND `b` = ?)"},
		{"a=1 or (page=skip:1,take:2) or b=2", "(`a` = ? OR `b` = ?)"},
		{"(sort=id:asc) and a=1", "`a` = ?"},
		{"a=1 or (load=[Orders:id=1]) or b=2", "(`a` = ? OR `b` = ?)"},
	} {
		t.Run(tc.dsl, func(t *testing.T) {
			f := h8New(tc.dsl)
			assert.NoError(t, f.BuildE(nil), "the built query expresses all of the input")
			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			assert.Equal(t, tc.where, where)
		})
	}
}

// CORE-2: SetSort converted the WHOLE dotted column name while AddFilter
// converts per '.'-separated segment, so one statement named two different
// identifiers for one input string and the ORDER BY silently stopped sorting.
func TestCORE2_SortAndFilterSpellOneIdentifierTheSameWay(t *testing.T) {
	for _, name := range []string{"orders.user_name", "orders.userName", "userName"} {
		t.Run(name, func(t *testing.T) {
			f := figo.New() // default SnakeCaseNaming
			f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
			f.AddFilter(figo.NotNullExpr{Field: name})
			f.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{{Name: name}}})
			f.Build(nil)
			sql, _, err := adapters.BuildRawSelect(f, "orders")
			require.NoError(t, err)

			where, _, err := adapters.BuildRawWhere(f)
			require.NoError(t, err)
			ident := where[:len(where)-len(" IS NOT NULL")]
			assert.Contains(t, sql, "ORDER BY "+ident+" ASC",
				"WHERE and ORDER BY must name the same identifier (%s)", ident)
		})
	}

	t.Run("qualifiedSortActuallySortsOnSQLite", func(t *testing.T) {
		db := h8DB(t, "core2")
		require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS orders (id integer primary key, user_name text)`).Error)
		db.Exec(`DELETE FROM orders`)
		db.Exec(`INSERT INTO orders (user_name) VALUES ('zoe'),('amy'),('mel')`)

		f := figo.New()
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
		f.AddFilter(figo.NotNullExpr{Field: "orders.user_name"})
		f.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "orders.user_name"}}})
		f.Build(nil)
		sql, args, err := adapters.BuildRawSelect(f, "orders")
		require.NoError(t, err)

		var got []string
		rows, err := db.Raw(sql, args...).Rows()
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id int
			var n string
			require.NoError(t, rows.Scan(&id, &n))
			got = append(got, n)
		}
		assert.Equal(t, []string{"amy", "mel", "zoe"}, got, "sql=%s", sql)
	})

	t.Run("nonIdempotentNamingConvertsOnlyOnce", func(t *testing.T) {
		f := figo.New()
		f.SetNamingFunc(func(s string) string { return "t_" + s })
		f.SetSort(&figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "a.b"}}})
		require.NotNil(t, f.GetSort())
		assert.Equal(t, "t_a.t_b", f.GetSort().Columns[0].Name)
		f.SetSort(f.GetSort()) // the FieldsPlugin "re-set the survivors" shape
		assert.Equal(t, "t_a.t_b", f.GetSort().Columns[0].Name)
	})
}

// ---- plugins used by the I1/I2 tests -------------------------------------

// h8InertPlugin implements ONLY figo.Plugin: registering it must not change a
// single thing about what Build produces.
type h8InertPlugin struct{}

func (h8InertPlugin) Name() string                                      { return "h8-inert" }
func (h8InertPlugin) Version() string                                   { return "1" }
func (h8InertPlugin) Initialize(f figo.Figo) error                      { return nil }
func (h8InertPlugin) BeforeQuery(f figo.Figo, ctx any) error            { return nil }
func (h8InertPlugin) AfterQuery(f figo.Figo, c any, r any) error        { return nil }
func (h8InertPlugin) BeforeParse(f figo.Figo, d string) (string, error) { return d, nil }
func (h8InertPlugin) AfterParse(f figo.Figo, d string) error            { return nil }

// h8DropPreloads empties every preload condition list.
type h8DropPreloads struct{ h8InertPlugin }

func (h8DropPreloads) Name() string { return "h8-drop" }
func (h8DropPreloads) FinalizePreloads(f figo.Figo, rel string, conds []figo.Expr) []figo.Expr {
	return nil
}

type h8User struct {
	ID     uint
	Name   string
	Orders []h8Order
}

type h8Order struct {
	ID       uint
	H8UserID uint
	Total    int
}

// I1: the new PreloadFinalizer loop deleted a relation whenever the finalizer
// result was empty — but an unconditioned preload legitimately HAS no
// conditions, so registering any plugin at all (even one implementing no
// finalizer) silently stopped preloading it, on every adapter, with no
// diagnostic.
func TestI1_UnconditionedPreloadSurvivesPluginRegistration(t *testing.T) {
	for _, dsl := range []string{"id=1 load=[Orders:sort=id:desc]", "id=1 load=[Orders:]"} {
		for _, mode := range []string{"no plugin", "inert plugin", "getter only"} {
			t.Run(dsl+" / "+mode, func(t *testing.T) {
				f := figo.New()
				switch mode {
				case "inert plugin":
					require.NoError(t, f.RegisterPlugin(h8InertPlugin{}))
				case "getter only":
					require.NotNil(t, f.GetPluginManager())
				}
				f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
				_ = f.AddFiltersFromString(dsl)
				_ = f.BuildE(nil)
				assert.Contains(t, f.GetPreloads(), "Orders",
					"an unconditioned preload must not depend on whether a plugin manager exists")
			})
		}
	}

	t.Run("GORM really issues the preload query", func(t *testing.T) {
		db := h8DB(t, "i1gorm")
		require.NoError(t, db.AutoMigrate(&h8User{}, &h8Order{}))
		db.Exec(`DELETE FROM h8_users`)
		db.Exec(`DELETE FROM h8_orders`)
		require.NoError(t, db.Create(&h8User{ID: 1, Name: "u", Orders: []h8Order{{Total: 5}}}).Error)

		for _, withPlugin := range []bool{false, true} {
			f := figo.New()
			if withPlugin {
				require.NoError(t, f.RegisterPlugin(h8InertPlugin{}))
			}
			_ = f.AddFiltersFromString("id=1 load=[Orders:sort=id:desc]")
			f.Build(adapters.GormAdapter{})

			var users []h8User
			require.NoError(t, adapters.ApplyGorm(f, db.Model(&h8User{})).Find(&users).Error)
			require.Len(t, users, 1)
			assert.Len(t, users[0].Orders, 1, "withPlugin=%v: the relation must still be preloaded", withPlugin)
		}
	})

	// The declared PreloadFinalizer contract is unchanged for a preload that
	// HAS conditions: emptying that list still drops the relation.
	t.Run("a finalizer still drops a conditioned preload", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(h8DropPreloads{}))
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
		_ = f.AddFiltersFromString("id=1 load=[Orders:total>100]")
		_ = f.BuildE(nil)
		assert.NotContains(t, f.GetPreloads(), "Orders")
	})
}

// I1 (second half): GetPluginManager constructs the manager on first call, so a
// read-only-looking getter must not change what the next Build produces.
func TestI1_GetPluginManagerDoesNotChangeTheBuild(t *testing.T) {
	build := func(touch bool) (string, map[string][]figo.Expr) {
		f := figo.New()
		if touch {
			_ = f.GetPluginManager()
		}
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
		_ = f.AddFiltersFromString("id=1 load=[Orders:]")
		_ = f.BuildE(nil)
		where, _, _ := adapters.BuildRawWhere(f)
		return where, f.GetPreloads()
	}
	w1, p1 := build(false)
	w2, p2 := build(true)
	assert.Equal(t, w1, w2)
	assert.Equal(t, p1, p2)
}

// h8PanicFinalizer panics on its Nth FinalizeClauses call, after re-setting the
// sort the way FieldsPlugin's pruning does.
type h8PanicFinalizer struct {
	h8InertPlugin
	panicOn int
	calls   int
}

func (p *h8PanicFinalizer) Name() string { return "h8-panic" }
func (p *h8PanicFinalizer) FinalizeClauses(f figo.Figo, c []figo.Expr) []figo.Expr {
	p.calls++
	if s := f.GetSort(); s != nil {
		f.SetSort(s)
	}
	if p.calls == p.panicOn {
		panic("boom")
	}
	return c
}

// I2 (+ CRITIQUE §1c rider): finalizeClauses set inFinalize and snapshotted
// sortFromDSL but restored both on the normal return path only. A recovered
// finalizer panic — the case guardPluginPanic exists for — therefore stranded
// them: the caller could no longer change the projection at all, and the
// previous DSL's sort= leaked into a later query that named no sort.
func TestI2_FinalizerPanicDoesNotStrandPerBuildState(t *testing.T) {
	t.Run("projection", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(&h8PanicFinalizer{panicOn: 2}))
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
		f.SetSelectFields("id", "email", "password")
		_ = f.AddFiltersFromString("id=1")
		f.Build(nil)

		func() {
			defer func() { require.NotNil(t, recover(), "the panic still propagates") }()
			_ = f.AddFiltersFromString("id=1")
			f.Build(nil)
		}()

		f.SetSelectFields("id") // the caller narrows the projection after recovering
		_ = f.AddFiltersFromString("id=1")
		f.Build(nil)
		assert.Equal(t, map[string]bool{"id": true}, f.GetSelectFields())
		sql, _, err := adapters.BuildRawSelect(f, "users")
		require.NoError(t, err)
		assert.NotContains(t, sql, "password")
	})

	t.Run("sort ownership", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(&h8PanicFinalizer{panicOn: 2}))
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
		_ = f.AddFiltersFromString("a=1 sort=age:desc")
		f.Build(nil)

		func() {
			defer func() { require.NotNil(t, recover()) }()
			_ = f.AddFiltersFromString("a=1 sort=age:desc")
			f.Build(nil)
		}()

		_ = f.AddFiltersFromString("b=2") // a new DSL that names no sort
		f.Build(nil)
		assert.Nil(t, f.GetSort(), "a DSL-derived sort must not survive as caller-owned")
		sql, _, err := adapters.BuildRawSelect(f, "t")
		require.NoError(t, err)
		assert.NotContains(t, sql, "ORDER BY")
	})
}

// h8VetoPlugin vetoes the render from BeforeQuery.
type h8VetoPlugin struct{ h8InertPlugin }

func (h8VetoPlugin) Name() string                           { return "h8-veto" }
func (h8VetoPlugin) BeforeQuery(f figo.Figo, ctx any) error { return fmt.Errorf("denied") }

// A10-1: the raw adapter's new identifier validation made figo.GetQuery return
// an UNTYPED nil, so the documented `f.GetQuery(ctx).(figo.SQLQuery)` pattern
// became a panic reachable from a query string (?filter=a%00b=1), with BuildE
// silent. GetQuery must answer an adapter's render refusal with a typed,
// fail-closed Query instead.
func TestA10_1_GetQueryNeverReturnsAnUntypedNil(t *testing.T) {
	for _, dsl := range []string{"a\x00b=1", "a=1 and sec\x00ret=\"x\"", "...<0"} {
		t.Run(dsl, func(t *testing.T) {
			f := h8New(dsl)
			_ = f.BuildE(nil)
			q := f.GetQuery(adapters.RawContext{Table: "t"})
			require.NotNil(t, q)
			var sq figo.SQLQuery
			require.NotPanics(t, func() { sq = q.(figo.SQLQuery) }, "README's retrieval pattern")
			assert.Equal(t, "1=0", sq.SQL, "a refused render must not be able to return a row")
			assert.Empty(t, sq.Args)
		})
	}

	t.Run("a successful render is untouched", func(t *testing.T) {
		f := h8New("a=1")
		f.Build(nil)
		q, ok := f.GetQuery(adapters.RawContext{Table: "t"}).(figo.SQLQuery)
		require.True(t, ok)
		assert.Contains(t, q.SQL, "SELECT")
	})

	// A plugin veto is the plugin author's decision, not a render failure: it
	// keeps the documented nil (plugins/core_integration_test.go pins this).
	t.Run("a hook veto still returns nil", func(t *testing.T) {
		f := figo.New()
		require.NoError(t, f.RegisterPlugin(h8VetoPlugin{}))
		f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
		_ = f.AddFiltersFromString("a=1")
		f.Build(nil)
		assert.Nil(t, f.GetQuery(adapters.RawContext{Table: "t"}))
	})
}

// A10-2: GetClauses/GetPreloads must keep deep-copying for public callers (M3),
// but a renderer only reads, and the deep copy is O(total bound-value size) on
// the hot path. The read-only accessors give in-tree adapters the parent's cost
// back without weakening the public snapshot.
func TestA10_2_RenderAccessorsSkipTheDeepCopy(t *testing.T) {
	f := figo.New()
	_ = f.AddFiltersFromString("tags<in>[1,2,3] load=[Orders:total>1]")
	f.Build(adapters.RawAdapter{})

	r, ok := f.(interface {
		ClausesForRender() []figo.Expr
		PreloadsForRender() map[string][]figo.Expr
	})
	require.True(t, ok, "the in-tree render accessors must exist")

	live := r.ClausesForRender()
	again := r.ClausesForRender()
	require.NotEmpty(t, live)
	assert.Equal(t, f.GetClauses(), live, "same content as the public snapshot")

	valsOf := func(clauses []figo.Expr) []any {
		and, ok := clauses[0].(figo.AndExpr)
		if ok {
			for _, o := range and.Operands {
				if in, ok := o.(figo.InExpr); ok {
					return in.Values
				}
			}
		}
		if in, ok := clauses[0].(figo.InExpr); ok {
			return in.Values
		}
		return nil
	}
	a, b := valsOf(live), valsOf(again)
	require.NotEmpty(t, a)
	require.NotEmpty(t, b)
	assert.Same(t, &a[0], &b[0], "no per-call deep copy of the bound values")

	snap1, snap2 := valsOf(f.GetClauses()), valsOf(f.GetClauses())
	require.NotEmpty(t, snap1)
	assert.NotSame(t, &snap1[0], &snap2[0], "GetClauses still hands out independent copies (M3)")

	pre := r.PreloadsForRender()
	assert.Equal(t, f.GetPreloads(), pre)
}

// A10-2, quantified. Run with:
//
//	go test -run XXX -bench H8Core -benchmem .
//
// At 6bb998c GetClauses cost 23ns/16B/1alloc regardless of the bound values; at
// ef28331 it is 1.25us/2.0KB/43allocs for 40 clauses and 26us/82KB for a
// 5000-element <in> list. ClausesForRender restores the parent's cost (24ns/16B)
// for the in-tree render paths.
func benchInstance(inList int) figo.Figo {
	f := figo.New()
	f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
	dsl := "a=1 and b=2 and c=3"
	if inList > 0 {
		dsl += " and tags<in>["
		for i := 0; i < inList; i++ {
			if i > 0 {
				dsl += ","
			}
			dsl += "1"
		}
		dsl += "]"
	}
	dsl += " load=[Orders:total>1]"
	_ = f.AddFiltersFromString(dsl)
	f.Build(nil)
	return f
}

func BenchmarkH8CoreGetClausesInList5000(b *testing.B) {
	f := benchInstance(5000)
	for i := 0; i < b.N; i++ {
		_ = f.GetClauses()
	}
}

func BenchmarkH8CoreForRenderInList5000(b *testing.B) {
	f := benchInstance(5000)
	r := f.(interface{ ClausesForRender() []figo.Expr })
	for i := 0; i < b.N; i++ {
		_ = r.ClausesForRender()
	}
}

// CORE-4: AddFilter normalizes CustomExpr.Field like every other expression's
// field. That is the declared rule; the doc comment that said the handler owns
// naming was the part that was wrong. Pin the behaviour the comment now states.
func TestCORE4_CustomExprFieldFollowsTheDocumentedNamingRule(t *testing.T) {
	seen := ""
	h := func(field, op string, v any) (string, []any, error) {
		seen = field
		return field + " " + op + " ?", []any{v}, nil
	}

	f := figo.New() // default SnakeCaseNaming
	f.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
	f.AddFilter(figo.CustomExpr{Field: "userName", Operator: "@>", Value: 1, Handler: h})
	f.Build(nil)
	_, _, err := adapters.BuildRawWhere(f)
	require.NoError(t, err)
	assert.Equal(t, "user_name", seen, "converted per the declared AddFilter rule")

	seen = ""
	g := figo.New()
	g.SetNamingFunc(figo.NoChangeNaming)
	g.SetAdapterObject(adapters.RawAdapter{Dialect: adapters.MySQLDialect})
	g.AddFilter(figo.CustomExpr{Field: "userName", Operator: "@>", Value: 1, Handler: h})
	g.Build(nil)
	_, _, err = adapters.BuildRawWhere(g)
	require.NoError(t, err)
	assert.Equal(t, "userName", seen, "NoChangeNaming opts out")
}
