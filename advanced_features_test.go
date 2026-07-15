package figo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAdvancedOperators(t *testing.T) {
	t.Run("JsonPathExpr", func(t *testing.T) {
		expr := JsonPathExpr{
			Field: "metadata",
			Path:  "$.user.name",
			Value: "john",
			Op:    "=",
		}

		assert.Equal(t, "metadata", expr.Field)
		assert.Equal(t, "$.user.name", expr.Path)
		assert.Equal(t, "john", expr.Value)
		assert.Equal(t, "=", expr.Op)
	})

	t.Run("ArrayContainsExpr", func(t *testing.T) {
		expr := ArrayContainsExpr{
			Field:  "tags",
			Values: []any{"tech", "golang", "database"},
		}

		assert.Equal(t, "tags", expr.Field)
		assert.Len(t, expr.Values, 3)
		assert.Contains(t, expr.Values, "tech")
	})

	t.Run("ArrayOverlapsExpr", func(t *testing.T) {
		expr := ArrayOverlapsExpr{
			Field:  "categories",
			Values: []any{"business", "finance"},
		}

		assert.Equal(t, "categories", expr.Field)
		assert.Len(t, expr.Values, 2)
	})

	t.Run("FullTextSearchExpr", func(t *testing.T) {
		expr := FullTextSearchExpr{
			Field:    "content",
			Query:    "machine learning algorithms",
			Language: "en",
		}

		assert.Equal(t, "content", expr.Field)
		assert.Equal(t, "machine learning algorithms", expr.Query)
		assert.Equal(t, "en", expr.Language)
	})

	t.Run("GeoDistanceExpr", func(t *testing.T) {
		expr := GeoDistanceExpr{
			Field:     "location",
			Latitude:  40.7128,
			Longitude: -74.0060,
			Distance:  10.0,
			Unit:      "km",
		}

		assert.Equal(t, "location", expr.Field)
		assert.Equal(t, 40.7128, expr.Latitude)
		assert.Equal(t, -74.0060, expr.Longitude)
		assert.Equal(t, 10.0, expr.Distance)
		assert.Equal(t, "km", expr.Unit)
	})

	t.Run("CustomExpr", func(t *testing.T) {
		handler := func(field, operator string, value any) (string, []any, error) {
			return "custom_query", []any{value}, nil
		}

		expr := CustomExpr{
			Field:    "custom_field",
			Operator: "custom_op",
			Value:    "custom_value",
			Handler:  handler,
		}

		assert.Equal(t, "custom_field", expr.Field)
		assert.Equal(t, "custom_op", expr.Operator)
		assert.Equal(t, "custom_value", expr.Value)
		assert.NotNil(t, expr.Handler)
	})
}
