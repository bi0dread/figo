package plugins

import (
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"github.com/stretchr/testify/require"
)

func TestCacheKeysPreserveContainerNilness(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		nilValue, emptyValue any
	}{
		{"bytes", []byte(nil), []byte{}},
		{"slice", []string(nil), []string{}},
		{"dynamic slice", []any(nil), []any{}},
		{"map", map[string]int(nil), map[string]int{}},
		{"dynamic map", map[string]any(nil), map[string]any{}},
		{"nested", []any{map[string]any{"value": []byte(nil)}}, []any{map[string]any{"value": []byte{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := func(value any, context bool) string {
				f := figo.New()
				var ctx any = "users"
				if context {
					ctx = value
				} else {
					f.AddFilter(figo.EqExpr{Field: "payload", Value: value})
				}
				require.NoError(t, f.BuildE(adapters.RawAdapter{}))
				return generateCacheKey(f, "query", ctx)
			}
			for _, context := range []bool{false, true} {
				nilKey, emptyKey := key(tc.nilValue, context), key(tc.emptyValue, context)
				require.NotEmpty(t, nilKey)
				require.NotEmpty(t, emptyKey)
				require.NotEqual(t, nilKey, emptyKey, "context=%t", context)
				require.Equal(t, nilKey, key(tc.nilValue, context), "nil values remain cacheable")
				require.Equal(t, emptyKey, key(tc.emptyValue, context), "empty values remain cacheable")
			}
		})
	}
}

func TestCopyQueryOwnsNestedContainers(t *testing.T) {
	args := func(q figo.Query) []any {
		if p, ok := q.(*figo.SQLQuery); ok {
			return p.Args
		}
		return q.(figo.SQLQuery).Args
	}
	for _, pointer := range []bool{false, true} {
		original := figo.SQLQuery{Args: []any{
			map[string]any{"values": []any{[]byte("alice"), []byte(nil), []byte{}}},
		}}
		var input figo.Query = original
		if pointer {
			input = &original
		}
		copied := copyQuery(input)
		require.IsType(t, input, copied)
		values := args(copied)[0].(map[string]any)["values"].([]any)
		copy(values[0].([]byte), "admin")
		values[1] = []byte("changed")
		require.Equal(t, []any{[]byte("alice"), []byte(nil), []byte{}},
			original.Args[0].(map[string]any)["values"])
	}
	require.Nil(t, args(copyQuery(figo.SQLQuery{})))
	require.NotNil(t, args(copyQuery(figo.SQLQuery{Args: []any{}})))
}
