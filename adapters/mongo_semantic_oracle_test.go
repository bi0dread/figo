package adapters

// The Mongo SEMANTIC oracle. Hunt #8's completeness critique named this the
// biggest untested gap in the project: "No truth-table oracle has ever been run
// against Elasticsearch or Mongo ... The ES and Mongo work was document-shape
// differentials against the parent: it answers 'did the rendered JSON change'
// and cannot answer 'does this JSON mean what the SQL means.'"
//
// This file answers the second question. One DSL string is rendered twice — by
// the raw adapter into SQL that is EXECUTED against SQLite, and by the Mongo
// adapter into a filter/pipeline that is evaluated by the MongoDB-semantics
// evaluator in mongo_semantics_eval_test.go — and the two row sets are
// compared. Where they legitimately differ, the divergence is named and pinned
// with the reason, so a future change cannot quietly turn a documented
// divergence into an undocumented one (or vice versa).

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// the corpus fixture: identical rows in SQLite and as BSON documents
// ---------------------------------------------------------------------------

// oracleRows is the shared fixture. SQL NULL maps to BSON null, the standard
// correspondence: a document that OMITS the field is covered separately by
// TestMongoEvaluator_MatchesDocumentedMongoDBSemantics, because SQL has no way
// to express "missing".
var oracleRows = []struct {
	id   int
	a    any // int or nil
	b    any // int or nil
	name any // string or nil
}{
	{1, 1, 10, "alice"},
	{2, 2, 20, "bob"},
	{3, nil, 30, "carol"},
	{4, 2, nil, "dave"},
	{5, 5, 50, nil},
	{6, 0, 0, ""},
}

func oracleDocs() []bson.M {
	out := make([]bson.M, 0, len(oracleRows))
	for _, r := range oracleRows {
		out = append(out, bson.M{"id": r.id, "a": r.a, "b": r.b, "name": r.name})
	}
	return out
}

func oracleDB(t *testing.T) *sql.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	require.NoError(t, err)
	d, err := g.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`CREATE TABLE items (id INTEGER, a INTEGER, b INTEGER, name TEXT)`)
	require.NoError(t, err)
	for _, r := range oracleRows {
		_, err = d.Exec(`INSERT INTO items (id,a,b,name) VALUES (?,?,?,?)`, r.id, r.a, r.b, r.name)
		require.NoError(t, err)
	}
	// The child collection for the aggregate/JOIN corpus.
	_, err = d.Exec(`CREATE TABLE orders (id INTEGER, item_id INTEGER, total INTEGER, status TEXT)`)
	require.NoError(t, err)
	for _, o := range oracleOrders {
		_, err = d.Exec(`INSERT INTO orders (id,item_id,total,status) VALUES (?,?,?,?)`, o.id, o.itemID, o.total, o.status)
		require.NoError(t, err)
	}
	return d
}

var oracleOrders = []struct {
	id     int
	itemID int
	total  int
	status string
}{
	{10, 1, 100, "paid"},
	{11, 1, 50, "open"},
	{12, 2, 500, "open"},
	{13, 3, 10, "paid"},
	{14, 6, 100, "paid"},
}

func oracleOrderDocs() []bson.M {
	out := make([]bson.M, 0, len(oracleOrders))
	for _, o := range oracleOrders {
		out = append(out, bson.M{"id": o.id, "item_id": o.itemID, "total": o.total, "status": o.status})
	}
	return out
}

// sqlIDs renders the DSL with the raw adapter and EXECUTES it: the SQL side of
// the oracle is never a string comparison.
func sqlIDs(t *testing.T, d *sql.DB, dsl string) []int {
	t.Helper()
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(dsl), "dsl did not parse: %s", dsl)
	f.Build(RawAdapter{Dialect: SQLiteDialect})
	q, ok := RawAdapter{Dialect: SQLiteDialect}.GetQuery(f, RawContext{Table: "items"})
	require.True(t, ok, "raw GetQuery failed for %q", dsl)
	s := q.(figo.SQLQuery)
	rows, err := d.Query(s.SQL, s.Args...)
	require.NoError(t, err, "statement did not execute: %s", s.SQL)
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		var a, b sql.NullInt64
		var name sql.NullString
		require.NoError(t, rows.Scan(&id, &a, &b, &name))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// mongoIDs renders the DSL with the Mongo adapter and evaluates the resulting
// filter against the document fixture with MongoDB's matching rules.
func mongoIDs(t *testing.T, dsl string) ([]int, error) {
	t.Helper()
	f := figo.New()
	if err := f.AddFiltersFromString(dsl); err != nil {
		return nil, err
	}
	f.Build(MongoAdapter{})
	filter, err := BuildMongoFilter(f)
	if err != nil {
		return nil, err
	}
	return evalFilterIDs(filter, oracleDocs())
}

func evalFilterIDs(filter bson.M, docs []bson.M) ([]int, error) {
	e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
	ids := []int{}
	for _, d := range docs {
		m, err := e.matchFilter(filter, d)
		if err != nil {
			return nil, err
		}
		if m {
			ids = append(ids, d["id"].(int))
		}
	}
	sort.Ints(ids)
	return ids, nil
}

// ---------------------------------------------------------------------------
// step 1: validate the evaluator itself against documented MongoDB rules
// ---------------------------------------------------------------------------

// TestMongoEvaluator_MatchesDocumentedMongoDBSemantics pins the evaluator
// against MongoDB's documented behaviour. An oracle that has not been validated
// is only a second opinion; every expectation below cites the rule it encodes.
func TestMongoEvaluator_MatchesDocumentedMongoDBSemantics(t *testing.T) {
	e := &mongoEval{}
	docs := map[string]bson.M{
		"present":  {"a": 1},
		"null":     {"a": nil},
		"missing":  {"z": 1},
		"arr":      {"a": bson.A{1, 2, 3}},
		"arrnull":  {"a": bson.A{nil}},
		"arrempty": {"a": bson.A{}},
		"nested":   {"m": bson.M{"k": 5}},
		"nestarr":  {"m": bson.A{bson.M{"k": 5}, bson.M{"k": 9}}},
		"str":      {"a": "x"},
	}
	cases := []struct {
		rule   string
		filter bson.M
		want   map[string]bool
	}{
		{
			// "{field: null} matches documents that either contain the field
			// whose value is null or that do not contain the field."
			rule:   "equality to null matches null AND missing",
			filter: bson.M{"a": nil},
			want:   map[string]bool{"present": false, "null": true, "missing": true, "arrnull": true, "arrempty": false},
		},
		{
			rule:   "$ne null is the negation: only a real value matches",
			filter: bson.M{"a": bson.M{"$ne": nil}},
			want:   map[string]bool{"present": true, "null": false, "missing": false, "arrnull": false, "arrempty": true},
		},
		{
			// "$exists: true matches documents that contain the field,
			// including documents where the field value is null."
			rule:   "$exists distinguishes null from missing",
			filter: bson.M{"a": bson.M{"$exists": true}},
			want:   map[string]bool{"present": true, "null": true, "missing": false},
		},
		{
			// "If the field holds an array, then the query selects documents
			// whose field holds an array that contains at least one element
			// that matches."
			rule:   "a scalar predicate matches any ELEMENT of an array field",
			filter: bson.M{"a": 2},
			want:   map[string]bool{"arr": true, "present": false, "arrempty": false},
		},
		{
			rule:   "range comparison also applies element-wise",
			filter: bson.M{"a": bson.M{"$gt": 2}},
			want:   map[string]bool{"arr": true, "present": false, "null": false, "missing": false},
		},
		{
			// Comparison operators only match values in the operand's type
			// bracket; null is alone in its bracket, so $gt null matches nothing.
			rule:   "$gt does not match across type brackets, and never matches null/missing",
			filter: bson.M{"a": bson.M{"$gt": nil}},
			want:   map[string]bool{"present": false, "null": false, "missing": false, "str": false},
		},
		{
			rule:   "$gt on a number does not match a string value",
			filter: bson.M{"a": bson.M{"$gt": 0}},
			want:   map[string]bool{"present": true, "str": false},
		},
		{
			rule:   "$in matches element-wise and null in the list matches missing",
			filter: bson.M{"a": bson.M{"$in": []any{3, nil}}},
			want:   map[string]bool{"arr": true, "missing": true, "null": true, "present": false},
		},
		{
			rule:   "$nin is the negation of $in, so it matches null and missing",
			filter: bson.M{"a": bson.M{"$nin": []any{1}}},
			want:   map[string]bool{"present": false, "null": true, "missing": true, "arr": false},
		},
		{
			rule:   "$in with an empty list matches nothing; $nin with an empty list matches everything",
			filter: bson.M{"a": bson.M{"$in": []any{}}},
			want:   map[string]bool{"present": false, "null": false, "missing": false},
		},
		{
			rule:   "$all requires every listed value in the array",
			filter: bson.M{"a": bson.M{"$all": []any{1, 3}}},
			want:   map[string]bool{"arr": true, "present": false},
		},
		{
			rule:   "a dotted path descends into a sub-document",
			filter: bson.M{"m.k": 5},
			want:   map[string]bool{"nested": true, "present": false},
		},
		{
			rule:   "a dotted path traverses an array of sub-documents implicitly",
			filter: bson.M{"m.k": 9},
			want:   map[string]bool{"nestarr": true, "nested": false},
		},
		{
			// This is the shape mongoAggregatePipeline emits to keep INNER JOIN
			// parent semantics, so its meaning has to be exact.
			rule:   "path.0 $exists true means the array is non-empty",
			filter: bson.M{"a.0": bson.M{"$exists": true}},
			want:   map[string]bool{"arr": true, "arrempty": false, "arrnull": true, "present": false, "missing": false},
		},
		{
			// The library-wide match-NOTHING sentinel on Mongo. Its ES analogue
			// inverting to match-ALL was hunt #8's one critical finding (D1).
			rule:   "$nor:[{}] is the match-NOTHING sentinel",
			filter: bson.M{"$nor": []bson.M{{}}},
			want:   map[string]bool{"present": false, "null": false, "missing": false, "arr": false},
		},
		{
			rule:   "the empty document is match-EVERYTHING",
			filter: bson.M{},
			want:   map[string]bool{"present": true, "null": true, "missing": true, "arr": true},
		},
		{
			rule:   "$nor of the match-nothing sentinel is match-everything",
			filter: bson.M{"$nor": []bson.M{{"$nor": []bson.M{{}}}}},
			want:   map[string]bool{"present": true, "null": true, "missing": true},
		},
		{
			rule:   "$nor matches when the inner predicate is false OR the field is absent",
			filter: bson.M{"$nor": []bson.M{{"a": 1}}},
			want:   map[string]bool{"present": false, "null": true, "missing": true, "str": true},
		},
	}
	for _, c := range cases {
		for docName, want := range c.want {
			got, err := e.matchFilter(c.filter, docs[docName])
			require.NoError(t, err, "%s / %s", c.rule, docName)
			assert.Equal(t, want, got, "rule %q on doc %s (%v) with filter %v", c.rule, docName, docs[docName], c.filter)
		}
	}
}

// ---------------------------------------------------------------------------
// step 2: DSL corpus, SQLite ground truth vs Mongo semantics
// ---------------------------------------------------------------------------

// oracleCase is one corpus entry. wantMongo is derived BY HAND from MongoDB's
// documented rules (not copied from the evaluator), so a disagreement points at
// either the adapter or the evaluator rather than confirming both. `divergence`
// must be non-empty exactly when wantMongo differs from the executed SQL row
// set, and names the documented rule that permits the difference.
type oracleCase struct {
	dsl        string
	wantMongo  []int
	divergence string
}

func TestMongoSemanticOracle_RootFilters(t *testing.T) {
	d := oracleDB(t)

	// The four divergence classes README "Cross-backend semantics: NULL rows"
	// declares: SQL's three-valued logic excludes NULL rows from !=, <nin> and
	// not(...), while Mongo has no UNKNOWN and matches them.
	const divNeq = "README: Mongo $ne matches null/missing; SQL != excludes NULL rows (three-valued logic)"
	const divNin = "README: Mongo $nin matches null/missing; SQL NOT IN excludes NULL rows"
	const divNot = "README: Mongo $nor matches documents where the inner predicate is false OR the field is absent; SQL NOT excludes NULL rows"

	cases := []oracleCase{
		// --- positive comparisons: no UNKNOWN is involved, so both agree ---
		{dsl: `a=1`, wantMongo: []int{1}},
		{dsl: `a=2`, wantMongo: []int{2, 4}},
		{dsl: `a=0`, wantMongo: []int{6}},
		{dsl: `a>1`, wantMongo: []int{2, 4, 5}},
		{dsl: `a>=2`, wantMongo: []int{2, 4, 5}},
		{dsl: `a<2`, wantMongo: []int{1, 6}},
		{dsl: `a<=2`, wantMongo: []int{1, 2, 4, 6}},
		{dsl: `name="alice"`, wantMongo: []int{1}},
		{dsl: `name=""`, wantMongo: []int{6}},
		{dsl: `a<bet>(1..2)`, wantMongo: []int{1, 2, 4}},
		{dsl: `a<bet>(2..1)`, wantMongo: []int{}}, // reversed range: empty on both
		{dsl: `a<in>[1,2]`, wantMongo: []int{1, 2, 4}},
		{dsl: `a<in>[]`, wantMongo: []int{}},   // the empty-<in> sentinel: match nothing
		{dsl: `a<nin>[]`, wantMongo: allIDs()}, // and its complement: match everything
		{dsl: `name=^"a%"`, wantMongo: []int{1}},

		// --- explicit null tests are two-valued on BOTH backends ---
		{dsl: `a<null>`, wantMongo: []int{3}},
		{dsl: `a<notnull>`, wantMongo: []int{1, 2, 4, 5, 6}},
		{dsl: `b<null>`, wantMongo: []int{4}},
		{dsl: `name<null>`, wantMongo: []int{5}},
		{dsl: `not (a<null>)`, wantMongo: []int{1, 2, 4, 5, 6}},

		// --- boolean composition of positive leaves: both agree ---
		{dsl: `a=1 or b>25`, wantMongo: []int{1, 3, 5}},
		{dsl: `a=1 and b>5`, wantMongo: []int{1}},
		{dsl: `a>0 and a<5`, wantMongo: []int{1, 2, 4}},
		{dsl: `a<in>[1,2] or b<null>`, wantMongo: []int{1, 2, 4}},
		{dsl: `a=1 or a=5 or a=0`, wantMongo: []int{1, 5, 6}},
		{dsl: `a=1 and b=10 and name="alice"`, wantMongo: []int{1}},

		// --- the four declared divergences ---
		// a != 2: rows 1,5,6 have a<>2; row 3 has a=null, which SQL drops and
		// Mongo keeps.
		{dsl: `a!=2`, wantMongo: []int{1, 3, 5, 6}, divergence: divNeq},
		// NOT IN (1,2): rows 5,6 qualify in SQL; Mongo adds the null row 3.
		{dsl: `a<nin>[1,2]`, wantMongo: []int{3, 5, 6}, divergence: divNin},
		// $nor:[{a:1}] keeps the a=null row 3; SQL NOT (a=1) drops it.
		{dsl: `not (a=1)`, wantMongo: []int{2, 3, 4, 5, 6}, divergence: divNot},
		// $nor over an OR: row 3 has b=30>25 so the inner OR is true and it is
		// excluded on BOTH; row 4 has b=null, so on Mongo the inner OR is FALSE
		// (b>25 cannot match null) and row 4 is kept, while SQL's (FALSE OR
		// UNKNOWN) = UNKNOWN excludes it.
		{dsl: `not (a=1 or b>25)`, wantMongo: []int{2, 4, 6}, divergence: divNot},
		// $nor over an AND: only row 1 satisfies the inner AND on Mongo, so
		// every other document survives (including the a=null row 3).
		{dsl: `not (a=1 and b>5)`, wantMongo: []int{2, 3, 4, 5, 6}, divergence: divNot},
		// != combined with a positive leaf: Mongo keeps row 3 (a null, b=30>15).
		{dsl: `a!=2 and b>15`, wantMongo: []int{3, 5}, divergence: divNeq},
		// README: "a null in the list matches null-or-missing documents on
		// Mongo; on SQL, IN (.., NULL) can never match a NULL row".
		{dsl: `a<in>[1,null]`, wantMongo: []int{1, 3},
			divergence: "README: a null inside <in> matches null/missing on Mongo; SQL IN (..,NULL) never matches a NULL row"},

		// --- depth > 1: the critique showed the one confirmed-critical ES
		// finding had never been probed past depth 1, so the nesting cases are
		// pinned here rather than assumed. ---
		// inner = a=1 OR (b>25 AND a!=2). On Mongo row 3 (a null, b=30) makes
		// the inner AND true because $ne matches null, so $nor excludes it —
		// which happens to coincide with SQL's UNKNOWN exclusion. Two different
		// logics, same answer: that coincidence is exactly what a shape
		// differential cannot see, so it is asserted rather than assumed.
		{dsl: `not (a=1 or (b>25 and a!=2))`, wantMongo: []int{2, 4, 6}},
		{dsl: `not (not (a=1))`, wantMongo: []int{1}},
		{dsl: `(a=1 or a=2) and (b>15 or b<null>)`, wantMongo: []int{2, 4}},
		{dsl: `(a=1 and b=10) or (a=5 and b=50)`, wantMongo: []int{1, 5}},
		{dsl: `not (a<in>[1,2])`, wantMongo: []int{3, 5, 6}, divergence: divNot},
	}

	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			gotSQL := sqlIDs(t, d, c.dsl)
			gotMongo, err := mongoIDs(t, c.dsl)
			require.NoError(t, err, "mongo render/eval failed for %q", c.dsl)

			// 1. The adapter must mean what MongoDB's rules say it means.
			assert.Equal(t, c.wantMongo, gotMongo, "Mongo row set for %q", c.dsl)

			// 2. Agreement with the executed SQL is the default; a difference is
			//    only acceptable when it is the declared one.
			if equalIDs(c.wantMongo, gotSQL) {
				assert.Empty(t, c.divergence, "%q agrees with SQL but is marked as a divergence", c.dsl)
				return
			}
			require.NotEmpty(t, c.divergence,
				"UNDOCUMENTED DIVERGENCE for %q: SQL(sqlite)=%v Mongo=%v", c.dsl, gotSQL, gotMongo)
			// A documented NULL divergence may only ADD rows SQL dropped for
			// UNKNOWN — never drop a row SQL returned. A Mongo result that is
			// missing a row SQL returned is a narrowing, which no documented
			// rule permits.
			for _, id := range gotSQL {
				assert.Contains(t, c.wantMongo, id,
					"%q: Mongo LOST row %d that SQL returned — no documented divergence permits narrowing", c.dsl, id)
			}
		})
	}
}

func allIDs() []int {
	out := make([]int, 0, len(oracleRows))
	for _, r := range oracleRows {
		out = append(out, r.id)
	}
	return out
}

func equalIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// step 3: the sentinel family (the D1 class) rendered by BOTH adapters
// ---------------------------------------------------------------------------

// TestMongoSemanticOracle_Sentinels executes the match-nothing / match-all
// sentinels through the Mongo evaluator and through SQLite, including the
// NESTED forms. Hunt #8's critique showed the Elasticsearch sentinel inversion
// (D1) was only ever probed at depth 1; these go deeper, and they are
// programmatic because OrExpr{} is a sentinel the parser produces indirectly.
func TestMongoSemanticOracle_Sentinels(t *testing.T) {
	d := oracleDB(t)
	all := allIDs()
	none := []int{}

	cases := []struct {
		name string
		expr figo.Expr
		want []int
	}{
		{"OrExpr{} is match-nothing", figo.OrExpr{}, none},
		{"AndExpr{} is match-everything", figo.AndExpr{}, all},
		{"not(OrExpr{}) is match-everything", figo.NotExpr{Operands: []figo.Expr{figo.OrExpr{}}}, all},
		{"not(AndExpr{}) is match-nothing", figo.NotExpr{Operands: []figo.Expr{figo.AndExpr{}}}, none},
		{"and(OrExpr{}) is match-nothing", figo.AndExpr{Operands: []figo.Expr{figo.OrExpr{}}}, none},
		{"or(OrExpr{}, a=1) keeps the real operand",
			figo.OrExpr{Operands: []figo.Expr{figo.OrExpr{}, figo.EqExpr{Field: "a", Value: 1}}}, []int{1}},
		{"and(a=1, OrExpr{}) is match-nothing",
			figo.AndExpr{Operands: []figo.Expr{figo.EqExpr{Field: "a", Value: 1}, figo.OrExpr{}}}, none},
		{"not(not(OrExpr{})) is match-nothing",
			figo.NotExpr{Operands: []figo.Expr{figo.NotExpr{Operands: []figo.Expr{figo.OrExpr{}}}}}, none},
		{"or(a=1, and(OrExpr{}, b=10)) is just a=1",
			figo.OrExpr{Operands: []figo.Expr{
				figo.EqExpr{Field: "a", Value: 1},
				figo.AndExpr{Operands: []figo.Expr{figo.OrExpr{}, figo.EqExpr{Field: "b", Value: 10}}},
			}}, []int{1}},
		{"not(or(a=1, OrExpr{})) excludes only a=1 and keeps null rows",
			figo.NotExpr{Operands: []figo.Expr{figo.OrExpr{Operands: []figo.Expr{
				figo.EqExpr{Field: "a", Value: 1}, figo.OrExpr{},
			}}}}, []int{2, 3, 4, 5, 6}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm := figo.New()
			fm.AddFilter(c.expr)
			fm.Build(MongoAdapter{})
			filter, err := BuildMongoFilter(fm)
			require.NoError(t, err)
			gotMongo, err := evalFilterIDs(filter, oracleDocs())
			require.NoError(t, err)
			assert.Equal(t, c.want, gotMongo, "Mongo sentinel semantics for %s (filter %v)", c.name, filter)

			// And the same expression through SQL, executed. The two may differ
			// only by the documented NULL rules, which is why the last case
			// carries the null rows.
			fr := figo.New()
			fr.AddFilter(c.expr)
			fr.Build(RawAdapter{Dialect: SQLiteDialect})
			q, ok := RawAdapter{Dialect: SQLiteDialect}.GetQuery(fr, RawContext{Table: "items"})
			require.True(t, ok)
			s := q.(figo.SQLQuery)
			rows, err := d.Query(s.SQL, s.Args...)
			require.NoError(t, err, "SQL did not execute: %s", s.SQL)
			defer rows.Close()
			gotSQL := []int{}
			for rows.Next() {
				var id int
				var a, b sql.NullInt64
				var name sql.NullString
				require.NoError(t, rows.Scan(&id, &a, &b, &name))
				gotSQL = append(gotSQL, id)
			}
			// No sentinel may render NARROWER on Mongo than on SQL: that is the
			// direction in which a filter silently loses rows.
			for _, id := range gotSQL {
				assert.Contains(t, gotMongo, id,
					"%s: Mongo LOST row %d that SQL returned (SQL=%q)", c.name, id, s.SQL)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// step 4: the aggregate path — existential parent semantics vs a SQL JOIN
// ---------------------------------------------------------------------------

// TestMongoSemanticOracle_AggregateExistential checks the thing H12 rewrote:
// the $lookup sub-pipeline plus the {"<As>.0": {$exists:true}} guard must select
// exactly the parents a SQL "JOIN ... ON <preload filters>" selects, and the
// joined array must contain only matching children.
func TestMongoSemanticOracle_AggregateExistential(t *testing.T) {
	d := oracleDB(t)
	joins := map[string]MongoJoin{
		"orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "orders"},
	}
	cases := []struct {
		dsl string
		sql string
	}{
		{`load=[orders:total>60]`,
			`SELECT DISTINCT i.id FROM items i JOIN orders o ON o.item_id = i.id AND o.total > 60 ORDER BY i.id`},
		{`load=[orders:status="paid"]`,
			`SELECT DISTINCT i.id FROM items i JOIN orders o ON o.item_id = i.id AND o.status = 'paid' ORDER BY i.id`},
		{`load=[orders:status="paid" and total>60]`,
			`SELECT DISTINCT i.id FROM items i JOIN orders o ON o.item_id = i.id AND o.status = 'paid' AND o.total > 60 ORDER BY i.id`},
		{`a=1 load=[orders:total>60]`,
			`SELECT DISTINCT i.id FROM items i JOIN orders o ON o.item_id = i.id AND o.total > 60 WHERE i.a = 1 ORDER BY i.id`},
		{`load=[orders:status="none"]`,
			`SELECT DISTINCT i.id FROM items i JOIN orders o ON o.item_id = i.id AND o.status = 'none' ORDER BY i.id`},
	}
	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			f := figo.New()
			require.NoError(t, f.AddFiltersFromString(c.dsl))
			f.Build(MongoAdapter{})
			pipe, err := BuildMongoAggregatePipeline(f, joins)
			require.NoError(t, err)

			e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
			docs, err := e.runPipeline(pipe, oracleDocs())
			require.NoError(t, err, "pipeline is not executable: %v", pipe)
			gotIDs := []int{}
			for _, doc := range docs {
				gotIDs = append(gotIDs, doc["id"].(int))
			}
			sort.Ints(gotIDs)

			rows, err := d.Query(c.sql)
			require.NoError(t, err)
			defer rows.Close()
			wantIDs := []int{}
			for rows.Next() {
				var id int
				require.NoError(t, rows.Scan(&id))
				wantIDs = append(wantIDs, id)
			}
			assert.Equal(t, wantIDs, gotIDs, "parent set for %q", c.dsl)

			// H12's second half: the relation array must hold ONLY matching
			// children, never every child of the parent.
			for _, doc := range docs {
				arr, ok := arrEntries(doc["orders"])
				require.True(t, ok, "the lookup output must be an array")
				require.NotEmpty(t, arr, "a surviving parent must carry at least one matching child")
				for _, child := range arr {
					cm := child.(bson.M)
					require.Equal(t, doc["id"], cm["item_id"], "a foreign child leaked into the array")
				}
			}
		})
	}
}

// TestMongoSemanticOracle_PreloadParentSetDivergesFromTheOtherAdapters pins the
// one place the four adapters disagree about what a DSL means, found by running
// this oracle: the Mongo aggregate path applies INNER-JOIN parent semantics,
// while raw and GORM leave the parent set alone and Elasticsearch refuses the
// build.
//
// Hunt #6's H9/H10 established the raw contract — "a preload must neither
// multiply nor annihilate the primary rows" — and removed the JOIN entirely
// (raw emits no JOIN segment at all for load=). GORM's Preload is a second
// query, so the parent set is likewise untouched. The Mongo aggregate path
// filters parents. `load=[orders:status="nonexistent"]` therefore returns EVERY
// parent on raw/GORM and ZERO documents on Mongo, from one DSL string.
//
// This is not a regression (6bb998c's parent-level $match on the produced array
// annihilated the same parents) and which contract is correct is a maintainer's
// policy call, so it is pinned rather than changed. If the policy is decided,
// change this test deliberately — do not let it drift.
func TestMongoSemanticOracle_PreloadParentSetDivergesFromTheOtherAdapters(t *testing.T) {
	d := oracleDB(t)
	joins := map[string]MongoJoin{
		"orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "orders"},
	}
	const dsl = `load=[orders:status="nonexistent"]`

	// raw: no JOIN, every parent survives.
	fr := figo.New()
	require.NoError(t, fr.AddFiltersFromString(dsl))
	fr.Build(RawAdapter{Dialect: SQLiteDialect})
	q, ok := RawAdapter{Dialect: SQLiteDialect}.GetQuery(fr, RawContext{Table: "items"})
	require.True(t, ok)
	s := q.(figo.SQLQuery)
	require.NotContains(t, s.SQL, "JOIN", "hunt #6 H9/H10 removed the preload JOIN")
	assert.Equal(t, allIDs(), sqlIDs(t, d, dsl), "raw keeps every parent")

	// mongo: the existential guard drops every parent.
	fm := figo.New()
	require.NoError(t, fm.AddFiltersFromString(dsl))
	fm.Build(MongoAdapter{})
	pipe, err := BuildMongoAggregatePipeline(fm, joins)
	require.NoError(t, err)
	e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
	docs, err := e.runPipeline(pipe, oracleDocs())
	require.NoError(t, err)
	assert.Empty(t, docs,
		"PINNED DIVERGENCE: the Mongo aggregate path applies INNER-JOIN parent semantics while raw/GORM keep every parent")

	// And the Find path refuses the same query outright (M7), so the three
	// Mongo entry points give three different answers to one DSL string: 6
	// parents (raw/GORM), 0 documents (aggregate), a build error (Find).
	_, err = BuildMongoFilter(fm)
	require.Error(t, err, "M7: the Find path fails closed on preload predicates")
}

// ---------------------------------------------------------------------------
// step 5: LIKE/ILIKE — a regex standing in for a SQL pattern
// ---------------------------------------------------------------------------

var likeRows = []struct {
	id   int
	name any
}{
	{1, "alice"},
	{2, "ALICE"},
	{3, "a.c"},
	{4, "a\nb"}, // the L9 DOTALL case: SQL '%' spans newlines, PCRE '.' does not
	{5, "a\nc"},
	{6, "a%b"},
	{7, "abc"},
	{8, ""},
	{9, nil},
}

// TestMongoSemanticOracle_LikePatterns compares the LIKE-to-regex translation
// against SQL LIKE, EXECUTED. SQLite's LIKE is case-INSENSITIVE for ASCII by
// default, which is a SQLite quirk rather than SQL's rule, so the pragma is
// turned on to make the comparison meaningful.
func TestMongoSemanticOracle_LikePatterns(t *testing.T) {
	g, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	require.NoError(t, err)
	d, err := g.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`PRAGMA case_sensitive_like = ON`)
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE items (id INTEGER, name TEXT)`)
	require.NoError(t, err)
	docs := make([]bson.M, 0, len(likeRows))
	for _, r := range likeRows {
		_, err = d.Exec(`INSERT INTO items (id,name) VALUES (?,?)`, r.id, r.name)
		require.NoError(t, err)
		docs = append(docs, bson.M{"id": r.id, "name": r.name})
	}

	cases := []oracleCase{
		{dsl: `name=^"a%"`, wantMongo: []int{1, 3, 4, 5, 6, 7}},
		{dsl: `name=^"a_c"`, wantMongo: []int{3, 5, 7}}, // '_' spans a newline, as SQL's does
		{dsl: `name=^"%.c"`, wantMongo: []int{3}},       // '.' is a literal in LIKE, so it must be escaped
		{dsl: `name=^"a%b"`, wantMongo: []int{4, 6}},
		{dsl: `name=^"ALICE"`, wantMongo: []int{2}},
		{dsl: `name=^"abc"`, wantMongo: []int{7}},
		{dsl: `name=^""`, wantMongo: []int{8}},
		{dsl: `name.=^"alice"`, wantMongo: []int{1, 2}},
		{dsl: `name.=^"A%"`, wantMongo: []int{1, 2, 3, 4, 5, 6, 7}},
		{dsl: `name!=^"a%"`, wantMongo: []int{2, 8, 9},
			divergence: "README: Mongo $nor matches documents where the field is null; SQL NOT LIKE excludes NULL rows"},
	}
	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			fr := figo.New()
			require.NoError(t, fr.AddFiltersFromString(c.dsl))
			fr.Build(RawAdapter{Dialect: SQLiteDialect})
			q, ok := RawAdapter{Dialect: SQLiteDialect}.GetQuery(fr, RawContext{Table: "items"})
			require.True(t, ok)
			s := q.(figo.SQLQuery)
			rows, err := d.Query(s.SQL, s.Args...)
			require.NoError(t, err, "statement did not execute: %s", s.SQL)
			gotSQL := []int{}
			for rows.Next() {
				var id int
				var name sql.NullString
				require.NoError(t, rows.Scan(&id, &name))
				gotSQL = append(gotSQL, id)
			}
			require.NoError(t, rows.Err())
			rows.Close()

			fm := figo.New()
			require.NoError(t, fm.AddFiltersFromString(c.dsl))
			fm.Build(MongoAdapter{})
			filter, err := BuildMongoFilter(fm)
			require.NoError(t, err)
			gotMongo, err := evalFilterIDs(filter, docs)
			require.NoError(t, err)

			assert.Equal(t, c.wantMongo, gotMongo, "Mongo row set for %q (filter %v)", c.dsl, filter)
			if equalIDs(c.wantMongo, gotSQL) {
				assert.Empty(t, c.divergence, "%q agrees with SQL but is marked as a divergence", c.dsl)
				return
			}
			require.NotEmpty(t, c.divergence,
				"UNDOCUMENTED DIVERGENCE for %q: SQL(sqlite)=%v Mongo=%v", c.dsl, gotSQL, gotMongo)
			for _, id := range gotSQL {
				assert.Contains(t, c.wantMongo, id,
					"%q: Mongo LOST row %d that SQL returned", c.dsl, id)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// step 6: sort= / page= must mean the same thing on both backends
// ---------------------------------------------------------------------------

// TestMongoSemanticOracle_SortAndPage compares the ORDER of the rows, not just
// the set: an aggregate that sorts before skipping is a different page from one
// that skips before sorting, and figo emits $sort/$skip/$limit as separate
// stages whose order is the whole semantics.
func TestMongoSemanticOracle_SortAndPage(t *testing.T) {
	d := oracleDB(t)
	joins := map[string]MongoJoin{
		"orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "orders"},
	}
	cases := []struct {
		dsl string
		sql string
	}{
		// No page= at all: figo's default is take:0, i.e. no LIMIT, so the
		// oracle statement must not carry one either.
		{`sort=a:asc`, `SELECT id FROM items ORDER BY a ASC`},
		{`sort=a:desc`, `SELECT id FROM items ORDER BY a DESC`},
		{`sort=a:asc page=take:2`, `SELECT id FROM items ORDER BY a ASC LIMIT 2`},
		{`sort=a:asc page=take:2,skip:2`, `SELECT id FROM items ORDER BY a ASC LIMIT 2 OFFSET 2`},
		{`sort=b:desc page=take:3,skip:1`, `SELECT id FROM items ORDER BY b DESC LIMIT 3 OFFSET 1`},
		{`a>=0 sort=a:asc page=take:2`, `SELECT id FROM items WHERE a >= 0 ORDER BY a ASC LIMIT 2`},
	}
	for _, c := range cases {
		t.Run(c.dsl, func(t *testing.T) {
			rows, err := d.Query(c.sql)
			require.NoError(t, err)
			defer rows.Close()
			want := []int{}
			for rows.Next() {
				var id int
				require.NoError(t, rows.Scan(&id))
				want = append(want, id)
			}

			f := figo.New()
			require.NoError(t, f.AddFiltersFromString(c.dsl))
			f.Build(MongoAdapter{})
			pipe, err := BuildMongoAggregatePipeline(f, joins)
			require.NoError(t, err)
			e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
			docs, err := e.runPipeline(pipe, oracleDocs())
			require.NoError(t, err)
			got := []int{}
			for _, doc := range docs {
				got = append(got, doc["id"].(int))
			}
			// ORDER is asserted, not just membership. SQLite sorts NULL first
			// ascending and BSON's canonical order puts null below every number,
			// so the two agree on the null row's position as well.
			assert.Equal(t, want, got, "ordered page for %q (pipeline %v)", c.dsl, pipe)
		})
	}
}

// TestMongoSemanticOracle_FindAndAggregateAgree checks the two Mongo entry
// points against each other on the SAME instance. For a preload-free query they
// are two renderings of one AST and must select the same documents in the same
// order: the Find path applies filter + sort + skip + limit through FindOptions,
// the aggregate path through $match/$sort/$skip/$limit stages, and nothing
// forces the two to stay in step.
func TestMongoSemanticOracle_FindAndAggregateAgree(t *testing.T) {
	for _, dsl := range []string{
		`a>=0`,
		`a>=0 sort=a:asc`,
		`a>=0 sort=a:desc page=take:2`,
		`a>=0 sort=a:asc page=take:2,skip:1`,
		`sort=b:desc page=take:0`, // take:0 means "no limit" on both paths
		`a!=2 sort=a:asc`,
		`not (a=1) sort=a:asc page=take:3`,
	} {
		t.Run(dsl, func(t *testing.T) {
			f := figo.New()
			require.NoError(t, f.AddFiltersFromString(dsl))
			f.Build(MongoAdapter{})

			// Find path: filter, then sort/skip/limit from the options.
			filter, err := BuildMongoFilter(f)
			require.NoError(t, err)
			opts := BuildMongoFindOptions(f)
			e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
			matched, err := e.stageMatch(oracleDocs(), filter)
			require.NoError(t, err)
			if opts.Sort != nil {
				matched, err = e.stageSort(matched, opts.Sort)
				require.NoError(t, err)
			}
			if opts.Skip != nil {
				matched, err = e.stageSkipLimit(matched, *opts.Skip, true)
				require.NoError(t, err)
			}
			if opts.Limit != nil {
				matched, err = e.stageSkipLimit(matched, *opts.Limit, false)
				require.NoError(t, err)
			}
			findIDs := []int{}
			for _, doc := range matched {
				findIDs = append(findIDs, doc["id"].(int))
			}

			// Aggregate path on the same instance, no joins.
			pipe, err := BuildMongoAggregatePipeline(f, nil)
			require.NoError(t, err)
			aggDocs, err := e.runPipeline(pipe, oracleDocs())
			require.NoError(t, err)
			aggIDs := []int{}
			for _, doc := range aggDocs {
				aggIDs = append(aggIDs, doc["id"].(int))
			}

			assert.Equal(t, findIDs, aggIDs, "Find and aggregate disagree for %q (filter %v, pipeline %v)", dsl, filter, pipe)
		})
	}
}

// TestMongoSemanticOracle_ObjectIDCoercion pins the ObjectIDFields conversion,
// which has no SQL analogue and therefore no cross-adapter oracle: a hex string
// on a configured field must be compared as an ObjectID (otherwise the common
// `_id="507f…"` lookup silently matches nothing), and a non-hex value must stay
// a string rather than erroring.
func TestMongoSemanticOracle_ObjectIDCoercion(t *testing.T) {
	oid, err := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	require.NoError(t, err)
	docs := []bson.M{
		{"id": 1, "_id": oid},
		{"id": 2, "_id": "507f1f77bcf86cd799439011"}, // stored as a string
	}
	e := &mongoEval{}

	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`_id="507f1f77bcf86cd799439011"`))
	f.Build(MongoAdapter{})
	filter, err := BuildMongoFilter(f)
	require.NoError(t, err)
	got := []int{}
	for _, d := range docs {
		m, err := e.matchFilter(filter, d)
		require.NoError(t, err)
		if m {
			got = append(got, d["id"].(int))
		}
	}
	assert.Equal(t, []int{1}, got, "the hex string must be compared as an ObjectID, not as a string")

	// A value that is not a valid ObjectID hex string passes through unchanged,
	// so it matches the string-typed document instead of failing the build.
	f2 := figo.New()
	require.NoError(t, f2.AddFiltersFromString(`_id="not-an-oid"`))
	f2.Build(MongoAdapter{})
	filter2, err := BuildMongoFilter(f2)
	require.NoError(t, err)
	assert.Equal(t, "not-an-oid", filter2["_id"])
}

// ---------------------------------------------------------------------------
// step 7: A5-1 — why the filed fix sketch is the wrong fix
// ---------------------------------------------------------------------------

// TestA5_1_AliasPlacementSketchWouldInvertTheAnswer executes the two pipelines
// A5-1 argues about. The finding proposes that exprsReferenceAlias also match a
// root field EQUAL to a $lookup alias, which would move `orders<null>` AFTER
// the lookups. A $lookup output is ALWAYS an array, so that placement makes the
// predicate constant — a different wrong answer, not a right one. This test is
// the evidence for the SKIP.
func TestA5_1_AliasPlacementSketchWouldInvertTheAnswer(t *testing.T) {
	joins := map[string]MongoJoin{
		"orders": {From: "orders", LocalField: "id", ForeignField: "item_id", As: "orders"},
	}
	f := figo.New()
	require.NoError(t, f.AddFiltersFromString(`orders<null> load=[orders:total>60]`))
	f.Build(MongoAdapter{})
	pipe, err := BuildMongoAggregatePipeline(f, joins)
	require.NoError(t, err)

	// As emitted today the root $match is stage 0, before the $lookup: it tests
	// the ROOT collection's own `orders` field, which is the only point at which
	// that value still exists. Our fixture has no root `orders` field, so
	// {orders: null} matches every document and the pipeline is bounded by the
	// existential guard the $lookup contributes.
	e := &mongoEval{collections: map[string][]bson.M{"orders": oracleOrderDocs()}}
	asIs, err := e.runPipeline(pipe, oracleDocs())
	require.NoError(t, err)
	require.NotEmpty(t, asIs, "the pre-lookup placement is still bounded by the {orders.0:{$exists:true}} guard")

	// Now the placement the fix sketch would produce: the same root $match moved
	// to the end. Re-order the stages by hand rather than patching the source.
	var moved []bson.D
	var rootMatch bson.D
	for i, stage := range pipe {
		if i == 0 {
			rootMatch = stage
			continue
		}
		moved = append(moved, stage)
	}
	moved = append(moved, rootMatch)
	after, err := e.runPipeline(moved, oracleDocs())
	require.NoError(t, err)
	assert.Empty(t, after,
		"a $lookup output is always an array, so a post-lookup {orders: null} matches NOTHING unconditionally — the sketch trades one wrong answer for another")

	// The mirror case confirms the inversion is systematic, not incidental.
	f2 := figo.New()
	require.NoError(t, f2.AddFiltersFromString(`orders<notnull> load=[orders:total>60]`))
	f2.Build(MongoAdapter{})
	pipe2, err := BuildMongoAggregatePipeline(f2, joins)
	require.NoError(t, err)
	var moved2 []bson.D
	var rootMatch2 bson.D
	for i, stage := range pipe2 {
		if i == 0 {
			rootMatch2 = stage
			continue
		}
		moved2 = append(moved2, stage)
	}
	moved2 = append(moved2, rootMatch2)
	after2, err := e.runPipeline(moved2, oracleDocs())
	require.NoError(t, err)
	assert.NotEmpty(t, after2,
		"and post-lookup {orders: {$ne: null}} matches every surviving parent unconditionally")
	fmt.Printf("A5-1: post-lookup orders<null> -> %d docs, orders<notnull> -> %d docs\n", len(after), len(after2))
}
