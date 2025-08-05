package figo

import (
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
)

func TestNew(t *testing.T) {
	f := New()
	assert.NotNil(t, f)
	assert.Equal(t, 0, f.GetPage().Skip)
	assert.Equal(t, 20, f.GetPage().Take)
}

func TestAddBanFields(t *testing.T) {
	f := New()
	f.AddIgnoreFields("sensitive_field", "internal_use_only")
	assert.True(t, f.GetIgnoreFields()["sensitive_field"])
	assert.True(t, f.GetIgnoreFields()["internal_use_only"])
}

func TestAddSelectFields(t *testing.T) {
	f := New()
	f.AddSelectFields("field1", "field2")
	assert.True(t, f.GetSelectFields()["field1"])
	assert.True(t, f.GetSelectFields()["field2"])
}

func TestBuild(t *testing.T) {
	f := New()

	f.AddFiltersFromString(`(id=1 or id=2) or id>=2 or id<=3 or id!=0 and vendor=vendor1 or name=ali and (place=tehran or place=shiraz or (v1=2 and v2=1 and (g1=0 or g1=2))) or GG=9 or GG=8 sort=id:desc,name:ace page=skip:10,take:10 load=[inner1:id=1 or name=ali | inner2:id=2 or name=ali]`)
	f.Build()
	assert.NotEmpty(t, f.GetClauses())
}

func TestApply(t *testing.T) {
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

	f := New()
	// "(id=1 and vendorId=22) and bank_id>11 or expedition_type=eq load=[TestInner1:id=3 or name=test1 | TestInner2:id=4] sort=id:desc page=skip:0,take:10"
	f.AddFiltersFromString(`(id=1 and vendorId="22") and bank_id>11 or expedition_type="eq" load=[TestInner1:id="3" or name="test1"] sort=id:desc page=skip:0,take:10 `)

	f.Build()
	db = db.Debug()
	db = f.Apply(db.Model(&TestModel{}))

	fullSql := f.GetSqlString(db, "SELECT", "FROM", "WHERE", "JOIN", "ORDER BY", "GROUP BY", "LIMIT", "OFFSET")

	var results []TestModel
	result := db.Find(&results)
	assert.Nil(t, result.Error)
	assert.NotEmpty(t, results)
	expectedQuery := "SELECT * FROM `test_models` WHERE (((`id` = \"1\" AND `vendor_id` = \"22\") AND `bank_id` > \"11\") OR `expedition_type` = \"eq\") ORDER BY `test_models`.`id` DESC LIMIT 10"
	assert.Equal(t, fullSql, expectedQuery)
}

func TestPageValidation(t *testing.T) {
	p := Page{Skip: -1, Take: 30}
	p.validate()
	assert.Equal(t, 0, p.Skip)
	assert.Equal(t, 20, p.Take)
}
