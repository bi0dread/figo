# figo playground

**Live demo: <https://bi0dread.github.io/figo/>**

An interactive, drag-and-drop demo of the [figo](https://github.com/bi0dread/figo) DSL, built
with [React Flow](https://reactflow.dev/). Drag operation blocks onto the canvas, wire them
into the **Query** node, and the resulting DSL string is generated live.

```
npm install
npm run dev      # http://localhost:5173
npm run build    # static bundle in dist/
```

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
