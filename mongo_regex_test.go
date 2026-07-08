package figo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// #5: LIKE/ILIKE/regex must serialize as a real BSON regex, not "$regex": {}.
func TestMongoRegexSerializesCorrectly(t *testing.T) {
	f := New()
	f.AddFilter(LikeExpr{Field: "name", Value: "%foo%"})
	f.Build()
	m, _ := BuildMongoFilter(f)

	re, ok := m["name"].(primitive.Regex)
	assert.True(t, ok, "LIKE must produce a primitive.Regex, not a bson.M{$regex:...}")
	assert.Equal(t, "^.*foo.*$", re.Pattern)

	// The whole point of #5: it must round-trip through the BSON encoder with the
	// pattern intact (a *regexp.Regexp serialized to an empty document).
	raw, err := bson.Marshal(m)
	assert.NoError(t, err)
	var back bson.M
	assert.NoError(t, bson.Unmarshal(raw, &back))
	got, ok := back["name"].(primitive.Regex)
	assert.True(t, ok)
	assert.Equal(t, "^.*foo.*$", got.Pattern, "pattern must survive BSON round-trip")
}

// #17: LIKE is anchored — exact match, not substring.
func TestMongoLikeIsAnchored(t *testing.T) {
	assert.Equal(t, "^abc$", likeToRegexPattern("abc"))    // no wildcards => exact
	assert.Equal(t, "^abc.*$", likeToRegexPattern("abc%")) // prefix
	assert.Equal(t, "^.*abc.*$", likeToRegexPattern("%abc%"))
}

// #18: SQL '_' single-char wildcard is translated (Mongo '.', ES '?').
func TestSingleCharWildcardTranslated(t *testing.T) {
	assert.Equal(t, "^a.c$", likeToRegexPattern("a_c"))
	assert.Equal(t, "a?c", sqlLikeToESWildcard("a_c"))
	assert.Equal(t, "a*c", sqlLikeToESWildcard("a%c"))
}

// Regex metacharacters in a LIKE value are escaped (no injection / accidental regex).
func TestMongoLikeEscapesMeta(t *testing.T) {
	assert.Equal(t, `^a\.b\+c$`, likeToRegexPattern("a.b+c"))
}

// ILIKE carries the case-insensitive option on the regex itself (not a separate
// $options key paired with a regex object, which Mongo rejects).
func TestMongoILikeUsesOptions(t *testing.T) {
	f := New()
	f.AddFilter(ILikeExpr{Field: "name", Value: "%bar%"})
	f.Build()
	m, _ := BuildMongoFilter(f)
	re, ok := m["name"].(primitive.Regex)
	assert.True(t, ok)
	assert.Equal(t, "i", re.Options)
}
