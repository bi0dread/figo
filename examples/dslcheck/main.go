// Command dslcheck is the Go half of the playground DSL emitter cross-check.
//
// playground/src/dsl.ts is a second, independently written implementation of
// "what DSL figo accepts". Its own regression suite (dsl.hunt7.regression.ts)
// only pins the emitter's output against itself, so a divergence between the
// TypeScript emitter and this parser is invisible to it by construction — which
// is how every playground finding in hunt #7 survived six hunts, and how hunt #8
// found the emitter still handing users `a<NUL>b=1` and `a..b=1`, identifiers the
// raw adapter refuses to render at all.
//
// dslcheck reads one emitted DSL string per line on stdin (produced by
// playground/src/dsl.crosscheck.ts) and asserts the one-directional invariant
// that matters:
//
//	the playground must never emit a string the Go side rejects.
//
// "Rejects" means a HARD render failure — ok=false from a (string, bool) adapter
// method, a nil Query, or a non-nil error from an error-returning build
// function. A parser DIAGNOSTIC is not a failure here: `Build` skipping malformed
// input while `BuildE` reports it is figo's documented contract, and the
// playground is entitled to emit input that carries a diagnostic as long as the
// query still renders. Diagnostics are reported as informational counts so a
// reviewer can see them without the check going red on the documented contract.
//
// Every string is checked under BOTH naming functions. SnakeCaseNaming (the
// default) masks some malformed identifiers by rewriting them — `a..b` becomes
// `a_b` — while NoChangeNaming passes them through to validateIdent verbatim, so
// checking only the default would have missed the dot half of the hunt-#8
// divergence entirely.
//
// # Exemptions, and why they are keyed on the INPUT
//
// Three adapter behaviours are refusals the playground cannot design away,
// because they are capability limits of one backend rather than malformed DSL:
//
//   - Elasticsearch has no preload path and fails the build for ANY `load=`.
//   - MongoDB's Find path fails closed on a preload carrying predicates, and the
//     adapter rejects a '$'-leading field name outright (it would execute as an
//     operator). A '$'-leading column is perfectly legal on all three SQL
//     dialects, so the emitter cannot strip it without breaking SQL users; it
//     warns instead.
//   - `=~` / `!=~` bind a regex the TARGET ENGINE compiles. An expression that is
//     a valid Go/PCRE regex need not be a valid one for MongoDB, and the emitter
//     cannot arbitrate between four regex dialects.
//
// Each exemption is keyed on a property of the INPUT that the emitter can see,
// never on matching the text of an error message: an error-text carve-out would
// silently absorb a genuinely new failure that happened to word itself the same
// way. The raw adapter — where validateIdent lives, and which is therefore the
// ground truth for identifier validity — is checked with NO exemptions at all.
//
// Usage:
//
//	node .tsbuild/dsl.crosscheck.js | go run ./examples/dslcheck
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// failure is one adapter refusing to render one emitted DSL string.
type failure struct {
	dsl     string
	naming  string
	adapter string
	detail  string
}

// namings are the two shipped NamingFuncs. A third-party func could reject more,
// but these are the two the library documents and the only two it ships.
var namings = []struct {
	name string
	fn   figo.NamingFunc
}{
	{"SnakeCaseNaming", figo.SnakeCaseNaming},
	{"NoChangeNaming", figo.NoChangeNaming},
}

func newFigo(naming figo.NamingFunc, dsl string) figo.Figo {
	f := figo.New()
	f.SetNamingFunc(naming)
	f.AddFiltersFromString(dsl)
	return f
}

// checkRaw exercises the raw adapter on all three dialects, through both the
// (string, bool) surface the playground's users reach and the error-returning
// package-level helpers.
func checkRaw(dsl, naming string, fn figo.NamingFunc, add func(failure)) {
	dialects := []struct {
		name string
		d    *adapters.SQLDialect
	}{
		{"mysql", nil}, // zero value renders MySQL
		{"postgres", adapters.PostgresDialect},
		{"sqlite", adapters.SQLiteDialect},
	}
	for _, dl := range dialects {
		a := adapters.RawAdapter{Dialect: dl.d}
		f := newFigo(fn, dsl)
		f.Build(a)
		ctx := adapters.RawContext{Table: "users"}

		if _, ok := a.GetSqlString(f, ctx); !ok {
			add(failure{dsl, naming, "raw/" + dl.name, "GetSqlString returned ok=false"})
		}
		if q, ok := a.GetQuery(f, ctx); !ok || q == nil {
			add(failure{dsl, naming, "raw/" + dl.name, fmt.Sprintf("GetQuery returned ok=%v nil=%v", ok, q == nil)})
		}
		// The full-SELECT path additionally validates the table name and the
		// projection; the segment path validates the same identifiers through a
		// different call chain, so both are worth crossing.
		if _, ok := a.GetSqlString(f, ctx, "SELECT", "FROM", "WHERE", "ORDER BY", "LIMIT"); !ok {
			add(failure{dsl, naming, "raw/" + dl.name, "GetSqlString(segments) returned ok=false"})
		}
	}

	f := newFigo(fn, dsl)
	f.Build(adapters.RawAdapter{})
	if _, _, err := adapters.BuildRawWhere(f); err != nil {
		add(failure{dsl, naming, "raw/BuildRawWhere", err.Error()})
	}
	if _, _, err := adapters.BuildRawSelect(f, "users"); err != nil {
		add(failure{dsl, naming, "raw/BuildRawSelect", err.Error()})
	}
}

// gormDB is a DryRun-only sqlite handle. GormAdapter renders by applying the
// instance to a *gorm.DB and reading the statement back, so it needs a real
// handle as its ctx — passing nil makes every call report ok=false and the leg
// would prove nothing.
func gormDB() (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open("file:dslcheck?mode=memory&cache=shared"), &gorm.Config{
		DryRun: true,
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}

type gormRow struct {
	ID int
}

func checkGorm(db *gorm.DB, dsl, naming string, fn figo.NamingFunc, add func(failure)) {
	f := newFigo(fn, dsl)
	f.Build(adapters.GormAdapter{})
	ctx := db.Session(&gorm.Session{DryRun: true, NewDB: true}).Model(&gormRow{})
	if _, ok := (adapters.GormAdapter{}).GetSqlString(f, ctx); !ok {
		add(failure{dsl, naming, "gorm", "GetSqlString returned ok=false"})
	}
	if q, ok := (adapters.GormAdapter{}).GetQuery(f, ctx); !ok || q == nil {
		add(failure{dsl, naming, "gorm", fmt.Sprintf("GetQuery returned ok=%v nil=%v", ok, q == nil)})
	}
}

// dollarLeadingIdent reports whether any '.'-separated segment of any field name
// or sort key in the instance starts with '$'. MongoDB rejects those (they would
// execute as operators); SQL does not. See the package comment.
func dollarLeadingIdent(f figo.Figo) bool {
	if hasDollarExpr(f.GetClauses()) {
		return true
	}
	for _, exprs := range f.GetPreloads() {
		if hasDollarExpr(exprs) {
			return true
		}
	}
	if s := f.GetSort(); s != nil {
		for _, c := range s.Columns {
			if strings.HasPrefix(c.Name, "$") {
				return true
			}
		}
	}
	for _, rel := range sortedKeys(f.GetPreloads()) {
		if strings.HasPrefix(rel, "$") {
			return true
		}
	}
	for name := range f.GetSelectFields() {
		if strings.HasPrefix(name, "$") {
			return true
		}
	}
	return false
}

func hasDollarExpr(exprs []figo.Expr) bool {
	found := false
	for _, e := range exprs {
		figo.Walk(e, func(n figo.Expr) {
			if fld, ok := figo.NodeField(n); ok && strings.HasPrefix(fld, "$") {
				found = true
			}
		})
	}
	return found
}

func sortedKeys(m map[string][]figo.Expr) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// regexOperand reports whether the built tree contains a regex node, whose value
// the TARGET engine compiles. See the package comment.
//
// This inspects the parsed AST rather than the DSL text on purpose: a substring
// test for "=~" also matches a quoted VALUE containing it (`a="x=~y"`), which
// would exempt a case that ought to be checked. An exemption that is wider than
// the behaviour it excuses is how a real divergence gets absorbed.
func regexOperand(f figo.Figo) bool {
	found := false
	visit := func(n figo.Expr) {
		switch n.(type) {
		case *figo.RegexExpr, figo.RegexExpr:
			found = true
		}
	}
	for _, e := range f.GetClauses() {
		figo.Walk(e, visit)
	}
	for _, exprs := range f.GetPreloads() {
		for _, e := range exprs {
			figo.Walk(e, visit)
		}
	}
	return found
}

func checkMongo(dsl, naming string, fn figo.NamingFunc, add func(failure)) {
	f := newFigo(fn, dsl)
	f.Build(adapters.MongoAdapter{})
	if dollarLeadingIdent(f) || regexOperand(f) {
		return
	}

	// The Find path fails closed when the instance carries preload predicates it
	// cannot express (hunt #6 M7) — the adapter's DOCUMENTED answer to `load=`
	// with conditions, not an emitter defect.
	if _, err := adapters.BuildMongoFilter(f); err != nil && len(f.GetPreloads()) == 0 {
		add(failure{dsl, naming, "mongo/find", err.Error()})
	}

	// The aggregate path needs a joins entry per preload relation; supply a valid
	// one for every relation the DSL named so a missing-config error (hunt #6 H13)
	// cannot be mistaken for an emitter defect.
	joins := map[string]adapters.MongoJoin{}
	for i, rel := range sortedKeys(f.GetPreloads()) {
		joins[rel] = adapters.MongoJoin{
			From:         "related",
			LocalField:   "_id",
			ForeignField: "parent_id",
			As:           fmt.Sprintf("rel_%d", i),
		}
	}
	if _, err := adapters.BuildMongoAggregatePipeline(f, joins); err != nil {
		add(failure{dsl, naming, "mongo/aggregate", err.Error()})
	}
}

func checkElasticsearch(dsl, naming string, fn figo.NamingFunc, add func(failure)) {
	f := newFigo(fn, dsl)
	f.Build(adapters.ElasticsearchAdapter{})

	// The ES adapter has no preload path and fails the build for ANY `load=`
	// (documented). That is an adapter capability limit, not an emitter defect.
	if len(f.GetPreloads()) > 0 {
		return
	}
	if _, err := adapters.GetElasticsearchQueryString(f); err != nil {
		add(failure{dsl, naming, "elasticsearch", err.Error()})
	}
}

func main() {
	var (
		failures []failure
		lines    int
		diags    int
	)
	add := func(f failure) { failures = append(failures, f) }

	db, err := gormDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dslcheck: opening the DryRun sqlite handle: %v\n", err)
		os.Exit(2)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		dsl := sc.Text()
		if strings.TrimSpace(dsl) == "" {
			continue
		}
		lines++
		for _, n := range namings {
			checkRaw(dsl, n.name, n.fn, add)
			checkGorm(db, dsl, n.name, n.fn, add)
			checkMongo(dsl, n.name, n.fn, add)
			checkElasticsearch(dsl, n.name, n.fn, add)
		}
		// Informational: BuildE's diagnostic channel. Not a failure — see the
		// package comment.
		f := figo.New()
		if err := f.AddFiltersFromString(dsl); err != nil {
			diags++
		} else if err := f.BuildE(adapters.RawAdapter{}); err != nil {
			diags++
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "dslcheck: reading stdin: %v\n", err)
		os.Exit(2)
	}
	if lines == 0 {
		fmt.Fprintln(os.Stderr, "dslcheck: no DSL strings on stdin — the emitter half did not run")
		os.Exit(2)
	}

	fmt.Printf("dslcheck: %d emitted DSL strings x %d naming funcs x 4 adapters\n", lines, len(namings))
	fmt.Printf("dslcheck: %d carried a BuildE diagnostic (informational — Build/BuildE contract)\n", diags)

	if len(failures) == 0 {
		fmt.Println("dslcheck: PASS — the emitter produced nothing the Go side refuses to render")
		return
	}

	// Group by adapter+detail so one systematic divergence reads as one entry
	// rather than as thousands of lines.
	type key struct{ naming, adapter, detail string }
	groups := map[key][]string{}
	for _, f := range failures {
		k := key{f.naming, f.adapter, f.detail}
		groups[k] = append(groups[k], f.dsl)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].adapter != keys[j].adapter {
			return keys[i].adapter < keys[j].adapter
		}
		if keys[i].naming != keys[j].naming {
			return keys[i].naming < keys[j].naming
		}
		return keys[i].detail < keys[j].detail
	})
	fmt.Fprintf(os.Stderr, "\ndslcheck: FAIL — %d refusals in %d distinct classes\n", len(failures), len(groups))
	for _, k := range keys {
		ex := groups[k]
		sort.Strings(ex)
		fmt.Fprintf(os.Stderr, "\n  [%s / %s] %s\n    %d case(s), e.g.\n", k.adapter, k.naming, k.detail, len(ex))
		for i, d := range ex {
			if i == 3 {
				break
			}
			fmt.Fprintf(os.Stderr, "      %q\n", d)
		}
	}
	fmt.Fprintln(os.Stderr, "\nThe emitter must not produce input the Go side rejects. Fix playground/src/dsl.ts.")
	os.Exit(1)
}
