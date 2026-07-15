package adapters

import (
	. "github.com/bi0dread/figo/v4"

	"testing"

	"github.com/stretchr/testify/assert"
)

// A representative expression type neither the Mongo nor ES adapter implements.
func unsupportedExpr() Expr {
	return GeoDistanceExpr{Field: "loc", Latitude: 1, Longitude: 2, Distance: 5, Unit: "km"}
}

// #4: an unsupported/advanced expression must fail loudly, never be silently
// dropped (which previously returned too many rows / the whole index).
func TestMongoErrorsOnUnsupportedExpr(t *testing.T) {
	t.Run("Standalone", func(t *testing.T) {
		f := New()
		f.AddFilter(unsupportedExpr())
		f.Build(nil)
		_, err := BuildMongoFilter(f)
		assert.Error(t, err, "unsupported expr must produce an error, not a dropped condition")
	})

	t.Run("NestedInAnd", func(t *testing.T) {
		f := New()
		f.AddFilter(AndExpr{Operands: []Expr{
			EqExpr{Field: "active", Value: true},
			unsupportedExpr(),
		}})
		f.Build(nil)
		_, err := BuildMongoFilter(f)
		assert.Error(t, err, "error must propagate out of nested operators")
	})

	t.Run("AdapterGetQueryFails", func(t *testing.T) {
		f := New()
		f.AddFilter(unsupportedExpr())
		f.Build(MongoAdapter{})
		_, ok := MongoAdapter{}.GetQuery(f, nil)
		assert.False(t, ok, "adapter must report failure rather than a partial query")
	})

	t.Run("SupportedStillWorks", func(t *testing.T) {
		f := New()
		f.AddFilter(EqExpr{Field: "id", Value: 1})
		f.Build(nil)
		m, err := BuildMongoFilter(f)
		assert.NoError(t, err)
		assert.Equal(t, 1, m["id"])
	})
}

func TestElasticsearchErrorsOnUnsupportedExpr(t *testing.T) {
	t.Run("BuildReturnsError", func(t *testing.T) {
		f := New()
		f.AddFilter(unsupportedExpr())
		f.Build(nil)
		_, err := BuildElasticsearchQuery(f)
		assert.Error(t, err)
	})

	t.Run("QueryStringReturnsError", func(t *testing.T) {
		f := New()
		f.AddFilter(unsupportedExpr())
		f.Build(nil)
		_, err := GetElasticsearchQueryString(f)
		assert.Error(t, err)
	})

	t.Run("NestedInOr", func(t *testing.T) {
		f := New()
		f.AddFilter(OrExpr{Operands: []Expr{
			EqExpr{Field: "a", Value: 1},
			unsupportedExpr(),
		}})
		f.Build(nil)
		_, err := BuildElasticsearchQuery(f)
		assert.Error(t, err, "error must propagate; must not degrade to match_all")
	})

	t.Run("BuilderToJSONSurfacesError", func(t *testing.T) {
		f := New()
		f.AddFilter(unsupportedExpr())
		f.Build(nil)
		_, err := NewElasticsearchQueryBuilder().FromFigo(f).ToJSON()
		assert.Error(t, err)
	})

	t.Run("SupportedStillWorks", func(t *testing.T) {
		f := New()
		f.AddFilter(EqExpr{Field: "id", Value: 1})
		f.Build(nil)
		_, err := BuildElasticsearchQuery(f)
		assert.NoError(t, err)
	})
}
