package plugins

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	figo "github.com/bi0dread/figo/v4"
)

// InjectionGuardPlugin screens every COLUMN IDENTIFIER a query is about to
// address and refuses the request — fail closed — when one of them is not a
// name a column can have.
//
// It exists because of this report:
//
//	filters=page%3Dskip:0,take:50+and+sort%3Dmedian_price:desc+or+1=1
//
// A `page=`/`sort=`/`load=` directive is consumed as a statement and its
// connector is discarded, so the `or` vanished and `1=1` became the ENTIRE
// top-level filter — a filter whose field is the literal `1`. figo rendered
//
//	WHERE `1` = ?
//
// with AddFiltersFromString returning nil, BuildE returning nil and Explain()
// printing "1 = 1". There was no signal anywhere that the query addressed an
// invented column, so the service ran it, the engine refused it, and the
// driver error — which quotes the whole statement — was returned to the
// caller as schema disclosure.
//
// This is NOT SQL injection and this plugin does not pretend to fix one.
// Values are bound (?/$n) and identifiers are quote-doubled by every adapter,
// so `1=1` reaches the engine as one quoted identifier and can never break out
// of it. The defect is that a nonsense identifier reached the database at all,
// with no way for the caller to reject the request. Both halves are what this
// plugin supplies:
//
//   - AfterParse returns a descriptive error, so AddFiltersFromString finally
//     has something to say about a malformed query and the service can answer
//     400 instead of forwarding a driver error.
//   - Every Build of a refused query renders figo's never-true clause (an empty
//     OrExpr, which is 1=0 on the SQL adapters, {"$nor":[{}]} on Mongo and
//     match_none on Elasticsearch), so a caller that IGNORES the error runs a
//     query matching nothing rather than an unfiltered scan.
//
// The second half is not belt-and-braces, it is load bearing, and it is not
// this plugin's doing: core installs the never-true clause whenever a parse
// hook rejects (figo's refuseDSL), which is the only place it can be installed
// safely. Every route a plugin can reach is prunable or escapable — an
// expression added through AddFilter is deleted by a FieldsPlugin policy, and a
// verdict remembered per instance is lost by Clone — and each of those holes
// turns a refusal into a full unfiltered scan, strictly worse than the
// malformed query being refused. Registering this plugin therefore also
// improves ValidationPlugin, LimitsPlugin and SyntaxPlugin rejections, which
// go through the same path.
//
// # Usage
//
//	g := plugins.NewInjectionGuardPlugin()
//	g.AllowFields("median_price", "city", "title") // optional but recommended
//	f := figo.New()
//	if err := f.RegisterPlugin(g); err != nil { ... }
//
//	if err := f.AddFiltersFromString(userDSL); err != nil {
//	    return c.Status(400).JSON(fiber.Map{"error": "invalid filters"})
//	}
//
// CHECK THAT ERROR. It is the signal, and it is the ONLY one: core keeps a
// plugin's parse-hook error on the AddFiltersFromString call that produced it,
// so a later BuildE returns nil for a query this plugin has refused. A caller
// that validates only through BuildE sees nothing — it just gets a query that
// matches nothing, which is safe but silent. Rejected(f) answers after the
// fact if that suits your call shape better.
//
// Do not echo err back to an untrusted client verbatim if you would rather not
// tell it which of its identifiers was refused; the message contains the
// caller's own input and never anything about your schema, but "invalid
// filters" is the safer answer. Log the full message.
//
// # What is screened
//
// Every identifier position that untrusted DSL can reach:
//
//	filter fields          `1=1`, `x--=1`, `x<>1` (which parses as field "x<"),
//	                       `x;=1`, "`x`=1", `a​b=1`
//	sort keys              `sort=1:desc`, `sort=x--:desc`, `sort=x;drop:desc`
//	preload relations      `load=[1:id=1]`, "load=[Rel`x:id=1]"
//	preload conditions     `load=[Rel:1=1]`
//	select fields          SetSelectFields("*"), SetSelectFields("(select 1)")
//
// Names are screened in the spelling that will actually be RENDERED, i.e.
// after the instance's naming func has run. That matters most for select
// fields, which are the one position figo converts at render time rather than
// on the way in: screening the caller's spelling instead would both miss a
// name that only becomes hostile after conversion and refuse a name that only
// looks hostile before it (under the default SnakeCaseNaming, "first-name" is
// a legal input that renders as `first_name`).
//
// The rule is one to MaxSegments dot-separated segments, each of at most
// MaxSegmentLength runes, made of letters, digits, `_` and `$`, and no segment
// composed solely of digits — which is exactly what the reported `1` was. It
// is deliberately narrower than "what an adapter can quote": every adapter can
// quote `1`, `x--`, `x;drop` and a backtick and render them inert, and every
// one of them is still a name no column has.
//
// Field names on the document adapters are not SQL columns and often break
// this rule legitimately (`@timestamp`, `user-agent`, `meta.tags.0`, paths
// more than three levels deep). Relax it for those with
//
//	g.AllowExtraRunes("-@").AllowNumericSegments(true).SetMaxSegments(8)
//
// CustomExpr is EXEMPT by default, matching the adapters: its handler receives
// the field verbatim and owns its own quoting by documented contract
// (adapters/raw.go, adapters/gorm.go), so a legitimate handler is routinely
// given an expression rather than a column name. CustomExpr cannot be produced
// by the DSL — only by AddFilter — so it is not attacker reachable. Turn the
// screen on with ScreenCustomExpr(true) if your handlers only ever take plain
// column names.
//
// # How the refusal survives
//
// Core keeps it. A parse-hook rejection drops the DSL and replaces the clause
// list with the never-true clause, in state Clone deep-copies and no
// ExprFilter runs over, so a clone of a refused instance is refused too and no
// other plugin can launder it away. The instance stays closed until it is given
// a DSL that parses acceptably; AddFiltersFromString("") does not reopen it.
//
// On top of that, every Build re-screens the clause list, sort and projection
// it is about to render. That is what catches a hostile name that never went
// through the DSL at all — AddFilter, SetSort, SetSelectFields — where there is
// no parse hook to reject from. State set AFTER the last Build is screened by
// the NEXT one, because there is no hook between a setter and a render.
//
// A panic while screening becomes an ordinary refusal rather than escaping:
// the probe parse runs the APPLICATION'S naming func over attacker-supplied
// names, and a panic escaping AddFiltersFromString would take core's refusal
// with it.
//
// # Concurrency and lifetime
//
// Safe for concurrent use. The reporting latch is bounded (see SetMaxLatched)
// and generational, so a flood of rejected requests cannot grow it without
// limit; an evicted entry costs only the detail Rejected would have reported,
// never the refusal itself, which core holds.
type InjectionGuardPlugin struct {
	mu                sync.RWMutex
	allowUnicode      bool
	allowNumericSegs  bool
	extraRunes        string
	maxSegments       int
	maxSegmentRunes   int
	rejectDiagnostics bool
	screenCustom      bool
	allowed           map[string]bool

	// Latch state. Kept under its own mutex so a Build's FinalizeClauses never
	// contends with a concurrent AllowFields on the config lock.
	//
	// Two generations rather than one map plus per-entry bookkeeping: when cur
	// fills, it becomes prev and a fresh cur is started, so every entry is
	// guaranteed to survive at least maxLatched further rejections and the
	// table never exceeds 2*maxLatched. Reads check both.
	lmu        sync.Mutex
	cur, prev  map[figo.Figo][]Violation
	maxLatched int
}

// Defaults. 64 RUNES per segment is MySQL's identifier limit (PostgreSQL's is
// 63), measured in characters as MySQL measures it. Four segments covers
// database.schema.table.column, which SQL Server and BigQuery both use and
// which figo renders correctly.
const (
	defaultMaxSegmentRunes = 64
	defaultMaxSegments     = 4
	defaultMaxLatched      = 4096
)

// Position names the place an identifier was found. It is part of the error
// message so a caller can tell "you filtered on a bad column" from "you sorted
// by one".
type Position string

const (
	PositionFilterField     Position = "filter field"
	PositionSortKey         Position = "sort key"
	PositionSelectField     Position = "select field"
	PositionPreloadRelation Position = "preload relation"
	PositionJSONPath        Position = "json path"
	PositionParse           Position = "parse"
)

// Violation is one refused identifier (or, for PositionParse, one thing the
// parser had to drop). Name carries the caller's own input verbatim and never
// anything derived from the schema.
type Violation struct {
	Position Position
	Name     string
	Reason   string
}

func (v Violation) String() string {
	if v.Position == PositionParse {
		return fmt.Sprintf("%s: %s", v.Position, v.Reason)
	}
	return fmt.Sprintf("%s %q: %s", v.Position, v.Name, v.Reason)
}

// violationError renders a whole set as one error. Violations are reported
// together rather than one at a time: a caller fixing a query wants the list,
// and a caller logging an attack wants the shape of it.
func violationError(vs []Violation) error {
	if len(vs) == 0 {
		return nil
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.String()
	}
	return fmt.Errorf("figo-injection-guard: rejected %d identifier(s): %s",
		len(vs), strings.Join(parts, "; "))
}

// NewInjectionGuardPlugin returns a guard with the strict defaults: ASCII
// identifiers only, at most 4 dot-separated segments of at most 64 runes, no
// all-digit segment, no field allowlist (syntax screening only), diagnostics
// that mean a filter was dropped treated as a rejection, CustomExpr exempt.
func NewInjectionGuardPlugin() *InjectionGuardPlugin {
	return &InjectionGuardPlugin{
		maxSegments:       defaultMaxSegments,
		maxSegmentRunes:   defaultMaxSegmentRunes,
		rejectDiagnostics: true,
		maxLatched:        defaultMaxLatched,
	}
}

// AllowFields restricts every identifier position — filter fields, sort keys,
// select fields AND preload relation names — to the given names, on top of the
// syntax screen. This is the part that actually stops probing: without it a
// syntactically valid but nonexistent column (`is_admin=1` against a table
// that has no such column) still reaches the engine and still produces a
// driver error to mine. Relation names go in the same list; there is no
// separate AllowRelations.
//
// Names match verbatim OR in their naming-converted spelling, in BOTH
// directions: AllowFields("City") accepts city= under SnakeCaseNaming (the
// registration is converted), and AllowFields("median_price") accepts
// medianPrice= (the input is converted). Calling it more than once
// accumulates.
//
// It applies to the CALLER'S DSL and nothing else. The syntax screen runs
// everywhere, but holding the rendered clause list to an allowlist would catch
// clauses the SERVER injected rather than the caller: a ScopePlugin registered
// before this guard has already appended its mandatory tenant filter by the
// time the clause list is screened, so AllowFields("city") would pill every
// query on a tenant-scoped instance — and the only way out would be to
// allowlist the very column the caller must never be able to name. Columns
// your own code passes to AddFilter, SetSort or SetSelectFields are likewise
// screened for shape but not membership; if those carry user input, run
// CheckDSL or your own check over it first.
func (p *InjectionGuardPlugin) AllowFields(names ...string) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.allowed == nil {
		p.allowed = make(map[string]bool, len(names))
	}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			p.allowed[n] = true
		}
	}
	return p
}

// AllowUnicodeIdentifiers accepts any Unicode letter where the default rule
// accepts only ASCII. Set it when the schema genuinely has non-ASCII column
// names (`عنوان`, `名前`).
//
// It stays strict where it matters: unicode.IsLetter excludes combining marks
// (Mn), format characters (Cf) and separators, so zero-width joiners, the RTL
// override U+202E and bare combining accents are refused in both modes. What
// it cannot do is stop a homoglyph — Cyrillic `а` is a letter, and so is Latin
// `a`. Pair it with AllowFields if that matters to you.
func (p *InjectionGuardPlugin) AllowUnicodeIdentifiers(on bool) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowUnicode = on
	return p
}

// AllowExtraRunes permits additional characters inside an identifier segment,
// in any position. Use it for stores whose field names are not SQL columns:
//
//	g.AllowExtraRunes("-@") // Elasticsearch's @timestamp, a user-agent field
//
// A hard floor applies whatever is listed: the three quote runes (`, " and '),
// the statement separator ; and every control byte are refused regardless. It
// is a floor, not a whitelist of everything dangerous — admitting '-' for
// `user-agent` also admits `x--`, which the SQL adapters render backtick-quoted
// and inert. Relax it for what your store actually names, not by habit.
func (p *InjectionGuardPlugin) AllowExtraRunes(runes string) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.extraRunes += runes
	return p
}

// AllowNumericSegments permits a segment composed entirely of digits.
//
// Default OFF, and it should stay off on a SQL store: an all-digit name is
// precisely the shape of the reported bug (`1=1` filtering on a column named
// `1`), and no SQL engine has such a column. Turn it on for Mongo or
// Elasticsearch, where `meta.tags.0` addresses an array element and the name
// is a BSON/JSON key that cannot be mistaken for a literal.
func (p *InjectionGuardPlugin) AllowNumericSegments(on bool) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowNumericSegs = on
	return p
}

// SetMaxSegments caps how many dot-separated segments an identifier may have
// (default 4: database.schema.table.column). Values below 1 are ignored.
func (p *InjectionGuardPlugin) SetMaxSegments(n int) *InjectionGuardPlugin {
	if n < 1 {
		return p
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxSegments = n
	return p
}

// SetMaxSegmentLength caps the length of one segment in RUNES (default 64,
// MySQL's identifier limit). Values below 1 are ignored.
func (p *InjectionGuardPlugin) SetMaxSegmentLength(n int) *InjectionGuardPlugin {
	if n < 1 {
		return p
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxSegmentRunes = n
	return p
}

// RejectParseDiagnostics controls whether a diagnostic from the parser is a
// rejection (default true).
//
// Leave it on for untrusted input. figo's own contract is that "Build skips
// malformed input silently and BuildE reports it", which means a dropped
// conjunct widens the query: `a b=1` renders WHERE `b` = ? and nothing else,
// and `id=1 union select 1` renders the same. Neither is dangerous by itself,
// but neither is the query the caller sent, and treating "figo could not parse
// part of this" as acceptable is how the reported bug got as far as the
// database.
//
// Diagnostics that report something INFORMATIONAL are never a rejection, on or
// off — see informationalDiagnostic. figo's diagnostic channel is not purely a
// "the query was altered" channel, and refusing documented, behaviour-neutral
// input would be the fastest way to get this plugin switched off.
func (p *InjectionGuardPlugin) RejectParseDiagnostics(on bool) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rejectDiagnostics = on
	return p
}

// ScreenCustomExpr brings CustomExpr.Field under the identifier screen
// (default false — see the type comment for why it is exempt).
func (p *InjectionGuardPlugin) ScreenCustomExpr(on bool) *InjectionGuardPlugin {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.screenCustom = on
	return p
}

// SetMaxLatched bounds the rejection table (default 4096 per generation, so at
// most 8192 entries). Values below 1 are ignored.
func (p *InjectionGuardPlugin) SetMaxLatched(n int) *InjectionGuardPlugin {
	if n < 1 {
		return p
	}
	p.lmu.Lock()
	defer p.lmu.Unlock()
	p.maxLatched = n
	return p
}

// Name implements figo.Plugin
func (p *InjectionGuardPlugin) Name() string { return "figo-injection-guard" }

// Version implements figo.Plugin
func (p *InjectionGuardPlugin) Version() string { return "1.0.0" }

// Initialize implements figo.Plugin
func (p *InjectionGuardPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements figo.Plugin
func (p *InjectionGuardPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements figo.Plugin
func (p *InjectionGuardPlugin) AfterQuery(figo.Figo, any, any) error { return nil }

// BeforeParse implements figo.Plugin
func (p *InjectionGuardPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) {
	return dsl, nil
}

// AfterParse implements figo.Plugin: it parses the DSL on a plugin-free probe
// instance, screens every identifier the parse produced, and rejects.
//
// The probe is a bare figo.New() rather than cloneForInspection because the
// guard must see what the CALLER SENT, not what other plugins left of it. A
// FieldsPlugin whitelist prunes a disallowed field inside the inspection
// clone's own Build, so a guard reading that clone finds nothing to refuse and
// the request is silently answered over a wider set — the exact outcome a
// reject control exists to replace. A bare instance also runs no hooks, so
// there is no recursion back into this plugin and no extra pass for the
// ExprFilters cloneForInspection would have carried.
func (p *InjectionGuardPlugin) AfterParse(f figo.Figo, dsl string) (err error) {
	if f == nil || strings.TrimSpace(dsl) == "" {
		return nil
	}

	// A panic in here must not widen the query, and this hook can panic on
	// input the caller chose: the probe parse runs the APPLICATION'S naming
	// func over attacker-supplied field names, so a naming func that panics on
	// some spelling panics here. Core's own guardPluginPanic fails closed for a
	// panic during Build, but a panic during AfterParse escapes
	// AddFiltersFromString, whose deferred rollback then restores the previous
	// (usually empty) DSL — leaving an application with panic-recovering
	// middleware holding an instance that renders a full unfiltered scan.
	// Turning it into an ordinary refusal keeps that instance closed and tells
	// the caller why.
	defer func() {
		if r := recover(); r != nil {
			vs := []Violation{{
				Position: PositionParse,
				Reason:   fmt.Sprintf("panic while screening the query: %v", r),
			}}
			p.latch(f, vs)
			err = violationError(vs)
		}
	}()

	naming := f.GetNamingFunc()
	vs := p.checkDSL(dsl, naming)

	// Programmatic state travels with the instance, not the DSL, so it is
	// screened here too — for shape only (see AllowFields): a hostile name that
	// arrived through SetSelectFields or SetSort would otherwise only be caught
	// at render, with no error anywhere.
	vs = append(vs, p.screenSort(f.GetSort())...)
	vs = append(vs, p.screenSelectFields(f.GetSelectFields(), naming)...)
	vs = dedupeViolations(vs)

	if len(vs) == 0 {
		p.clear(f)
		return nil
	}

	p.latch(f, vs)
	return violationError(vs)
}

// CheckDSL screens a DSL string without touching any figo instance, for
// callers that want to validate before they build. naming may be nil, in which
// case figo's default (SnakeCaseNaming) is used — pass the same naming func
// the real instance uses, since the parser converts field names on the way in
// and it is the CONVERTED name that reaches SQL.
func (p *InjectionGuardPlugin) CheckDSL(dsl string, naming figo.NamingFunc) []Violation {
	return p.checkDSL(dsl, naming)
}

func (p *InjectionGuardPlugin) checkDSL(dsl string, naming figo.NamingFunc) []Violation {
	probe := figo.New()
	if naming != nil {
		probe.SetNamingFunc(naming)
	}
	// A bare instance has no plugin manager, so this stores the string without
	// running any hook.
	_ = probe.AddFiltersFromString(dsl)
	diag := probe.BuildE(nil)

	allow := p.allowMatcher(probe.GetNamingFunc())

	var vs []Violation
	for _, c := range probe.GetClauses() {
		vs = append(vs, p.screenExprWith(c, allow)...)
	}
	for relation, conds := range probe.GetPreloads() {
		vs = append(vs, allow(PositionPreloadRelation, relation)...)
		for _, c := range conds {
			vs = append(vs, p.screenExprWith(c, allow)...)
		}
	}
	for _, col := range sortColumns(probe.GetSort()) {
		vs = append(vs, allow(PositionSortKey, col)...)
	}

	if diag != nil && p.rejectDiagnosticsEnabled() {
		// One violation per diagnostic line: errors.Join renders its parts
		// newline-separated, and a caller reading the message wants them
		// itemised the same way the identifier violations are.
		for _, line := range strings.Split(diag.Error(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || informationalDiagnostic(line) {
				continue
			}
			vs = append(vs, Violation{Position: PositionParse, Reason: line})
		}
	}
	return dedupeViolations(vs)
}

// informationalDiagnostic reports whether a parser diagnostic describes
// something that changed NOTHING about which rows the query matches.
//
// The list is deliberately tiny and matched by substring, because the default
// has to be "a diagnostic is a rejection": a diagnostic figo adds in future
// starts out refused, which is the safe direction. Both entries here are
// behaviour figo documents as supported, and refusing them would have been the
// fastest route to this plugin being switched off:
//
//   - A blank list part in <in>/<nin>. README: "A blank list part — a leading,
//     trailing or doubled comma — is skipped rather than becoming an
//     empty-string member". `id<in>[1,2,]` is what `ids.join(",") + ","`
//     produces, and refusing it turned a correct two-row answer into zero.
//   - A preload with no conditions. `load=[Orders:]` is a first-class case in
//     the core, and it is named as such in this file's own FinalizePreloads
//     comment — yet it made the PARENT query match nothing.
func informationalDiagnostic(line string) bool {
	return strings.Contains(line, "empty element in list") ||
		strings.Contains(line, "produced no conditions; relation preloaded unfiltered")
}

// FinalizeClauses implements figo.ClauseFinalizer: it is where the refusal
// becomes a query that matches nothing.
//
// This hook runs at the end of EVERY Build — including one whose DSL produced
// no filters at all — and it runs AFTER every ExprFilter, which is the only
// place a refusal can survive. An empty OrExpr returned from FilterExpr or
// handed to AddFilter is DELETED by FieldsPlugin's pruning (pruneExprFields
// drops a logical node with no operands), so a pill installed anywhere earlier
// would be laundered back into a filter-less match-everything query the moment
// a FieldsPlugin was registered.
//
// Two things can trigger it. A latched AfterParse rejection, which is the DSL
// path — by the time Build runs, AddFiltersFromString has already rolled the
// rejected DSL away and there is nothing left in the tree to recognise. And a
// direct screen of the clause list actually being rendered, which catches a
// hostile name that never went through the DSL at all: AddFilter, SetSort or
// SetSelectFields.
//
// Note what the second trigger implies about ordering: state set AFTER the
// last Build is screened by the NEXT one, because there is no hook between a
// setter and a render.
func (p *InjectionGuardPlugin) FinalizeClauses(f figo.Figo, clauses []figo.Expr) []figo.Expr {
	if f == nil {
		return clauses
	}

	vs := append(p.latched(f), p.screenRenderState(f, clauses)...)
	if len(vs) == 0 {
		return clauses
	}

	// Strip the hostile ORDER BY and projection entries as well. The pill makes
	// the WHERE never-true, but ORDER BY `1` DESC and SELECT `1` are rendered
	// from separate state and would still reach the engine — and reaching the
	// engine at all is what produced the schema-disclosing driver error this
	// plugin exists to stop. A SELECT list is resolved by the engine BEFORE any
	// row is filtered, so `WHERE 1=0` does not neutralise it on its own. Only
	// the offending entries are dropped; a clean sort or projection alongside a
	// hostile filter is left exactly as the caller set it.
	//
	// Both writes go through the public setters, which is also where the two
	// positions stop behaving alike — core's asymmetry, not a choice made here.
	// SetSelectFields under inFinalize writes the RENDER and leaves the
	// caller's request alone, and finalizeClauses re-derives the render from
	// that request on every Build, so the projection prune lasts exactly one
	// Build. The sort has no "asked" copy, so SetSort is the instance's sort
	// from then on: a refused column does not come back on a later Build. That
	// is the safe direction, it is what FieldsPlugin's sort pruning already
	// does, and a caller who wants the column back sets the sort again.
	naming := f.GetNamingFunc()
	p.pruneSort(f, f.GetSort())
	p.pruneSelectFields(f, f.GetSelectFields(), naming)

	return []figo.Expr{figo.OrExpr{}}
}

// screenRenderState screens everything this Build is about to render. Shape
// only: the clause list can hold clauses a ScopePlugin injected, which are the
// server's and not the caller's (see AllowFields).
func (p *InjectionGuardPlugin) screenRenderState(f figo.Figo, clauses []figo.Expr) []Violation {
	naming := f.GetNamingFunc()
	var vs []Violation
	for _, c := range clauses {
		vs = append(vs, p.screenExpr(c)...)
	}
	vs = append(vs, p.screenSort(f.GetSort())...)
	vs = append(vs, p.screenSelectFields(f.GetSelectFields(), naming)...)
	return vs
}

// FinalizePreloads implements figo.PreloadFinalizer: a rejected query's
// preloads must match nothing too.
//
// Returning the pill rather than an empty list is deliberate. An empty list
// DROPS a conditioned relation, which is also closed, but it is a no-op for a
// relation that was preloaded unconditioned (load=[Orders:]) — and there the
// relation would still be fetched in full. A never-true condition closes both.
//
// The relation NAME cannot be repaired here (figo has no setter for it), so a
// hostile name still reaches GORM's Preload and Mongo's $lookup. Neither
// renders it as SQL — GORM resolves it against the schema and answers
// ErrUnsupportedRelation — and the top-level pill means there are no parent
// rows for a preload to hang off in any case. It is refused loudly by
// AfterParse, which is where a hostile relation name is meant to be caught.
func (p *InjectionGuardPlugin) FinalizePreloads(f figo.Figo, relation string, conds []figo.Expr) []figo.Expr {
	if f == nil {
		return conds
	}

	vs := p.latched(f)
	vs = append(vs, p.screenIdent(PositionPreloadRelation, relation)...)
	for _, c := range conds {
		vs = append(vs, p.screenExpr(c)...)
	}
	if len(vs) == 0 {
		return conds
	}
	return []figo.Expr{figo.OrExpr{}}
}

// Rejected reports what f is currently refused for.
//
// It answers from the reason recorded by AfterParse, and — when there is none
// — from a live screen of the instance's current state. That second half is
// what covers the programmatic path, which has no error channel of its own:
// AddFilter returns nothing and a clause finalizer cannot fail a Build, so a
// refused SetSort or SetSelectFields would otherwise return zero rows with
// nothing for an operator to grep for.
//
// On that path, call it BEFORE Build. A Build strips the offending sort and
// projection entries, so afterwards there is nothing left to screen and this
// reports clean about a query that is (correctly) never-true. It deliberately
// does not guess from the presence of the never-true clause: that clause is
// also what a refused-then-reopened instance carries until its next Build, and
// reporting a stale refusal is worse than reporting none.
func (p *InjectionGuardPlugin) Rejected(f figo.Figo) ([]Violation, bool) {
	if f == nil {
		return nil, false
	}
	if vs := p.latched(f); len(vs) > 0 {
		return vs, true
	}
	if vs := dedupeViolations(p.screenRenderState(f, f.GetClauses())); len(vs) > 0 {
		return vs, true
	}
	return nil, false
}

// Release drops the recorded reason for f's refusal. It does NOT reopen the
// query: core holds a refused instance closed until it is given a DSL that
// parses acceptably, which is the only way to reuse one.
func (p *InjectionGuardPlugin) Release(f figo.Figo) { p.clear(f) }

// ---------------------------------------------------------------------------
// screening
// ---------------------------------------------------------------------------

// screenExpr screens every identifier reachable from one expression, for shape
// only.
//
// figo.Walk is used rather than a hand-rolled type switch so a node type added
// to the core is visited here for free, and because it hands the visitor
// POINTER nodes, which is the form figo.NodeField reads. Positions Walk
// reaches but NodeField does not are handled explicitly: OrderBy (which
// carries its columns in a slice, so there is no single "the field") and
// JsonPathExpr.Path (which the document adapters concatenate onto Field to
// form the queried key). So are the POINTER forms of the logical nodes, which
// Walk itself passes through without recursing — see screenLogicalOperands.
func (p *InjectionGuardPlugin) screenExpr(e figo.Expr) []Violation {
	return p.screenExprWith(e, p.screenIdent)
}

func (p *InjectionGuardPlugin) screenExprWith(e figo.Expr, screen func(Position, string) []Violation) []Violation {
	if e == nil {
		return nil
	}
	screenCustom := p.screenCustomEnabled()

	var vs []Violation
	figo.Walk(derefLogical(e), func(n figo.Expr) {
		switch v := n.(type) {
		case *figo.OrderBy:
			for _, col := range v.Columns {
				vs = append(vs, screen(PositionSortKey, col.Name)...)
			}
			return
		case *figo.JsonPathExpr:
			vs = append(vs, screen(PositionFilterField, v.Field)...)
			vs = append(vs, screenJSONPath(v.Path)...)
			return
		case *figo.CustomExpr:
			if !screenCustom {
				return
			}
			vs = append(vs, screen(PositionFilterField, v.Field)...)
			return
		}
		if field, ok := figo.NodeField(n); ok {
			vs = append(vs, screen(PositionFilterField, field)...)
		}
	})
	return vs
}

// derefLogical rewrites every POINTER logical node in the tree into its value
// form, so figo.Walk recurses into it.
//
// Walk visits an already-pointer node through its default arm, which does NOT
// recurse — so every identifier under a *AndExpr/*OrExpr/*NotExpr handed to
// AddFilter would be invisible to the screen. Every adapter refuses a pointer
// node today, so nothing hostile can render through one, but a screen that
// silently stops at a subtree is one adapter change away from mattering.
//
// The obvious fix — a *AndExpr arm in the VISITOR that re-screens the operands
// — is a remote denial of service, and a subtle one. Walk's VALUE arm does
// `v.Operands = walkOperands(...)` and then `visit(&v)`: it recurses first and
// hands the visitor a POINTER to the copy. So such an arm fires for every
// ordinary value node too, after Walk has already screened that subtree, and
// re-walks it. Cost is 3*2^d-2 screen calls for depth d: a 321-byte query of
// plain nested parentheses — well inside what dslResourceGuard admits and what
// the complexity suite pins as supported — took 79 seconds and 16 GB, on input
// the guard ACCEPTS. Normalising before the walk keeps it linear and one pass.
func derefLogical(e figo.Expr) figo.Expr {
	switch v := e.(type) {
	case *figo.AndExpr:
		return figo.AndExpr{Operands: derefOperands(v.Operands)}
	case *figo.OrExpr:
		return figo.OrExpr{Operands: derefOperands(v.Operands)}
	case *figo.NotExpr:
		return figo.NotExpr{Operands: derefOperands(v.Operands)}
	case figo.AndExpr:
		return figo.AndExpr{Operands: derefOperands(v.Operands)}
	case figo.OrExpr:
		return figo.OrExpr{Operands: derefOperands(v.Operands)}
	case figo.NotExpr:
		return figo.NotExpr{Operands: derefOperands(v.Operands)}
	}
	return e
}

func derefOperands(operands []figo.Expr) []figo.Expr {
	if operands == nil {
		return nil
	}
	out := make([]figo.Expr, len(operands))
	for i, op := range operands {
		out[i] = derefLogical(op)
	}
	return out
}

// sortColumns lists a sort spec's column names, nil-safely.
func sortColumns(s *figo.OrderBy) []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Columns))
	for _, col := range s.Columns {
		names = append(names, col.Name)
	}
	return names
}

func (p *InjectionGuardPlugin) screenSort(s *figo.OrderBy) []Violation {
	var vs []Violation
	for _, name := range sortColumns(s) {
		vs = append(vs, p.screenIdent(PositionSortKey, name)...)
	}
	return vs
}

// screenSelectFields screens the projection in its RENDERED spelling.
//
// The projection is the one screened position figo converts at RENDER time
// rather than on the way in — SetSelectFields stores the caller's string
// verbatim and both SQL adapters run it through the naming func when they
// build the SELECT list. Screening the stored string instead was wrong in both
// directions: a naming func that produces a hostile name slipped through
// (SELECT `1` alongside a WHERE the same guard had refused), and a name that
// is only hostile BEFORE conversion was refused (under the default
// SnakeCaseNaming, "first-name" renders as `first_name`, and '-' is a word
// separator figo's own naming func is documented to accept).
func (p *InjectionGuardPlugin) screenSelectFields(fields map[string]bool, naming figo.NamingFunc) []Violation {
	if len(fields) == 0 {
		return nil
	}
	// Sorted so the violation list is deterministic; map order is not.
	names := make([]string, 0, len(fields))
	for name, on := range fields {
		if on {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var vs []Violation
	for _, name := range names {
		vs = append(vs, p.screenIdent(PositionSelectField, renderedName(name, naming))...)
	}
	return vs
}

// renderedName applies a naming func the way figo does: per dot-separated
// segment, leaving an empty segment empty. Mirrors normalizeFieldName in the
// core and normalizeColumnName in the raw adapter — converting the whole
// string instead folds the qualifier into the column, because SnakeCaseNaming
// treats '.' as a word separator.
func renderedName(name string, naming figo.NamingFunc) string {
	if naming == nil || name == "" {
		return name
	}
	if !strings.Contains(name, ".") {
		return naming(name)
	}
	segs := strings.Split(name, ".")
	for i, s := range segs {
		if s == "" {
			continue
		}
		segs[i] = naming(s)
	}
	return strings.Join(segs, ".")
}

// allowMatcher returns a screen that applies the syntax rule AND the
// allowlist, with the allowlist matched in both spellings.
//
// The registration is converted here rather than at AllowFields time because
// the naming func belongs to the figo instance, not to the plugin: one guard
// can be registered on instances with different naming strategies, and
// AllowFields is routinely called before any of them exists.
func (p *InjectionGuardPlugin) allowMatcher(naming figo.NamingFunc) func(Position, string) []Violation {
	p.mu.RLock()
	allowed := make(map[string]bool, len(p.allowed)*2)
	for n := range p.allowed {
		allowed[n] = true
		allowed[renderedName(n, naming)] = true
	}
	p.mu.RUnlock()

	return func(pos Position, name string) []Violation {
		if vs := p.screenIdent(pos, name); len(vs) > 0 {
			return vs
		}
		if len(allowed) == 0 {
			return nil
		}
		if allowed[name] || allowed[renderedName(name, naming)] {
			return nil
		}
		// A qualified name is allowed by its bare column too: a policy written
		// as AllowFields("city") should not have to also spell "users.city",
		// and figo emits the qualified form for a dotted DSL field.
		if i := strings.LastIndex(name, "."); i >= 0 {
			if bare := name[i+1:]; allowed[bare] || allowed[renderedName(bare, naming)] {
				return nil
			}
		}
		return []Violation{{
			Position: pos,
			Name:     name,
			Reason:   "not in the allowed field list",
		}}
	}
}

// screenIdent applies the SYNTAX rule only. It returns at most one violation
// per name so a single bad identifier does not produce a wall of text.
//
// The allowlist is deliberately NOT applied here, because this is the screen
// that runs over the clause list being rendered — and by then the list can
// contain clauses the SERVER injected rather than the caller. See AllowFields.
func (p *InjectionGuardPlugin) screenIdent(pos Position, name string) []Violation {
	p.mu.RLock()
	rule := identRule{
		allowUnicode:    p.allowUnicode,
		allowNumericSeg: p.allowNumericSegs,
		extra:           p.extraRunes,
		maxSegments:     p.maxSegments,
		maxSegmentRunes: p.maxSegmentRunes,
	}
	p.mu.RUnlock()

	if reason := rule.reason(name); reason != "" {
		return []Violation{{Position: pos, Name: name, Reason: reason}}
	}
	return nil
}

// identRule is the identifier shape policy, snapshotted so a screen never
// holds the config lock while it runs.
type identRule struct {
	allowUnicode    bool
	allowNumericSeg bool
	extra           string
	maxSegments     int
	maxSegmentRunes int
}

// reason returns why name cannot be a column identifier, or "" if it can be.
//
// One to maxSegments dot-separated segments; each segment 1..maxSegmentRunes
// RUNES of letters, digits, '_', '$' and any AllowExtraRunes character; no
// segment composed solely of digits.
//
// "Not solely digits" is the load-bearing clause and the reported bug's exact
// shape: `1=1` filters on a column named `1`, which no engine has. A leading
// digit on its own is fine — MySQL allows `2fa_enabled`, and refusing it broke
// real schemas for no security gain.
func (r identRule) reason(name string) string {
	if name == "" {
		return "empty identifier"
	}
	if !utf8.ValidString(name) {
		return "not valid UTF-8"
	}

	segments := strings.Split(name, ".")
	if len(segments) > r.maxSegments {
		return fmt.Sprintf("has %d dot-separated segments, at most %d allowed",
			len(segments), r.maxSegments)
	}

	for _, seg := range segments {
		if seg == "" {
			return "has an empty dot-separated segment"
		}
		if n := utf8.RuneCountInString(seg); n > r.maxSegmentRunes {
			return fmt.Sprintf("segment %q is %d characters, at most %d allowed",
				seg, n, r.maxSegmentRunes)
		}
		for _, c := range seg {
			if !r.partOK(c) {
				return fmt.Sprintf("segment %q contains %s, which cannot appear in a column name",
					seg, describeRune(c))
			}
		}
		if !r.allowNumericSeg && allDigits(seg) {
			return fmt.Sprintf("segment %q is only digits, which names no column", seg)
		}
	}
	return ""
}

func (r identRule) partOK(c rune) bool {
	if c == '_' || c == '$' {
		return true
	}
	// Extra runes never reach past the hard floor: a character that can
	// terminate or comment a statement, or that no log line should carry, is
	// refused however the guard is configured.
	if c >= 0x20 && c != 0x7f && c != '`' && c != '"' && c != '\'' && c != ';' &&
		strings.ContainsRune(r.extra, c) {
		return true
	}
	if r.allowUnicode {
		return unicode.IsLetter(c) || unicode.IsDigit(c)
	}
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func allDigits(seg string) bool {
	for _, c := range seg {
		if !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

// describeRune names a rune in a message without ever emitting a raw control
// byte, a zero-width character or a bidi override into a log line — printing
// those verbatim is how a log viewer gets attacked by the thing being logged.
func describeRune(r rune) string {
	switch {
	case r < 0x20 || r == 0x7f:
		return fmt.Sprintf("control byte %#02x", r)
	case r <= unicode.MaxASCII:
		return fmt.Sprintf("%q", r)
	// Invisibles are classified BEFORE printability: a combining mark IS
	// printable by unicode.IsPrint, so leaving it to the %q arm below would put
	// a character with no glyph of its own into the message, where it lands on
	// whatever precedes it in the log viewer.
	case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return fmt.Sprintf("invisible character U+%04X", r)
	case !unicode.IsPrint(r):
		return fmt.Sprintf("non-printing character U+%04X", r)
	default:
		return fmt.Sprintf("%q (U+%04X)", r, r)
	}
}

// screenJSONPath screens JsonPathExpr.Path, which is a JSONPath expression
// rather than a column name, so the column rule does not apply to it.
//
// It is rendered only by Mongo and Elasticsearch, where the result is a
// BSON/JSON object key and structurally cannot be broken out of, so this
// refuses only what is never legitimate in a path and is a sign of something
// else being attempted: control bytes and the SQL quote runes.
func screenJSONPath(path string) []Violation {
	if path == "" {
		return nil
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || r == '`' || r == '"' || r == '\'' {
			return []Violation{{
				Position: PositionJSONPath,
				Name:     path,
				Reason:   fmt.Sprintf("contains %s", describeRune(r)),
			}}
		}
	}
	return nil
}

// pruneSort re-sets the sort to the columns that passed the screen. Called
// only on the rejection path, from FinalizeClauses, where SetSort writes the
// render and leaves the caller's request alone.
func (p *InjectionGuardPlugin) pruneSort(f figo.Figo, s *figo.OrderBy) {
	if s == nil || len(s.Columns) == 0 {
		return
	}
	kept := make([]figo.OrderByColumn, 0, len(s.Columns))
	for _, col := range s.Columns {
		if len(p.screenIdent(PositionSortKey, col.Name)) == 0 {
			kept = append(kept, col)
		}
	}
	if len(kept) == len(s.Columns) {
		return
	}
	if len(kept) == 0 {
		f.SetSort(nil)
		return
	}
	f.SetSort(&figo.OrderBy{Columns: kept})
}

// pruneSelectFields re-sets the projection to the columns that passed the
// screen.
//
// Dropping the last surviving column leaves an empty set, which means SELECT *
// — normally a widening, and the reason FieldsPlugin refuses to do it. Here it
// is safe and is the right answer: this only runs when the query is already
// being replaced by a never-true clause, so no row can come back through the
// widened projection, and the alternative is emitting SELECT `1` and handing
// the engine the very identifier we are refusing.
func (p *InjectionGuardPlugin) pruneSelectFields(f figo.Figo, fields map[string]bool, naming figo.NamingFunc) {
	if len(fields) == 0 {
		return
	}
	kept := make([]string, 0, len(fields))
	dropped := false
	for name, on := range fields {
		if !on {
			continue
		}
		// Screened in the rendered spelling, kept in the caller's: the
		// projection is stored unconverted and the adapter converts it again.
		if len(p.screenIdent(PositionSelectField, renderedName(name, naming))) == 0 {
			kept = append(kept, name)
		} else {
			dropped = true
		}
	}
	if !dropped {
		return
	}
	sort.Strings(kept)
	f.SetSelectFields(kept...)
}

func (p *InjectionGuardPlugin) rejectDiagnosticsEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rejectDiagnostics
}

func (p *InjectionGuardPlugin) screenCustomEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.screenCustom
}

// dedupeViolations collapses repeats. `1=1 or 1=1` is two nodes and one
// mistake, and the same identifier is often reached twice (a preload condition
// is screened by both AfterParse and FinalizePreloads).
func dedupeViolations(vs []Violation) []Violation {
	if len(vs) < 2 {
		return vs
	}
	seen := make(map[Violation]bool, len(vs))
	out := vs[:0]
	for _, v := range vs {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------------------
// latch
// ---------------------------------------------------------------------------

// The latch records WHY an instance was refused, for Rejected. It is
// reporting only — the query is kept closed by core, which replaces the clause
// list with the never-true clause when a parse hook rejects (see figo's
// refuseDSL), so nothing here is load bearing for the fail-closed property.
//
// That distinction is what makes keying it by the instance correct. An earlier
// version made the latch the sole carrier of the refusal and keyed it by the
// instance's plugin manager so that a Clone would inherit it — which meant a
// sibling clone parsing a CLEAN DSL cleared the verdict for every refused
// sibling, and those siblings, whose DSL had already been rolled away, rendered
// completely unfiltered. Under a clone-per-request template roughly a third of
// refused requests escaped that way. A reporting-only latch has no such duty:
// it may miss, and the query stays closed regardless.

func (p *InjectionGuardPlugin) latch(f figo.Figo, vs []Violation) {
	if f == nil {
		return
	}
	p.lmu.Lock()
	defer p.lmu.Unlock()
	if p.cur == nil {
		p.cur = make(map[figo.Figo][]Violation)
	}
	if p.maxLatched > 0 && len(p.cur) >= p.maxLatched {
		p.prev = p.cur
		p.cur = make(map[figo.Figo][]Violation)
	}
	p.cur[f] = append([]Violation(nil), vs...)
	delete(p.prev, f)
}

// latched returns a COPY of the violations held against f.
//
// The copy is not defensive tidiness, it is required. Both finalizer hooks do
// `vs := p.latched(f)` and then append the identifiers they screen; handing
// back the stored slice would let that append write into the latch's own
// backing array whenever it had spare capacity — and FinalizeClauses and
// FinalizePreloads run on the same instance in the same Build, so two writers
// would share it. Rejected hands the copy to the caller for the same reason.
func (p *InjectionGuardPlugin) latched(f figo.Figo) []Violation {
	if f == nil {
		return nil
	}
	p.lmu.Lock()
	defer p.lmu.Unlock()
	if vs, ok := p.cur[f]; ok {
		return append([]Violation(nil), vs...)
	}
	// A hit in the older generation is promoted, so an instance that keeps
	// being built does not fall off the end of the table while it is in use.
	if vs, ok := p.prev[f]; ok {
		if p.cur == nil {
			p.cur = make(map[figo.Figo][]Violation)
		}
		p.cur[f] = vs
		delete(p.prev, f)
		return append([]Violation(nil), vs...)
	}
	return nil
}

func (p *InjectionGuardPlugin) clear(f figo.Figo) {
	if f == nil {
		return
	}
	p.lmu.Lock()
	defer p.lmu.Unlock()
	delete(p.cur, f)
	delete(p.prev, f)
}

// Compile-time proof that the guard implements the three hooks it documents.
var (
	_ figo.Plugin           = (*InjectionGuardPlugin)(nil)
	_ figo.ClauseFinalizer  = (*InjectionGuardPlugin)(nil)
	_ figo.PreloadFinalizer = (*InjectionGuardPlugin)(nil)
)
