package figo

import (
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	f.AddBanFields("sensitive_field", "internal_use_only")
	assert.True(t, f.GetBanFields()["sensitive_field"])
	assert.True(t, f.GetBanFields()["internal_use_only"])
}

func TestAddFiltersFromString(t *testing.T) {
	f := New()
	f.AddFiltersFromString("id:[eq:9,or,eq:10]|or|vendorId:[eq:22]|and|bank_id:[gt:11]|or|expedition_type:[eq:eq]")
	assert.NotEmpty(t, f.GetMainFilter().Children)
}

func TestBuild(t *testing.T) {
	f := New()
	f.AddFiltersFromString("id:[eq:9,or,eq:10]|or|vendorId:[eq:22]|and|bank_id:[gt:11]|or|expedition_type:[eq:eq]")
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
	f.AddFiltersFromString("id:[eq:7,or,eq:10]|or|vendorId:[eq:22]|and|bank_id:[gt:11]|or|expedition_type:[eq:eq]|load:[TestInner1:id:[eq:3,or,eq:4]|or|name:[eq:test122]]")
	f.Build()
	db = db.Debug()
	db = f.Apply(db.Model(&TestModel{}))

	var results []TestModel
	result := db.Find(&results)
	assert.Nil(t, result.Error)
	assert.NotEmpty(t, results)
}

func TestPagination(t *testing.T) {
	f := New()
	f.AddFiltersFromString("page:[skip=10&take=20]")
	assert.Equal(t, 10, f.GetPage().Skip)
	assert.Equal(t, 20, f.GetPage().Take)
}

func TestAddFilter(t *testing.T) {
	f := New()
	f.AddFilter(OperationEq, clause.Eq{
		Column: clause.Column{Name: "id"},
		Value:  9,
	})
	assert.NotEmpty(t, f.GetMainFilter().Children)
	assert.Equal(t, OperationEq, f.GetMainFilter().Children[0].Operation)
}

func TestGetPreloads(t *testing.T) {
	f := New()
	f.AddFiltersFromString("load:[relation1&relation2]")
	assert.Equal(t, 2, len(f.GetPreloads()))
	assert.Contains(t, f.GetPreloads(), "relation1")
	assert.Contains(t, f.GetPreloads(), "relation2")
}
