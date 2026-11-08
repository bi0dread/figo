package figo_test

import (
	"reflect"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/bi0dread/figo/v4/plugins"
	"gorm.io/gorm"
)

type followupBlob []byte

func followupQuery(t *testing.T, value any) figo.Figo {
	t.Helper()
	f := figo.New()
	f.SetSelectFields("id")
	f.AddFilter(figo.EqExpr{Field: "payload", Value: value})
	auditBuild(t, f, adapters.RawAdapter{Dialect: adapters.SQLiteDialect})
	return f
}

func followupIDs(t *testing.T, db *gorm.DB, q figo.SQLQuery) []int {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := sqlDB.Query(q.SQL, q.Args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestFollowupCachedBinaryArgumentsAreIndependent(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (id INTEGER, payload BLOB)",
		"INSERT INTO users VALUES (1,X'616c696365'),(2,X'61646d696e')")
	for _, named := range []bool{false, true} {
		name := "bytes"
		if named {
			name = "named bytes"
		}
		t.Run(name, func(t *testing.T) {
			cache := auditCache(t)
			newQuery := func() figo.Figo {
				var value any = []byte("alice")
				if named {
					value = followupBlob("alice")
				}
				return followupQuery(t, value)
			}
			// Mutate both the miss result and a hit result. Each subsequent
			// request is independent of the instance that returned the buffer.
			for i := 0; i < 3; i++ {
				f := newQuery()
				got := cache.GetCachedQuery(f, "users").(figo.SQLQuery)
				want := f.GetQuery("users").(figo.SQLQuery)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("request %d received another caller's mutated arguments: got %#v, want %#v", i, got, want)
				}
				if ids := followupIDs(t, db, got); !reflect.DeepEqual(ids, []int{1}) {
					t.Fatalf("request %d matched the wrong row: %v", i, ids)
				}
				copy(reflect.ValueOf(got.Args[0]).Bytes(), "admin")
			}
			if stats := cache.Stats(); stats.Misses != 1 || stats.Hits != 2 {
				t.Fatalf("binary queries should still be cached: %+v", stats)
			}
		})
	}
}

func TestFollowupCacheSeparatesNilAndEmptyBinaryValues(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (id INTEGER, payload BLOB)",
		"INSERT INTO users VALUES (1,NULL),(2,X'')")
	for _, tc := range []struct {
		name   string
		values []any
	}{
		{"bytes nil first", []any{[]byte(nil), []byte{}}},
		{"bytes empty first", []any{[]byte{}, []byte(nil)}},
		{"named bytes nil first", []any{followupBlob(nil), followupBlob{}}},
		{"named bytes empty first", []any{followupBlob{}, followupBlob(nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := auditCache(t)
			for pass := 0; pass < 2; pass++ {
				for _, value := range tc.values {
					f := followupQuery(t, value)
					got := cache.GetCachedQuery(f, "users").(figo.SQLQuery)
					want := f.GetQuery("users").(figo.SQLQuery)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("nil and empty values shared an entry: got %#v, want %#v", got, want)
					}
					wantIDs := []int{2}
					if reflect.ValueOf(value).IsNil() {
						wantIDs = []int{} // SQL payload = NULL is unknown, so matches no rows.
					}
					if ids := followupIDs(t, db, got); !reflect.DeepEqual(ids, wantIDs) {
						t.Fatalf("value %#v matched %v, want %v", value, ids, wantIDs)
					}
				}
			}
			if stats := cache.Stats(); stats.Misses != 2 || stats.Hits != 2 {
				t.Fatalf("nil and empty values need separate reusable entries: %+v", stats)
			}
		})
	}
}

func TestFollowupClonePreservesBinaryQueryResults(t *testing.T) {
	db := auditDB(t, "CREATE TABLE users (id INTEGER, payload BLOB)",
		"INSERT INTO users VALUES (1,NULL),(2,X''),(3,X'61')")
	for _, tc := range []struct {
		name  string
		value []byte
		ids   []int
	}{
		{"nil", nil, []int{}},
		{"empty", []byte{}, []int{2}},
		{"nonempty", []byte("a"), []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := followupQuery(t, tc.value)
			original := f.GetQuery("users").(figo.SQLQuery)
			cloned := f.Clone().GetQuery("users").(figo.SQLQuery)
			if !reflect.DeepEqual(original, cloned) {
				t.Fatalf("Clone changed arguments: original=%#v, clone=%#v", original, cloned)
			}
			for _, q := range []figo.SQLQuery{original, cloned} {
				if ids := followupIDs(t, db, q); !reflect.DeepEqual(ids, tc.ids) {
					t.Fatalf("value %#v matched %v, want %v", tc.value, ids, tc.ids)
				}
			}
		})
	}
}

func TestFollowupCloningPreservesNilAndEmptyContainers(t *testing.T) {
	for _, value := range []any{
		[]byte(nil), []byte{}, []any(nil), []any{}, map[string]any(nil), map[string]any{},
		followupBlob(nil), followupBlob{}, []string(nil), []string{},
		map[string]int(nil), map[string]int{},
		[]any{[]byte(nil), []any(nil), map[string]any(nil)},
		map[string]any{"nil": []byte(nil), "empty": []byte{}},
	} {
		f := followupQuery(t, value)
		for _, copied := range []figo.Expr{
			figo.CloneExpr(figo.EqExpr{Field: "payload", Value: value}),
			f.GetClauses()[0],
			f.Clone().GetClauses()[0],
		} {
			if got := copied.(figo.EqExpr).Value; !reflect.DeepEqual(got, value) {
				t.Fatalf("copy changed container shape: got %#v, want %#v", got, value)
			}
		}
	}
}

func TestFollowupParenthesizedListsPreserveWhitespaceAndFollowingFilters(t *testing.T) {
	for _, tc := range []struct {
		dsl  string
		args []any
	}{
		{`id<in>(1, 2) and tenant_id=7`, []any{int64(1), int64(2), int64(7)}},
		{`id <in> (1, 2) and tenant_id=7`, []any{int64(1), int64(2), int64(7)}},
		{`id<in>[1, 2] and tenant_id=7`, []any{int64(1), int64(2), int64(7)}},
		{`id<nin>( 1, 2 )and tenant_id=7`, []any{int64(1), int64(2), int64(7)}},
		{"id<in>(1,\t2,\n3) and tenant_id=7", []any{int64(1), int64(2), int64(3), int64(7)}},
		{`name<in>( "a,b", "c)d" ) and tenant_id=7`, []any{"a,b", "c)d", int64(7)}},
		{`(id<in>(1, 2) or id=3) and tenant_id=7`, []any{int64(1), int64(2), int64(3), int64(7)}},
		{`id<bet>( 1 .. 2 ) and tenant_id=7`, []any{int64(1), int64(2), int64(7)}},
		{`name=Report(2024 and tenant_id=7`, []any{"Report(2024", int64(7)}},
	} {
		t.Run(tc.dsl, func(t *testing.T) {
			for _, guarded := range []bool{false, true} {
				f := figo.New()
				if guarded {
					if err := f.RegisterPlugin(plugins.NewInjectionGuardPlugin()); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.AddFiltersFromString(tc.dsl); err != nil {
					t.Fatal(err)
				}
				auditBuild(t, f, adapters.RawAdapter{})
				q := f.GetQuery("users").(figo.SQLQuery)
				if !reflect.DeepEqual(q.Args, tc.args) {
					t.Fatalf("guard=%t: arguments changed: got %#v, want %#v", guarded, q.Args, tc.args)
				}
			}
		})
	}
}

func TestFollowupUnclosedParenthesizedListsStillRejected(t *testing.T) {
	for _, dsl := range []string{
		`id<in>(1, 2 and tenant_id=7`, `id<nin>(1, 2 and tenant_id=7`,
		`id <in> (1, 2 and tenant_id=7`, `id<bet>( 1 .. 2 and tenant_id=7`,
		`id <bet> ( 1 .. 2 and tenant_id=7`,
	} {
		t.Run(dsl, func(t *testing.T) {
			f := figo.New()
			if err := f.AddFiltersFromString(dsl); err != nil {
				t.Fatal(err)
			}
			if err := f.BuildE(adapters.RawAdapter{}); err == nil || figo.IsInformationalDiag(err) {
				t.Fatalf("unclosed value must produce a destructive diagnostic, got %v", err)
			}
			f = figo.New()
			if err := f.RegisterPlugin(plugins.NewInjectionGuardPlugin()); err != nil {
				t.Fatal(err)
			}
			if err := f.AddFiltersFromString(dsl); err == nil {
				t.Fatal("guard accepted an unclosed value")
			}
			auditBuild(t, f, adapters.RawAdapter{})
			where, _, err := adapters.BuildRawWhere(f)
			if err != nil || where != "1=0" {
				t.Fatalf("rejected query must match no rows: where=%q err=%v", where, err)
			}
		})
	}
}
