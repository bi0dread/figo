package figo

import (
	"testing"

	"strings"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestNew(t *testing.T) {
	f := New(nil)
	assert.NotNil(t, f)
	assert.Equal(t, 0, f.GetPage().Skip)
	assert.Equal(t, 20, f.GetPage().Take)
}

func TestAddBanFields(t *testing.T) {
	f := New(nil)
	f.AddIgnoreFields("sensitive_field", "internal_use_only")
	assert.True(t, f.GetIgnoreFields()["sensitive_field"])
	assert.True(t, f.GetIgnoreFields()["internal_use_only"])
}

func TestAddSelectFields(t *testing.T) {
	f := New(nil)
	f.AddSelectFields("field1", "field2")
	assert.True(t, f.GetSelectFields()["field1"])
	assert.True(t, f.GetSelectFields()["field2"])
}

func TestBuild(t *testing.T) {
	f := New(nil)

	f.AddFiltersFromString(`(id=1 or id=2) or id>=2 or id<=3 or id!=0 and vendor=vendor1 or name=ali and (place=tehran or place=shiraz or (v1=2 and v2=1 and (g1=0 or g1=2))) or GG=9 or GG=8 sort=id:desc,name:ace page=skip:10,take:10 load=[inner1:id=1 or name=ali | inner2:id=2 or name=ali]`)
	f.Build()
	assert.NotEmpty(t, f.GetClauses())
}

func TestAdapterSelection(t *testing.T) {
	f := New(GormAdapter{})
	if _, ok := f.GetAdapterObject().(GormAdapter); !ok {
		t.Fatalf("expected GormAdapter")
	}
	f.SetAdapterObject(RawAdapter{})
	if _, ok := f.GetAdapterObject().(RawAdapter); !ok {
		t.Fatalf("expected RawAdapter")
	}
}

func TestGormAndRawAdapters(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}

	type TestInnerModel1 struct {
		ID   int
		Name string
		XX   int
	}

	type TestInnerModel2 struct {
		ID   int
		Name string
		XX   int
	}

	// Define a model
	type TestModel struct {
		ID             int
		VendorID       int
		BankID         int
		ExpeditionType string
		TestInner1     []*TestInnerModel1 `gorm:"foreignKey:XX"`
		TestInner2     []*TestInnerModel2 `gorm:"foreignKey:XX"`
	}

	// Create the table
	err = db.AutoMigrate(&TestInnerModel1{})
	if err != nil {
		t.Fatalf("failed to migrate database: %v", err)
	}

	// Create the table
	err = db.AutoMigrate(&TestInnerModel2{})
	if err != nil {
		t.Fatalf("failed to migrate database: %v", err)
	}

	// Create the table
	err = db.AutoMigrate(&TestModel{})
	if err != nil {
		t.Fatalf("failed to migrate database: %v", err)
	}

	// Insert some test data
	db.Create(&TestModel{ID: 1, VendorID: 22, BankID: 12, ExpeditionType: "eq", TestInner1: []*TestInnerModel1{{XX: 1, ID: 3, Name: "test1"}}})
	db.Create(&TestModel{ID: 2, VendorID: 22, BankID: 13, ExpeditionType: "eq", TestInner2: []*TestInnerModel2{{XX: 2, ID: 4, Name: "test2"}}})

	// Build filters
	f := New(GormAdapter{})
	f.AddFiltersFromString(`load=[inner1:id=1 or name=ali | inner2:id=2 or name=ali] and gg=~"^ab.*" and (id=1 and vendorId="22") and bank_id=11 or expedition_type=^"%e%" sort=id:desc page=skip:0,take:10 and (id<in>[1,2,3] and name.=^"%ab%") and (price<bet>10..20 and deleted_at<null>) and kind<notnull> and status<nin>[x,y]`)
	f.Build()

	// GORM adapter - apply and get SQL (expanded by GORM explain)
	db2 := ApplyGorm(f, db.Model(&TestModel{}))
	s := f.GetSqlString(db2, "WHERE")
	assert.Contains(t, s, "IN (")
	assert.Contains(t, s, "LIKE")
	assert.Contains(t, s, "BETWEEN")
	assert.Contains(t, s, "IS NULL")
	assert.Contains(t, s, "IS NOT NULL")

	// RAW adapter - ensure placeholder expansion and segment order works
	f.SetAdapterObject(RawAdapter{})

	// Only WHERE + SORT + JOIN
	rawSql := f.GetSqlString(RawContext{Table: "test_models"}, "SELECT", "FROM", "WHERE", "SORT", "JOIN")
	assert.Contains(t, rawSql, "WHERE ")
}

func TestRawAdapterBuild(t *testing.T) {
	f := New(RawAdapter{})
	f.AddFiltersFromString(`(id=1 and vendorId="22") and bank_id=11 or expedition_type=^"%e%" sort=id:desc page=skip:0,take:10`)
	f.AddIgnoreFields("bank_id")
	f.Build()

	sql, args := BuildRawSelect(f, "test_models")
	assert.Equal(t, "SELECT * FROM `test_models` WHERE ((`id` = ? AND `vendor_id` = ?) OR `expedition_type` LIKE ?) ORDER BY `id` DESC LIMIT 10", sql)
	assert.Equal(t, []any{int64(1), "22", "%e%"}, args)
}

func TestMongoAdapterBuild(t *testing.T) {
	f := New(nil)
	f.AddFiltersFromString(`(id=1 and vendorId="22") and bank_id=11 or expedition_type=^"%e%" sort=id:desc page=skip:0,take:10`)
	f.AddIgnoreFields("bank_id")
	f.Build()

	// Filter
	filter := BuildMongoFilter(f)
	// Expect a top-level $or between (id AND vendor_id) and expedition_type like
	orVal, ok := filter["$or"].([]bson.M)
	assert.True(t, ok)
	assert.Len(t, orVal, 2)
	// Left side: $and with id and vendor_id
	leftAnd, ok := orVal[0]["$and"].([]bson.M)
	assert.True(t, ok)
	// Verify keys present with expected values
	// Order may vary
	var hasID, hasVendor bool
	for _, m := range leftAnd {
		if v, ok := m["id"]; ok && v == int64(1) {
			hasID = true
		}
		if v, ok := m["vendor_id"]; ok && v == "22" {
			hasVendor = true
		}
	}
	assert.True(t, hasID)
	assert.True(t, hasVendor)
	// Right side: expedition_type like (regex)
	right := orVal[1]
	if rv, ok := right["expedition_type"].(bson.M); ok {
		_, ok2 := rv["$regex"]
		assert.True(t, ok2)
	} else {
		t.Fatalf("expedition_type regex not found in filter")
	}

	// Options
	opts := BuildMongoFindOptions(f)
	if opts.Limit == nil || *opts.Limit != int64(10) {
		t.Fatalf("limit not set to 10")
	}
	if opts.Skip != nil {
		t.Fatalf("skip should be nil for 0")
	}
	if sd, ok := opts.Sort.(bson.D); ok {
		assert.Len(t, sd, 1)
		assert.Equal(t, "id", sd[0].Key)
		assert.Equal(t, -1, sd[0].Value)
	} else {
		t.Fatalf("sort not set as bson.D")
	}

	// Preloads to joins
	f2 := New(nil)
	f2.AddFiltersFromString(`load=[TestInner1:id="3" or name="test1" | TestInner2:id=4]`)
	f2.Build()
	joins := map[string]MongoJoin{
		"TestInner1": {From: "testinner1", LocalField: "id", ForeignField: "XX", As: "TestInner1"},
		"TestInner2": {From: "testinner2", LocalField: "id", ForeignField: "XX", As: "TestInner2"},
	}
	pipe := BuildMongoAggregatePipeline(f2, joins)
	// Expect at least two $lookup stages
	lookupCount := 0
	matchQualified := 0
	for _, stage := range pipe {
		var lookupVal any
		var matchVal any
		for _, e := range stage { // stage is a bson.D
			switch e.Key {
			case "$lookup":
				lookupVal = e.Value
			case "$match":
				matchVal = e.Value
			}
		}
		if lookupVal != nil {
			lookupCount++
		}
		if matchVal != nil {
			if mm, ok := matchVal.(bson.M); ok {
				// look for qualified keys
				for k := range mm {
					if strings.HasPrefix(k, "TestInner1.") || strings.HasPrefix(k, "TestInner2.") {
						matchQualified++
						break
					}
				}
			}
		}
	}
	assert.Equal(t, 2, lookupCount)
	assert.True(t, matchQualified >= 1)
}

func TestPageValidation(t *testing.T) {
	p := Page{Skip: -1, Take: 30}
	p.validate()
	assert.Equal(t, 0, p.Skip)
	assert.Equal(t, 30, p.Take)
}

func TestRawSelectFieldsColumns(t *testing.T) {
	f := New(RawAdapter{})
	f.AddSelectFields("id", "vendorId")
	f.Build()

	sql := f.GetSqlString(RawContext{Table: "test_models"}, "SELECT", "FROM")
	assert.Contains(t, sql, "SELECT ")
	assert.Contains(t, sql, "`id`")
	assert.Contains(t, sql, "`vendor_id`")
	assert.Contains(t, sql, " FROM `test_models`")
}

func TestGormSelectFieldsColumns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}

	type TestModel struct {
		ID       int
		VendorID int
	}

	if err := db.AutoMigrate(&TestModel{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	f := New(GormAdapter{})
	f.AddSelectFields("id", "vendorId")
	f.Build()

	db2 := ApplyGorm(f, db.Model(&TestModel{}))
	sql := f.GetSqlString(db2, "SELECT")
	assert.NotContains(t, sql, "SELECT *")
	assert.Contains(t, sql, "id")
	assert.Contains(t, sql, "vendor_id")
}

type dummyAdapter struct{}

func (dummyAdapter) GetSqlString(f Figo, ctx any, conditionType ...string) (string, bool) {
	return "DUMMY", true
}

func (dummyAdapter) GetQuery(f Figo, ctx any, conditionType ...string) (Query, bool) {
	return SQLQuery{SQL: "DUMMY", Args: nil}, true
}

func TestAdapterObjectDelegation(t *testing.T) {
	f := New(dummyAdapter{})
	out := f.GetSqlString(nil, "SELECT")
	assert.Equal(t, "DUMMY", out)
}

func TestGetQueryRaw(t *testing.T) {
	f := New(RawAdapter{})
	f.Build()
	q := f.GetQuery(RawContext{Table: "test_models"}, "SELECT", "FROM")
	sqlq, ok := q.(SQLQuery)
	assert.True(t, ok)
	assert.Equal(t, "SELECT * FROM `test_models`", sqlq.SQL)
}

func TestGetQueryGorm(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}
	type TestModel struct{ ID int }
	_ = db.AutoMigrate(&TestModel{})
	f := New(GormAdapter{})
	f.Build()
	db2 := ApplyGorm(f, db.Model(&TestModel{}))
	q := f.GetQuery(db2, "SELECT")
	_, ok := q.(SQLQuery)
	assert.True(t, ok)
}

func TestGetQueryMongoFindAndAgg(t *testing.T) {
	// FIND
	f := New(MongoAdapter{})
	f.AddFiltersFromString(`id=1 or expedition_type=^"%e%"`)
	f.Build()
	q := f.GetQuery(nil)
	_, isFind := q.(MongoFindQuery)
	assert.True(t, isFind)

	// AGGREGATE
	f2 := New(MongoAdapter{})
	f2.AddFiltersFromString(`load=[Rel:id=1]`)
	f2.Build()
	joins := map[string]MongoJoin{"Rel": {From: "rels", LocalField: "id", ForeignField: "pid", As: "Rel"}}
	q2 := f2.GetQuery(joins, "AGG")
	_, isAgg := q2.(MongoAggregateQuery)
	assert.True(t, isAgg)
}

func TestRawNewOperations(t *testing.T) {
	f := New(RawAdapter{})
	f.AddFilter(InExpr{Field: "id", Values: []any{1, 2, 3}})
	f.AddFilter(ILikeExpr{Field: "name", Value: "%ab%"})
	f.AddFilter(BetweenExpr{Field: "price", Low: 10, High: 20})
	f.AddFilter(IsNullExpr{Field: "deleted_at"})
	f.AddFilter(NotNullExpr{Field: "kind"})
	f.AddFilter(NotInExpr{Field: "status", Values: []any{"x", "y"}})
	f.Build()

	where, args := BuildRawWhere(f)
	assert.Contains(t, where, "`id` IN (")
	assert.Contains(t, where, "LOWER(`name`) LIKE LOWER(?)")
	assert.Contains(t, where, "`price` BETWEEN ? AND ?")
	assert.Contains(t, where, "`deleted_at` IS NULL")
	assert.Contains(t, where, "`kind` IS NOT NULL")
	assert.Contains(t, where, "`status` NOT IN (")
	assert.Equal(t, []any{1, 2, 3, "%ab%", 10, 20, "x", "y"}, args)
}

func TestGormNewOperations(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}
	type M struct{ ID int }
	_ = db.AutoMigrate(&M{})

	f := New(GormAdapter{})
	// DSL exercising new ops
	f.AddFiltersFromString(`(id<in>[1,2,3] and name.=^"%ab%") and (price<bet>(10..20) and deleted_at<null>) and kind<notnull> and status<nin>[x,y] and name=~"^ab.*"`)
	f.Build()
	db2 := ApplyGorm(f, db.Model(&M{}))
	s := f.GetSqlString(db2, "SELECT", "FROM", "WHERE")
	assert.Contains(t, s, "IN (")
	assert.Contains(t, s, "LIKE")
	assert.Contains(t, s, "BETWEEN")
	assert.Contains(t, s, "IS NULL")
	assert.Contains(t, s, "IS NOT NULL")
	assert.Contains(t, s, "REGEXP")
}

func TestMongoNewOperations(t *testing.T) {
	f := New(nil)
	f.AddFilter(InExpr{Field: "id", Values: []any{1, 2, 3}})
	f.AddFilter(ILikeExpr{Field: "name", Value: "%ab%"})
	f.AddFilter(BetweenExpr{Field: "price", Low: 10, High: 20})
	f.AddFilter(IsNullExpr{Field: "deleted_at"})
	f.AddFilter(NotNullExpr{Field: "kind"})
	f.AddFilter(NotInExpr{Field: "status", Values: []any{"x", "y"}})
	f.Build()

	m := BuildMongoFilter(f)
	// find id $in within top-level or $and aggregation
	foundIn := false
	if andList, ok := m["$and"].([]bson.M); ok {
		for _, it := range andList {
			if mv, ok2 := it["id"].(bson.M); ok2 {
				if _, ok3 := mv["$in"]; ok3 {
					foundIn = true
					break
				}
			}
		}
	} else if mv, ok := m["id"].(bson.M); ok {
		_, foundIn = mv["$in"]
	}
	if !foundIn {
		t.Fatalf("id $in missing")
	}
	// name ilike -> regex + options i (search similar way)
	foundILike := false
	if andList, ok := m["$and"].([]bson.M); ok {
		for _, it := range andList {
			if mv, ok2 := it["name"].(bson.M); ok2 {
				if _, ok3 := mv["$regex"]; ok3 {
					if opt, ok4 := mv["$options"]; ok4 && opt == "i" {
						foundILike = true
						break
					}
				}
			}
		}
	} else if mv, ok := m["name"].(bson.M); ok {
		if _, ok2 := mv["$regex"]; ok2 {
			if opt, ok4 := mv["$options"]; ok4 && opt == "i" {
				foundILike = true
			}
		}
	}
	if !foundILike {
		t.Fatalf("name ilike missing")
	}
}

func TestRegexSQLOperatorConfig(t *testing.T) {
	// ensure we restore default after test
	defer SetRegexSQLOperator("REGEXP")
	SetRegexSQLOperator("~*")

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}
	type M struct{ ID int }
	_ = db.AutoMigrate(&M{})

	f := New(GormAdapter{})
	f.AddFiltersFromString(`name=~"^ab.*"`)
	f.Build()
	db2 := ApplyGorm(f, db.Model(&M{}))
	s := f.GetSqlString(db2, "SELECT", "FROM", "WHERE")
	assert.Contains(t, s, "~*")
}
