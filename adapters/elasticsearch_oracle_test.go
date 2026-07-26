package adapters

// Truth-table oracle for the Elasticsearch adapter.
//
// Every previous audit of this adapter compared the rendered JSON *document*
// against the document a previous commit rendered. That answers "did the shape
// change" and cannot answer "does this document mean what the SQL means" — which
// is why hunt #8's D1 (a fail-closed match_none sentinel inverting to match-ALL
// under bool.must_not) survived eight hunts with green tests.
//
// This file closes that gap. It evaluates the emitted Elasticsearch document
// against an in-memory corpus using Lucene's boolean semantics, executes the raw
// adapter's SQL for the SAME DSL against SQLite, and asserts the two row sets are
// identical. SQL is the ground truth because it is the semantics figo documents.
//
// Deliberate scope limit: every corpus row has every field populated. The ES
// data model has no NULL — a SQL NULL is a MISSING field — and ES's must_not of
// a term query matches a document missing the field, whereas SQL's `NOT (a = 1)`
// is UNKNOWN for a NULL `a` and excludes the row. That divergence is real, is
// older than this adapter's current form, and is orthogonal to the three-valued
// logic this oracle is here to police: the UNKNOWN in every case below comes from
// a `null` LITERAL IN THE QUERY, not from the data. Adding NULL data would make
// the oracle report that pre-existing divergence on every negated predicate and
// drown the signal.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------------------------------------------------------------------------
// Lucene boolean evaluator
// ---------------------------------------------------------------------------

// esEvalClause reports whether one rendered Elasticsearch clause matches doc.
//
// The two Lucene rules that matter for the sentinel semantics, and that no
// shape-diff can express:
//
//  1. A bool query that produces NO clause at all is answered with
//     MatchAllDocsQuery — an empty bool is match_all, not match_none.
//  2. bool.must_not is a pure exclusion: it adds no positive clause, so
//     must_not of a clause that matches ZERO documents excludes nothing and
//     therefore matches EVERY document. FALSE and UNKNOWN are indistinguishable
//     in the document but behave oppositely here.
//
// A missing field never matches term/terms/range (Lucene has no NULL); that is
// the ES analogue of SQL's UNKNOWN for those operators.
func esEvalClause(clause map[string]any, doc map[string]any) (bool, error) {
	if len(clause) != 1 {
		return false, fmt.Errorf("clause must have exactly one key, got %d: %v", len(clause), clause)
	}
	for k, v := range clause {
		switch k {
		case "match_all":
			return true, nil
		case "match_none":
			return false, nil
		case "term":
			field, want, err := esOneField(v)
			if err != nil {
				return false, err
			}
			got, ok := doc[field]
			return ok && esValuesEqual(got, want), nil
		case "terms":
			field, want, err := esOneField(v)
			if err != nil {
				return false, err
			}
			got, ok := doc[field]
			if !ok {
				return false, nil
			}
			list, ok := want.([]any)
			if !ok {
				return false, fmt.Errorf(`"terms" value for %q is %T, not a list`, field, want)
			}
			for _, w := range list {
				if esValuesEqual(got, w) {
					return true, nil
				}
			}
			return false, nil
		case "exists":
			spec, ok := v.(map[string]any)
			if !ok {
				return false, fmt.Errorf(`"exists" body is %T`, v)
			}
			field, ok := spec["field"].(string)
			if !ok {
				return false, fmt.Errorf(`"exists" has no string "field": %v`, spec)
			}
			_, present := doc[field]
			return present, nil
		case "range":
			field, body, err := esOneField(v)
			if err != nil {
				return false, err
			}
			bounds, ok := body.(map[string]any)
			if !ok {
				return false, fmt.Errorf(`"range" body for %q is %T`, field, body)
			}
			got, present := doc[field]
			if !present {
				return false, nil
			}
			gotN, err := esNumber(got)
			if err != nil {
				return false, err
			}
			for op, bound := range bounds {
				if bound == nil {
					// Lucene reads a null bound as UNBOUNDED. Modelled here on
					// purpose: it is precisely why a nil operand must not render
					// a range at all.
					continue
				}
				boundN, err := esNumber(bound)
				if err != nil {
					return false, err
				}
				switch op {
				case "gt":
					if !(gotN > boundN) {
						return false, nil
					}
				case "gte":
					if !(gotN >= boundN) {
						return false, nil
					}
				case "lt":
					if !(gotN < boundN) {
						return false, nil
					}
				case "lte":
					if !(gotN <= boundN) {
						return false, nil
					}
				default:
					return false, fmt.Errorf("unsupported range op %q", op)
				}
			}
			return true, nil
		case "bool":
			body, ok := v.(map[string]any)
			if !ok {
				return false, fmt.Errorf(`"bool" body is %T`, v)
			}
			return esEvalBool(body, doc)
		default:
			// Anything the corpus can reach must be modelled explicitly; an
			// unmodelled clause is a corpus bug, not a silent pass.
			return false, fmt.Errorf("unsupported clause %q — extend the oracle or narrow the corpus", k)
		}
	}
	return false, fmt.Errorf("unreachable")
}

func esEvalBool(body map[string]any, doc map[string]any) (bool, error) {
	must, err := esClauseList(body["must"])
	if err != nil {
		return false, err
	}
	filter, err := esClauseList(body["filter"])
	if err != nil {
		return false, err
	}
	should, err := esClauseList(body["should"])
	if err != nil {
		return false, err
	}
	mustNot, err := esClauseList(body["must_not"])
	if err != nil {
		return false, err
	}

	if len(must) == 0 && len(filter) == 0 && len(should) == 0 && len(mustNot) == 0 {
		// Rule 1: a bool with no clauses is MatchAllDocsQuery.
		return true, nil
	}

	for _, c := range append(append([]map[string]any{}, must...), filter...) {
		ok, err := esEvalClause(c, doc)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for _, c := range mustNot {
		// Rule 2: exclusion only. A never-matching clause here excludes nothing.
		ok, err := esEvalClause(c, doc)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	if len(should) > 0 {
		msm := 1
		if len(must) > 0 || len(filter) > 0 {
			// Lucene: with a positive must/filter clause present, should
			// clauses are optional (minimum_should_match defaults to 0).
			msm = 0
		}
		if raw, present := body["minimum_should_match"]; present {
			n, err := esNumber(raw)
			if err != nil {
				return false, err
			}
			msm = int(n)
		}
		hits := 0
		for _, c := range should {
			ok, err := esEvalClause(c, doc)
			if err != nil {
				return false, err
			}
			if ok {
				hits++
			}
		}
		if hits < msm {
			return false, nil
		}
	}
	return true, nil
}

// esClauseList normalises a bool occurrence slot. The adapter emits some slots
// as a bare object (NeqExpr/IsNullExpr/NotInExpr) and others as an array, and
// Elasticsearch accepts both.
func esClauseList(v any) ([]map[string]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return []map[string]any{t}, nil
	case []map[string]any:
		return t, nil
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("bool occurrence element is %T", e)
			}
			out = append(out, m)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("bool occurrence slot is %T", v)
	}
}

func esOneField(v any) (string, any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("field body is %T, not an object", v)
	}
	if len(m) != 1 {
		return "", nil, fmt.Errorf("field body has %d keys, want 1: %v", len(m), m)
	}
	for k, val := range m {
		return k, val, nil
	}
	return "", nil, fmt.Errorf("unreachable")
}

func esValuesEqual(got, want any) bool {
	if want == nil || got == nil {
		return false // Lucene cannot express a null term.
	}
	g, gErr := esNumber(got)
	w, wErr := esNumber(want)
	if gErr == nil && wErr == nil {
		return g == w
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func esNumber(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float32:
		return float64(n), nil
	case float64:
		return n, nil
	}
	return 0, fmt.Errorf("value %v (%T) is not numeric", v, v)
}

// ---------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------

// esOracleLeaves: half are ordinary predicates, half carry SQL UNKNOWN via a
// `null` literal in the query. The UNKNOWN half is what negation must not
// invert.
var esOracleLeaves = []string{
	// definite predicates
	"a=1", "a=2", "b>1", "c<3", "a>=2", "b<=1",
	"a<in>[1,3]", "c<nin>[1]", "a<bet>(1..2)", "a!=2",
	// FALSE (not UNKNOWN): figo renders `IN ()` as 1=0 / match_none, and its
	// negation legitimately matches everything.
	"a<in>[]",
	// nil canonicalised to IS NULL / IS NOT NULL rather than to UNKNOWN
	"a=null", "a!=null",
	// UNKNOWN for at least some rows
	"a>null", "b<=null", "a>=null", "c<in>[null]", "a<in>[1,null]",
	"a<in>[1,2,null]", "d<nin>[null]", "a<nin>[2,null]", "b<nin>[1,3,null]",
	"a<bet>(null..2)", "b<bet>(1..null)",
}

// esOracleNarrow keeps the 3-slot templates to a tractable size while retaining
// one leaf of every flavour (definite, FALSE, UNKNOWN-range, UNKNOWN-in,
// UNKNOWN-notin, UNKNOWN-between).
var esOracleNarrow = []string{
	"a=1", "b>1", "a<in>[1,3]", "a<in>[]",
	"a>null", "c<in>[null]", "a<nin>[2,null]", "a<bet>(null..2)",
}

var esOracleTemplates1 = []string{
	"%[1]s",
	"not %[1]s",
	"not not %[1]s",
	"not (not %[1]s)",
	"not not not %[1]s",
	"not (not (not %[1]s))",
}

var esOracleTemplates2 = []string{
	"%[1]s and %[2]s",
	"%[1]s or %[2]s",
	"%[1]s %[2]s", // two top-level clauses: figo's implicit AND
	"not (%[1]s and %[2]s)",
	"not (%[1]s or %[2]s)",
	"not (%[1]s and not %[2]s)",
	"not (%[1]s or not %[2]s)",
	"not not (%[1]s or %[2]s)",
	"not not (%[1]s and %[2]s)",
	"not (not %[1]s or not %[2]s)",
	"not (not %[1]s and not %[2]s)",
}

var esOracleTemplates3 = []string{
	"(%[1]s and %[2]s) or %[3]s",
	"(%[1]s or %[2]s) and %[3]s",
	"not ((%[1]s or %[2]s) and %[3]s)",
	"not ((%[1]s and %[2]s) or %[3]s)",
	"%[1]s and not (%[2]s or %[3]s)",
	"not (%[1]s or (%[2]s and not %[3]s))",
	// depth 4+: the sentinel sits three and four levels below the negation.
	"not (not (%[1]s or %[2]s) and %[3]s)",
	"not (%[1]s and (%[2]s or not (%[3]s and %[1]s)))",
	"not (not (%[1]s and (%[2]s or %[3]s)) or not %[1]s)",
	"not ((%[1]s or not (%[2]s and %[3]s)) and not (%[2]s or %[3]s))",
}

func esOracleCorpus() []string {
	out := make([]string, 0, 8192)
	for _, tpl := range esOracleTemplates1 {
		for _, l := range esOracleLeaves {
			out = append(out, fmt.Sprintf(tpl, l))
		}
	}
	for _, tpl := range esOracleTemplates2 {
		for _, l1 := range esOracleLeaves {
			for _, l2 := range esOracleLeaves {
				out = append(out, fmt.Sprintf(tpl, l1, l2))
			}
		}
	}
	for _, tpl := range esOracleTemplates3 {
		for _, l1 := range esOracleNarrow {
			for _, l2 := range esOracleNarrow {
				for _, l3 := range esOracleNarrow {
					out = append(out, fmt.Sprintf(tpl, l1, l2, l3))
				}
			}
		}
	}
	return out
}

type esOracleDoc struct {
	id   int
	vals map[string]any
}

func esOracleRows() []esOracleDoc {
	rows := make([]esOracleDoc, 0, 12)
	id := 0
	for _, a := range []int{1, 2, 3} {
		for _, b := range []int{1, 2} {
			for _, c := range []int{1, 3} {
				id++
				rows = append(rows, esOracleDoc{id: id, vals: map[string]any{
					"a": a, "b": b, "c": c, "d": (a+b)%3 + 1,
				}})
			}
		}
	}
	return rows
}

func esOracleDB(t *testing.T, rows []esOracleDoc) *sql.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	db, err := gdb.DB()
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, c INTEGER, d INTEGER)")
	require.NoError(t, err)
	for _, r := range rows {
		_, err = db.Exec("INSERT INTO t (id,a,b,c,d) VALUES (?,?,?,?,?)",
			r.id, r.vals["a"], r.vals["b"], r.vals["c"], r.vals["d"])
		require.NoError(t, err)
	}
	return db
}

func esOracleSQLIDs(t *testing.T, db *sql.DB, where string, args []any) []int {
	t.Helper()
	q := "SELECT id FROM t"
	if strings.TrimSpace(where) != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY id"
	rows, err := db.Query(q, args...)
	require.NoError(t, err, "query: %s args=%v", q, args)
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// ---------------------------------------------------------------------------
// The oracle
// ---------------------------------------------------------------------------

// TestElasticsearchTruthTableOracle is the instrument hunt #8's critique found
// missing: for every DSL in the corpus, the row set Elasticsearch would return
// for the emitted document must equal the row set SQLite returns for the raw
// adapter's SQL. Zero disagreements is the contract.
func TestElasticsearchTruthTableOracle(t *testing.T) {
	rows := esOracleRows()
	db := esOracleDB(t, rows)
	corpus := esOracleCorpus()

	type mismatch struct {
		dsl        string
		sqlIDs     []int
		esIDs      []int
		esDocument string
	}
	var bad []mismatch
	evaluated := 0

	for _, dsl := range corpus {
		f := figo.New()
		if err := f.AddFiltersFromString(dsl); err != nil {
			t.Fatalf("corpus DSL %q did not parse: %v", dsl, err)
		}
		if err := f.BuildE(RawAdapter{}); err != nil {
			t.Fatalf("corpus DSL %q did not build: %v", dsl, err)
		}
		where, args, err := BuildRawWhere(f)
		require.NoError(t, err, "raw where for %q", dsl)
		sqlIDs := esOracleSQLIDs(t, db, where, args)

		esQ, err := BuildElasticsearchQuery(f)
		require.NoError(t, err, "es build for %q", dsl)
		esIDs := []int{}
		for _, r := range rows {
			ok, err := esEvalClause(esQ.Query, r.vals)
			require.NoError(t, err, "evaluating %q -> %v", dsl, esQ.Query)
			if ok {
				esIDs = append(esIDs, r.id)
			}
		}
		sort.Ints(sqlIDs)
		sort.Ints(esIDs)
		evaluated++
		if fmt.Sprint(sqlIDs) != fmt.Sprint(esIDs) {
			body, _ := GetElasticsearchQueryStringCompact(f)
			bad = append(bad, mismatch{dsl: dsl, sqlIDs: sqlIDs, esIDs: esIDs, esDocument: body})
		}
	}

	t.Logf("oracle: %d DSL strings x %d documents; %d disagreements", evaluated, len(rows), len(bad))
	for i, m := range bad {
		if i >= 25 {
			t.Logf("... and %d more", len(bad)-25)
			break
		}
		t.Errorf("SEMANTIC DIVERGENCE\n  dsl : %s\n  sql : %v\n  es  : %v\n  body: %s",
			m.dsl, m.sqlIDs, m.esIDs, m.esDocument)
	}
	require.Empty(t, bad, "the Elasticsearch document must mean what the SQL means")
}

// TestElasticsearchOracleSelfCheck pins the two Lucene rules the evaluator
// encodes, so a future edit to the evaluator cannot quietly make the oracle
// vacuous. Without rule 2 the whole oracle would pass at ef28331.
func TestElasticsearchOracleSelfCheck(t *testing.T) {
	doc := map[string]any{"a": 1}

	// Rule 1: a clause-less bool is match_all.
	ok, err := esEvalClause(map[string]any{"bool": map[string]any{}}, doc)
	require.NoError(t, err)
	require.True(t, ok, "an empty bool query is MatchAllDocsQuery on Lucene")

	// Rule 2: must_not of a zero-matching clause excludes nothing.
	ok, err = esEvalClause(map[string]any{"bool": map[string]any{
		"must_not": []map[string]any{{"match_none": map[string]any{}}},
	}}, doc)
	require.NoError(t, err)
	require.True(t, ok, "must_not of match_none matches EVERY document — the D1 inversion")

	// match_none inside must is fatal to the conjunction.
	ok, err = esEvalClause(map[string]any{"bool": map[string]any{
		"must": []map[string]any{{"term": map[string]any{"a": 1}}, {"match_none": map[string]any{}}},
	}}, doc)
	require.NoError(t, err)
	require.False(t, ok)

	// should + minimum_should_match:1 is a real disjunction.
	ok, err = esEvalClause(map[string]any{"bool": map[string]any{
		"should":               []map[string]any{{"term": map[string]any{"a": 9}}, {"match_none": map[string]any{}}},
		"minimum_should_match": 1,
	}}, doc)
	require.NoError(t, err)
	require.False(t, ok)

	// A missing field never matches a range (the ES analogue of UNKNOWN).
	ok, err = esEvalClause(map[string]any{"range": map[string]any{"zz": map[string]any{"gt": 0}}}, doc)
	require.NoError(t, err)
	require.False(t, ok)

	// An unmodelled clause is an error, never a silent pass.
	_, err = esEvalClause(map[string]any{"wildcard": map[string]any{"a": "x*"}}, doc)
	require.Error(t, err)
}
