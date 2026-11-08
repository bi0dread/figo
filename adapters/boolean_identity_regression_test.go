package adapters

import (
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Check result sets against explicit truth tables, rather than comparing one
// adapter's rendering with another adapter that could make the same mistake.
func TestBooleanEmptyGroupIdentities(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.Exec("CREATE TABLE items (id INTEGER)").Error)
	require.NoError(t, db.Exec("INSERT INTO items VALUES (1), (2)").Error)
	docs := []bson.M{{"id": 1}, {"id": 2}}

	one := figo.EqExpr{Field: "id", Value: 1}
	two := figo.EqExpr{Field: "id", Value: 2}
	and := func(es ...figo.Expr) figo.Expr { return figo.AndExpr{Operands: es} }
	or := func(es ...figo.Expr) figo.Expr { return figo.OrExpr{Operands: es} }
	not := func(es ...figo.Expr) figo.Expr { return figo.NotExpr{Operands: es} }
	all, none := []int{1, 2}, []int{}
	cases := []struct {
		name string
		expr figo.Expr
		want []int
	}{
		{"empty and", and(), all},
		{"empty or", or(), none},
		{"empty not", not(), all},
		{"nil or operand", or(nil, one), []int{1}},
		{"all nil or operands", or(nil, nil), none},
		{"true before or predicate", or(and(), one), all},
		{"true after or predicate", or(one, and()), all},
		{"or of only true", or(and()), all},
		{"true and false", or(and(), or()), all},
		{"empty not inside or", or(not(), one), all},
		{"nested true inside or", or(and(and()), one), all},
		{"nor with true first", not(and(), one), none},
		{"nor with true last", not(one, and()), none},
		{"not of or with true", not(or(and(), one)), none},
		{"double not of or with true", not(not(or(one, and()))), all},
		{"true does not remove sibling filter", and(two, or(one, and())), []int{2}},
		{"true before sibling filter", and(or(one, and()), two), []int{2}},
		{"nested false does not remove sibling alternative", or(two, not(or(one, and()))), []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, backend := range []string{"raw", "gorm", "mongo", "elasticsearch"} {
				t.Run(backend, func(t *testing.T) {
					f := figo.New()
					f.AddFilter(tc.expr)
					f.SetSelectFields("id")
					got := []int{}
					switch backend {
					case "raw":
						require.NoError(t, f.BuildE(RawAdapter{Dialect: SQLiteDialect}))
						query, args, err := BuildRawSelect(f, "items")
						require.NoError(t, err)
						require.NoError(t, db.Raw(query, args...).Scan(&got).Error, query)
					case "gorm":
						require.NoError(t, f.BuildE(GormAdapter{}))
						require.NoError(t, ApplyGorm(f, db.Table("items")).Scan(&got).Error)
					case "mongo":
						require.NoError(t, f.BuildE(MongoAdapter{}))
						filter, err := BuildMongoFilter(f)
						require.NoError(t, err)
						got, err = evalFilterIDs(filter, docs)
						require.NoError(t, err)
					case "elasticsearch":
						require.NoError(t, f.BuildE(ElasticsearchAdapter{}))
						query, err := BuildElasticsearchQuery(f)
						require.NoError(t, err)
						for _, doc := range docs {
							match, err := esEvalClause(query.Query, map[string]any(doc))
							require.NoError(t, err)
							if match {
								got = append(got, doc["id"].(int))
							}
						}
					}
					require.Equal(t, tc.want, got)
				})
			}
		})
	}
}

func TestBooleanNonPredicateOperandsStayIgnored(t *testing.T) {
	db := rawTestDB(t)
	order := figo.OrderBy{Columns: []figo.OrderByColumn{{Name: "id", Desc: true}}}
	for _, ignored := range []figo.Expr{order, figo.CustomExpr{
		Handler: func(string, string, any) (string, []any, error) { return "", []any{999}, nil },
	}} {
		f := figo.New()
		f.AddFilter(figo.OrExpr{Operands: []figo.Expr{ignored, figo.EqExpr{Field: "id", Value: 1}}})
		f.Build(RawAdapter{Dialect: SQLiteDialect})
		query := sqliteQuery(t, f)
		require.Equal(t, []any{1}, query.Args, "ignored fragments must not leave bind arguments")
		require.Equal(t, []int64{1}, queryIDs(t, db, query))
	}
	// Mongo also ignores OrderBy in boolean position; unlike an empty AND,
	// it must not make OR match every document.
	f := figo.New()
	f.AddFilter(figo.OrExpr{Operands: []figo.Expr{order, figo.EqExpr{Field: "id", Value: 1}}})
	f.Build(MongoAdapter{})
	filter, err := BuildMongoFilter(f)
	require.NoError(t, err)
	got, err := evalFilterIDs(filter, []bson.M{{"id": 1}, {"id": 2}})
	require.NoError(t, err)
	require.Equal(t, []int{1}, got)
}

func TestBooleanTrueOperandDoesNotHideRenderErrors(t *testing.T) {
	// Pointer expressions are unsupported by all four adapters. Even when a
	// sibling determines the boolean result, rendering must still report errors.
	invalid := &figo.EqExpr{Field: "id", Value: 1}
	for _, operands := range [][]figo.Expr{{figo.AndExpr{}, invalid}, {invalid, figo.AndExpr{}}} {
		for _, expr := range []figo.Expr{figo.OrExpr{Operands: operands}, figo.NotExpr{Operands: operands}} {
			f := figo.New()
			f.AddFilter(expr)
			f.Build(RawAdapter{})
			_, _, err := BuildRawWhere(f)
			require.Error(t, err)
			_, err = toGormClauseWithFigo(expr, f)
			require.Error(t, err)
			_, err = BuildMongoFilter(f)
			require.Error(t, err)
			_, err = BuildElasticsearchQuery(f)
			require.Error(t, err)
		}
	}
}
