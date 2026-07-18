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

The generated strings are validated against the actual Go parser (`figo.AddFiltersFromString`
+ the raw SQL adapter), including operator coverage, nested groups, preload filters, and the
list/range/null forms.
