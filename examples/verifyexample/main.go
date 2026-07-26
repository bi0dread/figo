// Command verifyexample checks that examples/example_usage.go still produces
// what it claims.
//
// example_usage.go is the one end-to-end artifact a new user copies, and until
// now CI only COMPILED it — it was never executed and nothing asserted on its
// output. Compiling is close to no coverage at all here, because every failure
// mode this program has is a runtime one: the Elasticsearch adapter reports a
// render it could not perform as ok=true with a fail-closed
// `{"query":{"match_none":{}}}` body, so a broken example still prints nine
// tidy-looking documents and "All examples completed!".
//
// verifyexample reads the example's stdout and asserts on the CLAIMS the example
// makes, not on its bytes. A golden file would be stronger but also wrong for
// this job: it would go red for any unrelated formatting change to an adapter,
// and this repo has adapters under active change. Every assertion below is a
// claim the example's own printed text makes about itself.
//
// Usage (see .github/workflows/test.yml):
//
//	go run ./examples/ | go run ./examples/verifyexample
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// check is one assertion about the example's output.
type check struct {
	// what the example claims, in the reviewer's words
	claim string
	// every substring that must be present for the claim to hold
	want []string
	// substrings that must NOT be present
	reject []string
}

var checks = []check{
	{
		claim: "prints its banner and reaches the end without log.Fatal",
		want: []string{
			"figo Elasticsearch Adapter Usage Examples",
			"All examples completed!",
		},
	},
	{
		claim: "Example 1 renders a term query for status=active",
		want:  []string{"Example 1: Basic Term Query", `"term"`, `"status": "active"`},
	},
	{
		claim: "Example 2 renders both range bounds under one bool/must",
		want:  []string{"Example 2: Range Query", `"bool"`, `"must"`, `"gt": 25`, `"gte": 80`},
	},
	{
		claim: "Example 3 translates the SQL LIKE wildcards to ES wildcards",
		want:  []string{"Example 3: Wildcard Search", `"wildcard"`, `"*gmail*"`},
	},
	{
		claim: "Example 4 renders <in> as a terms query carrying all three members",
		want:  []string{"Example 4: Terms Query", `"terms"`, `"tech"`, `"business"`, `"finance"`},
	},
	{
		claim: "Example 5 renders <bet> as a two-sided range",
		want:  []string{"Example 5: Between Query", `"range"`, `"gte": 100`, `"lte": 500`},
	},
	{
		claim: "Example 6 keeps the nested groups nested",
		want:  []string{"Example 6: Complex Nested Query", `"should"`, `"must"`, `"minimum_should_match"`},
	},
	{
		claim: "Example 7 honours sort= priority order and page=take:5",
		// The sort keys must appear in the order the DSL named them; asserting the
		// substring "score" before "age" is what pins priority order, so this claim
		// is checked positionally below rather than by presence alone.
		want: []string{"Example 7: Pagination and Sorting", `"sort"`, `"order": "desc"`, `"size": 5`},
	},
	{
		claim: "Example 8's fluent builder emits the explicit size and _source it was given",
		want:  []string{"Example 8: Fluent Builder", `"size": 10`, `"_source"`, `"email"`},
	},
	{
		claim: "Example 9 narrows _source to the four selected fields",
		want:  []string{"Example 9: Field Selection", `"_source"`, `"id"`, `"name"`, `"score"`},
	},
	{
		claim: "no example silently failed closed",
		// match_none is the adapter's fail-closed body and `"query": null` was the
		// pre-fix zero-struct marshal. Either one appearing means a query the
		// adapter could not render was printed as though it had rendered — the exact
		// failure the example's discarded error returns used to hide.
		reject: []string{"match_none", `"query": null`},
	},
	{
		claim: "every example prints a query, not an empty body",
		want:  []string{"Generated Query:"},
		// An empty ES search body is match_all — it must never be what the example
		// shows a reader.
		reject: []string{"Generated Query:\n{}", "Generated Query:\n\n"},
	},
}

// section returns the slice of out between the first occurrence of start and the
// next occurrence of end (to the end of out when end is absent).
func section(out, start, end string) (string, bool) {
	i := strings.Index(out, start)
	if i < 0 {
		return "", false
	}
	rest := out[i:]
	if j := strings.Index(rest, end); j > 0 {
		return rest[:j], true
	}
	return rest, true
}

func main() {
	raw, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifyexample: reading stdin: %v\n", err)
		os.Exit(2)
	}
	out := string(raw)
	if strings.TrimSpace(out) == "" {
		fmt.Fprintln(os.Stderr, "verifyexample: nothing on stdin — the example did not run")
		os.Exit(2)
	}

	var failed []string
	for _, c := range checks {
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				failed = append(failed, fmt.Sprintf("%s\n      missing: %q", c.claim, w))
			}
		}
		for _, r := range c.reject {
			if strings.Contains(out, r) {
				failed = append(failed, fmt.Sprintf("%s\n      present but must not be: %q", c.claim, r))
			}
		}
	}

	// Example 7's claim is about ORDER, which no substring test can express. The
	// search has to be scoped to Example 7's own section: `"age": {` also appears in
	// Example 2's range query, several hundred bytes earlier.
	if sec, ok := section(out, "Example 7:", "Example 8:"); !ok {
		failed = append(failed, "Example 7 honours sort= priority order\n      could not locate Example 7's output section")
	} else if i, j := strings.Index(sec, `"score": {`), strings.Index(sec, `"age": {`); i < 0 || j < 0 || i > j {
		failed = append(failed, fmt.Sprintf(
			"Example 7 honours sort= priority order (sort=score:desc,age:asc)\n      expected the score sort key before the age one (score at %d, age at %d within the section)", i, j))
	}

	// All nine example headers, in the order the program prints them.
	prev := -1
	for n := 1; n <= 9; n++ {
		at := strings.Index(out, fmt.Sprintf("Example %d:", n))
		if at < 0 {
			failed = append(failed, fmt.Sprintf("the example advertises nine examples\n      Example %d is missing", n))
			continue
		}
		if at < prev {
			failed = append(failed, fmt.Sprintf("the example advertises nine examples in order\n      Example %d appears out of order", n))
		}
		prev = at
	}

	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "verifyexample: FAIL — %d unmet claim(s) in examples/example_usage.go\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(os.Stderr, "\n  - %s\n", f)
		}
		fmt.Fprintln(os.Stderr, "\nThe example is what a new user copies. Either the example or the behaviour it documents is wrong.")
		os.Exit(1)
	}
	fmt.Printf("verifyexample: PASS — %d claims held over %d bytes of output\n", len(checks), len(raw))
}
