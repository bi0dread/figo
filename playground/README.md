# figo playground

**Live demo: <https://bi0dread.github.io/figo/>**

An interactive, drag-and-drop demo of the [figo](https://github.com/bi0dread/figo) DSL, built
with [React Flow](https://reactflow.dev/). Drag operation blocks onto the canvas, wire them
into the **Query** node, and the resulting DSL string is generated live.

```
npm install
npm run dev      # http://localhost:5173
npm run build    # static bundle in dist/
npm run test:preview # live DSL/table regression checks
```

## Live data examples

Below the canvas, the live data preview parses the generated DSL string and applies
it to 18 sample customer accounts and their orders. Six presets demonstrate nested
conditions, active customers, revenue and date ranges, case-insensitive name search,
pagination, and filtered Orders preloads. Loading a preset also updates the canvas.

Edit the preview's DSL to try your own filters, or enable **Follow visual builder**
to resume using the canvas output. **All source rows** shows the original dataset
with badges for matching, excluded, and paginated-out rows. Expand a row's Orders
button to compare the original orders with the query's filtered preload.

This is a browser-side demonstration, not a database execution engine. Comparisons
are typed, NULL predicates follow SQL three-valued logic, and sorting places NULLs
last. Regex operators and relations other than Orders report an explicit error.
Backend collation, date coercion, and NULL ordering can differ. The Go adapters
remain authoritative for actual database queries.

## Blocks

| Block | Emits | Notes |
|-------|-------|-------|
| Condition | `field<op>value` | All figo operators: `=` `!=` `>` `>=` `<` `<=` `=^` `!=^` `.=^` `=~` `!=~` `<in>` `<nin>` `<bet>` `<null>` `<notnull>` |
| AND / OR | `(a and b)` / `(a or b)` | Accept any number of inputs; operands merge top → bottom |
| NOT | `not x` | Single input |
| Sort | `sort=field:dir` | Multiple sort nodes merge into one directive, top → bottom |
| Page | `page=skip:N,take:N` | One per query |
| Load | `load=[Rel:filter]` | Has its own `filter` input handle — wire any expression subtree into it |

Values follow figo's typing rules: numbers, booleans, `null`, and ISO dates stay bare;
everything else is double-quoted. Wrap a value in quotes yourself (`"0123"`) to force string
typing. Nodes that don't reach the Query node are dimmed and excluded from the DSL.

The DSL emitter here is an independent TypeScript re-implementation
(`src/dsl.ts`) — figo's Go code never runs in the browser. It is cross-checked
automatically in CI: the "DSL emitter vs the Go parser" step of
`.github/workflows/playground-ci.yml` enumerates the emitter's node/value
space and feeds every emitted string through the real Go parser and all four
adapters (`examples/dslcheck`), so a divergence fails the build instead of a
user's query. The `=~` regex operator is exempt from the cross-check, and
wherever the two implementations could ever disagree, the Go parser is
authoritative.
