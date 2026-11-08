package figo_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/bi0dread/figo/v4/plugins"
	"go.mongodb.org/mongo-driver/bson"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func auditCache(t *testing.T) *plugins.CachePlugin {
	t.Helper()
	p := plugins.NewCachePlugin(plugins.CacheConfig{Enabled: true, TTL: time.Minute, MaxSize: 10})
	t.Cleanup(p.Close)
	return p
}

func auditBuild(t *testing.T, f figo.Figo, a figo.Adapter) {
	t.Helper()
	if err := f.BuildE(a); err != nil {
		t.Fatal(err)
	}
}

func auditDB(t *testing.T, statements ...string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, s := range statements {
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestAuditCacheDoesNotMergeDistinctParameterLists(t *testing.T) {
	cache := auditCache(t)
	query := func(values []any) figo.Figo {
		f := figo.New()
		f.AddFilter(figo.InExpr{Field: "name", Values: values})
		auditBuild(t, f, adapters.RawAdapter{})
		return f
	}
	a := query([]any{"a;(string)b", "c"})
	b := query([]any{"a", "b;(string)c"})
	cache.GetCachedQuery(a, "users")
	got := cache.GetCachedQuery(b, "users").(figo.SQLQuery)
	want := b.GetQuery("users").(figo.SQLQuery)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache served another request's arguments: got %#v, want %#v; stats=%+v", got.Args, want.Args, cache.Stats())
	}
	// Correctness must not come from disabling caching for string values.
	cache.GetCachedQuery(a, "users")
	cache.GetCachedQuery(b, "users")
	if stats := cache.Stats(); stats.Misses != 2 || stats.Hits != 2 {
		t.Fatalf("distinct requests should each occupy a reusable entry: %+v", stats)
	}
}

func TestAuditCacheKeysTheNamingOfEveryProjectionSegment(t *testing.T) {
	cache := auditCache(t)
	query := func(mapped string) figo.Figo {
		f := figo.New()
		f.SetNamingFunc(func(s string) string {
			if s == "name" {
				return mapped
			}
			return s
		})
		f.SetSelectFields("users.name")
		auditBuild(t, f, adapters.RawAdapter{})
		return f
	}
	a, b := query("private_name"), query("public_name")
	cache.GetCachedSqlString(a, "users")
	got, want := cache.GetCachedSqlString(b, "users"), b.GetSqlString("users")
	if got != want {
		t.Fatalf("wrong projection from cache: got %s, want %s", got, want)
	}
}

func TestAuditFieldPruningPreservesFalseIdentity(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (id INTEGER)", "INSERT INTO users VALUES (1),(2)")
	for _, enabled := range []bool{false, true} {
		f := figo.New()
		if enabled {
			p := plugins.NewFieldsPlugin()
			p.AddIgnoreFields("unrelated")
			if err := f.RegisterPlugin(p); err != nil {
				t.Fatal(err)
			}
		}
		f.AddFilter(figo.AndExpr{Operands: []figo.Expr{figo.EqExpr{Field: "id", Value: 1}, figo.OrExpr{}}})
		auditBuild(t, f, adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
		sql, args, err := adapters.BuildRawSelect(f, "users", "id")
		if err != nil {
			t.Fatal(err)
		}
		var rows []int
		if err := db.Raw(sql, args...).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("ignoring an unrelated field changed false to true: policy=%t rows=%v sql=%s", enabled, rows, sql)
		}
	}
}

func TestAuditGuardRejectsUnclosedListThatSwallowsConjunction(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (id INTEGER, status TEXT, tenant_id INTEGER)", "INSERT INTO users VALUES (1,'active',7),(2,'active',99)")
	for _, dsl := range []string{
		`status<nin>[archived and tenant_id=7`,
		`status <nin> [archived and tenant_id=7`,
		`status<nin>[[archived]`,
		`status<in>(archived`,
		`status<nin>[archived) and tenant_id=7`,
	} {
		t.Run(dsl, func(t *testing.T) {
			bare := figo.New()
			if err := bare.AddFiltersFromString(dsl); err != nil {
				t.Fatal(err)
			}
			if err := bare.BuildE(nil); err == nil || figo.IsInformationalDiag(err) {
				t.Fatalf("malformed list must produce a destructive diagnostic, got %v", err)
			}
			f := figo.New()
			if err := f.RegisterPlugin(plugins.NewInjectionGuardPlugin()); err != nil {
				t.Fatal(err)
			}
			if err := f.AddFiltersFromString(dsl); err == nil {
				t.Fatal("guard accepted malformed list")
			}
			auditBuild(t, f, adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
			sql, args, err := adapters.BuildRawSelect(f, "users", "id")
			if err != nil {
				t.Fatal(err)
			}
			var rows []int
			if err := db.Raw(sql, args...).Scan(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("refused query returned rows %v: %s", rows, sql)
			}
		})
	}
}

func TestAuditListDelimiterValidationPreservesLiteralValues(t *testing.T) {
	for _, dsl := range []string{
		`status<nin>["a]b","["]`, `status<in>["(",")"]`,
		`status<in>(1,2)`, `status<in>["a,b","c"]`, `status<nin>[]`,
		`status<in>[Report(2024,3.14]`, `status<in>[[draft]]`,
		`status<in>"[draft"`, `status<in>a,b`,
	} {
		t.Run(dsl, func(t *testing.T) {
			f := figo.New()
			if err := f.AddFiltersFromString(dsl); err != nil {
				t.Fatal(err)
			}
			auditBuild(t, f, adapters.RawAdapter{})
			if len(f.GetClauses()) == 0 {
				t.Fatal("valid list was discarded")
			}
		})
	}
}

func TestAuditDSLKeepsQualifiedNamesConsistentWithProjection(t *testing.T) {
	f := figo.New()
	f.SetSelectFields("users.userName")
	if err := f.AddFiltersFromString(`users.userName="alice" sort=users.userName:desc`); err != nil {
		t.Fatal(err)
	}
	auditBuild(t, f, adapters.RawAdapter{})
	got, _, err := adapters.BuildRawSelect(f, "users")
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT `users`.`user_name` FROM `users` WHERE `users`.`user_name` = ? ORDER BY `users`.`user_name` DESC"
	if got != want {
		t.Fatalf("qualified names disagree: got %s; want %s", got, want)
	}
}

func TestAuditWhitelistKeepsItsOwnQualifiedField(t *testing.T) {
	f := figo.New()
	policy := plugins.NewFieldsPlugin()
	policy.SetAllowedFields("users.userName")
	policy.EnableFieldWhitelist()
	if err := f.RegisterPlugin(policy); err != nil {
		t.Fatal(err)
	}
	f.AddFilter(figo.EqExpr{Field: "users.userName", Value: "alice"})
	auditBuild(t, f, adapters.RawAdapter{})
	sql, args, err := adapters.BuildRawWhere(f)
	if err != nil {
		t.Fatal(err)
	}
	if sql == "" || len(args) != 1 {
		t.Fatalf("explicitly whitelisted field was dropped: WHERE=%q args=%v", sql, args)
	}
}

func TestAuditMongoProjectionHasNoParentChildCollision(t *testing.T) {
	for _, tc := range []struct{ fields, want []string }{
		{[]string{"a", "a-b", "a.b"}, []string{"a", "a-b"}},
		{[]string{"a.b", "a.b-c", "a.b.c"}, []string{"a.b", "a.b-c"}},
		{[]string{"a", "a!", "a.b", "ab"}, []string{"a", "a!", "ab"}},
	} {
		t.Run(strings.Join(tc.fields, ","), func(t *testing.T) {
			f := figo.New()
			f.SetNamingFunc(figo.NoChangeNaming)
			f.SetSelectFields(tc.fields...)
			auditBuild(t, f, adapters.MongoAdapter{})
			_, opts, err := adapters.AdapterMongoGetFind(f)
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := adapters.BuildMongoAggregatePipeline(f, nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, stage := range pipeline {
				if stage[0].Key != "$project" {
					continue
				}
				projection := stage[0].Value.(bson.D)
				found = true
				var keys []string
				for _, field := range projection {
					keys = append(keys, field.Key)
				}
				if !reflect.DeepEqual(keys, tc.want) {
					t.Fatalf("projection=%v, want %v", keys, tc.want)
				}
				if !reflect.DeepEqual(opts.Projection, projection) {
					t.Fatalf("find and aggregate projections differ: %#v vs %#v", opts.Projection, projection)
				}
				for _, parent := range projection {
					for _, child := range projection {
						if strings.HasPrefix(child.Key, parent.Key+".") {
							t.Fatalf("invalid MongoDB projection: %q and its child %q both survive: %#v", parent.Key, child.Key, projection)
						}
					}
				}
			}
			if !found {
				t.Fatal("missing projection stage")
			}
		})
	}
}

func TestAuditGormQuotesKeywordProjectionWithoutModel(t *testing.T) {
	for _, col := range []string{"null", "true", "false", "current_date", "1", "123e2"} {
		t.Run(col, func(t *testing.T) {
			db := auditDB(t, "CREATE TABLE users (id INTEGER, `"+col+"` TEXT)", "INSERT INTO users VALUES (1,'stored-value')")
			f := figo.New()
			f.SetSelectFields(col)
			auditBuild(t, f, adapters.GormAdapter{})
			var rows []map[string]any
			if err := adapters.ApplyGorm(f, db.Table("users")).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0][col] != "stored-value" {
				t.Fatalf("requested column rendered as SQL constant: query=%#v rows=%#v", f.GetQuery(db.Table("users")), rows)
			}
		})
	}
}

type auditTaggedProjection struct {
	DisplayName string `gorm:"column:display_text"`
}

func TestAuditGormQuotedProjectionKeepsModelFieldMapping(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (display_text TEXT)", "INSERT INTO users VALUES ('visible')")
	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	f.SetSelectFields("DisplayName")
	auditBuild(t, f, adapters.GormAdapter{})
	for _, modelTiming := range []string{"before apply", "after apply", "find destination"} {
		t.Run(modelTiming, func(t *testing.T) {
			q := db.Table("users")
			if modelTiming == "before apply" {
				q = q.Model(&auditTaggedProjection{})
			}
			q = adapters.ApplyGorm(f, q)
			if modelTiming == "after apply" {
				q = q.Model(&auditTaggedProjection{})
			}
			var rows []auditTaggedProjection
			if err := q.Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].DisplayName != "visible" {
				t.Fatalf("model column mapping was lost: %#v", rows)
			}
		})
	}
	var count int64
	if err := adapters.ApplyGorm(f, db.Table("users")).Model(&auditTaggedProjection{}).Distinct().Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("model column mapping was lost in Count: %d", count)
	}
	// The builder belongs only to the returned query, not the shared handle.
	if _, changed := db.ClauseBuilders["SELECT"]; changed {
		t.Fatal("ApplyGorm modified the caller's SELECT builder")
	}
}
