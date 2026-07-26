// Package doccontract pins the behaviour README.md and PLUGIN_SYSTEM_GUIDE.md
// assert, and pins the removal of the assertions that ef28331 made false.
//
// Hunt #8 found three findings in the docs area (A10-4, R3, ES-8) that share one
// root cause: ef28331 changed four documented contracts and shipped without the
// documentation, so the docs and the code contradicted each other inside a single
// commit. Nothing in the repo could notice, because no test had ever read the
// docs and no test pinned the contracts they describe.
//
// The two halves below are deliberately different in kind:
//
//   - TestDocsDoNotClaim* read the markdown and fail if a claim ef28331 falsified
//     is still there. These are the actual regression tests for the three
//     findings: each one fails at ef28331 and passes after the doc edit.
//   - TestContract* pin the CODE behaviour the docs now describe, so the next
//     behaviour change breaks a test instead of silently invalidating a document.
//     They pass at ef28331 too — the code was already right; it was the prose that
//     was wrong — so they are pins, not regression tests, and they are what make
//     the next round notice.
//
// The pins are written at the level of the guarantee, not the literal output. The
// Elasticsearch pagination one asserts the INVARIANT (from + size never exceeds
// the result window) rather than "size is 9995 at offset 5", because a further
// pagination fix is in flight for the past-the-window case and any correct version
// of it must still satisfy the invariant.
package doccontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	// examples/doccontract -> repo root
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// --------------------------------------------------------------------------
// The regression tests: claims ef28331 falsified must be gone.
// --------------------------------------------------------------------------

// TestA10_4_DocsDoNotClaimNilPluginManager covers A10-4. ef28331 made
// GetPluginManager lazily construct its manager (figo.go) while, in the same
// commit, documenting that it returns nil (README + guide) and telling the reader
// to nil-check.
func TestA10_4_DocsDoNotClaimNilPluginManager(t *testing.T) {
	banned := []string{
		"// nil until the first RegisterPlugin/SetPluginManager",
		"`GetPluginManager()` returns `nil` on an instance",
		"**`GetPluginManager()` returns `nil` on an instance",
		"// Get plugin manager (nil if no plugin has ever been registered)",
		"if manager == nil {",
	}
	for _, doc := range []string{"README.md", "PLUGIN_SYSTEM_GUIDE.md"} {
		body := readDoc(t, doc)
		for _, b := range banned {
			if strings.Contains(body, b) {
				t.Errorf("%s still documents the nil GetPluginManager contract: %q", doc, b)
			}
		}
		if !strings.Contains(body, "never") {
			t.Errorf("%s does not state the never-nil contract", doc)
		}
	}
}

// TestR3_ReadmeDoesNotClaimPreloadJoin covers R3. ef28331 deleted the raw
// adapter's preload JOIN (hunt #6 H10) and declared it a documented behaviour
// change, but README still described the JOIN as the adapter's behaviour.
func TestR3_ReadmeDoesNotClaimPreloadJoin(t *testing.T) {
	body := readDoc(t, "README.md")
	banned := []string{
		"`load=` preloads render into the full SELECT as `JOIN <table> ON <filter>` clauses",
		"preloads render into the full SELECT",
	}
	for _, b := range banned {
		if strings.Contains(body, b) {
			t.Errorf("README still documents the deleted preload JOIN: %q", b)
		}
	}
	if !strings.Contains(body, "contributes nothing to the raw adapter's SELECT") {
		t.Error("README does not state that load= contributes nothing to the raw SELECT")
	}
}

// TestES8_ReadmeDoesNotClaimFlatSizeCap covers the primary half of ES-8: the
// claim that take:0 renders a flat "size": 10000, false at every offset >= 1
// since ef28331 clamped it to the result window.
func TestES8_ReadmeDoesNotClaimFlatSizeCap(t *testing.T) {
	body := readDoc(t, "README.md")
	if strings.Contains(body, "renders an explicit `\"size\": 10000` (the `index.max_result_window`\ndefault) for `take:0`") {
		t.Error("README still claims a flat size:10000 for take:0")
	}
	if !strings.Contains(body, "max_result_window - from") {
		t.Error("README does not document the from-aware size clamp")
	}
}

// TestES8_ReadmeDocumentsFailClosedContract covers the second half of ES-8: the
// omission. ef28331 changed the ES adapter's (string, bool) contract so a failed
// render reports ok=true with a match_none body, and nothing said so.
func TestES8_ReadmeDocumentsFailClosedContract(t *testing.T) {
	body := readDoc(t, "README.md")
	for _, want := range []string{
		"match_none",
		"`BuildElasticsearchQuery`",
		"Checking `if q == nil` or `if !ok` will therefore never fire.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("README does not document the ES fail-closed contract: missing %q", want)
		}
	}
}

// --------------------------------------------------------------------------
// The pins: the code behaviour the docs now describe.
// --------------------------------------------------------------------------

// TestContractPluginManagerNeverNil pins the contract the docs now state. It is
// deliberately blind to HOW the manager is produced — hunt #8 also removed the
// getter's build-time side effect, which must not change the observable answer.
func TestContractPluginManagerNeverNil(t *testing.T) {
	cases := map[string]func() figo.Figo{
		"fresh":          func() figo.Figo { return figo.New() },
		"clone of fresh": func() figo.Figo { return figo.New().Clone() },
		"after SetPluginManager nil": func() figo.Figo {
			f := figo.New()
			f.SetPluginManager(nil)
			return f
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			pm := mk().GetPluginManager()
			if pm == nil {
				t.Fatal("GetPluginManager returned nil; the docs promise a usable manager")
			}
			// The docs tell readers to use this instead of a nil check, so it has to
			// be safe to call and has to report "no plugins".
			if got := len(pm.ListPlugins()); got != 0 {
				t.Errorf("ListPlugins() = %d, want 0 on a plugin-free instance", got)
			}
			if _, ok := pm.GetPlugin("nope"); ok {
				t.Error("GetPlugin found a plugin on a plugin-free instance")
			}
		})
	}
}

// TestContractRawPreloadNotInSelect pins R3: load= must not reach the rendered
// SELECT on any raw path, and BuildRawPreloads must still expose the fragment.
func TestContractRawPreloadNotInSelect(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`id>0 load=[Orders:total>100]`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.RawAdapter{})

	sql, args, err := adapters.BuildRawSelect(f, "users")
	if err != nil {
		t.Fatalf("BuildRawSelect: %v", err)
	}
	if strings.Contains(sql, "JOIN") || strings.Contains(sql, "Orders") || strings.Contains(sql, "total") {
		t.Errorf("preload leaked into the raw SELECT: %q", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want just the id bound value (no preload args)", args)
	}

	// Including the JOIN segment must not resurrect it either.
	seg, ok := adapters.RawAdapter{}.GetSqlString(f, adapters.RawContext{Table: "users"}, "SELECT", "FROM", "JOIN", "WHERE")
	if !ok {
		t.Fatal("segment render failed")
	}
	if strings.Contains(seg, "JOIN") {
		t.Errorf("the JOIN segment emitted something: %q", seg)
	}

	// ...and the documented escape hatch still works.
	pre, err := adapters.BuildRawPreloads(f)
	if err != nil {
		t.Fatalf("BuildRawPreloads: %v", err)
	}
	p, found := pre["Orders"]
	if !found {
		t.Fatal(`BuildRawPreloads lost the "Orders" relation; the README names it as the only route to the predicate`)
	}
	if !strings.Contains(p.Where, "total") || len(p.Args) != 1 {
		t.Errorf("BuildRawPreloads[Orders] = %+v, want the rendered total predicate and its arg", p)
	}
}

// TestContractESFailClosedIsOkTrue pins the ES-8 fail-signal contract: the
// (string, bool) surface must report ok=true with a match_none body, and the error
// must stay reachable on the channels the README names.
func TestContractESFailClosedIsOkTrue(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`a=1 load=[Orders:total>100]`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(adapters.ElasticsearchAdapter{})

	s, ok := adapters.ElasticsearchAdapter{}.GetSqlString(f, nil)
	if !ok {
		t.Error("GetSqlString reported ok=false; the README promises ok=true with a fail-closed body")
	}
	if !strings.Contains(s, "match_none") {
		t.Errorf("GetSqlString body = %q, want match_none", s)
	}
	q, ok := adapters.ElasticsearchAdapter{}.GetQuery(f, nil)
	if !ok || q == nil {
		t.Errorf("GetQuery ok=%v nil=%v; the README promises a concrete wrapper", ok, q == nil)
	}
	// An EMPTY body would be match_all on Elasticsearch — the hazard the fix exists
	// to close. Never acceptable, whatever else changes.
	if strings.TrimSpace(f.GetSqlString(nil)) == "" {
		t.Fatal("figo.GetSqlString returned an empty ES body, which means match_all")
	}
	if _, err := adapters.BuildElasticsearchQuery(f); err == nil {
		t.Error("BuildElasticsearchQuery returned no error; the README names it as an error channel")
	}
	if _, err := adapters.GetElasticsearchQueryString(f); err == nil {
		t.Error("GetElasticsearchQueryString returned no error; the README names it as an error channel")
	}
}

// TestContractESPaginationFitsResultWindow pins the ES-8 pagination INVARIANT
// rather than the literal sizes: whatever the adapter renders for take:0, ES
// validates from+size against index.max_result_window, so the sum must never
// exceed it. Written this way on purpose — a further fix to the past-the-window
// case is in flight, and any correct version of it still satisfies this.
func TestContractESPaginationFitsResultWindow(t *testing.T) {
	const window = 10000
	for _, skip := range []int{0, 1, 5, 99, 9999} {
		f := figo.New()
		if err := f.AddFiltersFromString("a=1 page=skip:" + itoa(skip) + ",take:0"); err != nil {
			t.Fatalf("skip=%d: AddFiltersFromString: %v", skip, err)
		}
		f.Build(adapters.ElasticsearchAdapter{})
		body, err := adapters.GetElasticsearchQueryStringCompact(f)
		if err != nil {
			t.Fatalf("skip=%d: %v", skip, err)
		}
		var doc struct {
			From *int `json:"from"`
			Size *int `json:"size"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("skip=%d: unmarshalling %q: %v", skip, body, err)
		}
		from := 0
		if doc.From != nil {
			from = *doc.From
		}
		if doc.Size == nil {
			t.Fatalf("skip=%d: no explicit size in %q — ES would default to 10 hits", skip, body)
		}
		if from+*doc.Size > window {
			t.Errorf("skip=%d: from+size = %d+%d = %d exceeds index.max_result_window %d (%q)",
				skip, from, *doc.Size, from+*doc.Size, window, body)
		}
		if from != skip {
			t.Errorf("skip=%d: rendered from=%d", skip, from)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
