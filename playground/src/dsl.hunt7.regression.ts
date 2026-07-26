/*
 * Regression tests for the hunt #7 playground findings.
 *
 * Deliberately dependency-free: it uses no `node:` imports and no test runner,
 * so `tsc --noEmit` (the existing `npm run build`) type-checks it with the
 * packages already in package.json. To execute it, compile with the tsc that is
 * already a devDependency and run the output — this is what Playground CI does:
 *
 *     npx tsc src/dsl.hunt7.regression.ts --outDir .tsbuild \
 *         --target es2022 --module esnext --moduleResolution bundler --skipLibCheck
 *     node .tsbuild/dsl.hunt7.regression.js
 *
 * A failed check throws, which is a non-zero exit.
 */
// `./dsl.js` (not `./dsl`) so the emitted ES module is resolvable by Node.
import { generateDsl, type AppNode } from './dsl.js'
import type { Edge } from '@xyflow/react'

let edgeSeq = 0
const edge = (source: string, target: string, targetHandle: string): Edge => ({
  id: `e${edgeSeq++}`,
  source,
  target,
  targetHandle,
})

const root = (): AppNode => ({ id: 'root', type: 'root', position: { x: 900, y: 0 }, data: {} })
const cond = (
  id: string,
  field: string,
  op: string,
  value = '',
  value2 = '',
  y = 0,
): AppNode => ({ id, type: 'condition', position: { x: 0, y }, data: { field, op, value, value2 } })
const logic = (id: string, kind: string, y = 0): AppNode => ({
  id,
  type: 'logic',
  position: { x: 300, y },
  data: { kind },
})
const sortNode = (id: string, field: string, dir = 'asc'): AppNode => ({
  id,
  type: 'sort',
  position: { x: 300, y: 0 },
  data: { field, dir },
})
const pageNode = (id: string, skip: number, take: number): AppNode => ({
  id,
  type: 'page',
  position: { x: 300, y: 0 },
  data: { skip, take },
})

let failures = 0
let checks = 0
function check(name: string, ok: boolean, detail: string): void {
  checks++
  if (!ok) {
    failures++
    console.error(`FAIL ${name}: ${detail}`)
  }
}
const eq = (name: string, got: unknown, want: unknown): void =>
  check(name, got === want, `got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`)
const hasWarning = (name: string, warnings: string[], needle: string): void =>
  check(
    name,
    warnings.some((w) => w.includes(needle)),
    `no warning containing ${JSON.stringify(needle)} in ${JSON.stringify(warnings)}`,
  )

// ---------------------------------------------------------------- A7-3
// A quoted <in> item containing a comma must survive regardless of the spacing
// around the separating comma. The old regex alternation only entered its
// quoted branch when a `"` sat at the scan index, so `a, "b,c", d` was torn
// into four values.
{
  const cases: [string, string][] = [
    ['a,"b,c",d', 'city<in>["a","b,c","d"]'],
    ['a, "b,c", d', 'city<in>["a","b,c","d"]'],
    ['Boston, "New York, NY", Austin', 'city<in>["Boston","New York, NY","Austin"]'],
    ['"New York, NY", Boston', 'city<in>["New York, NY","Boston"]'],
    ['Austin, Boston', 'city<in>["Austin","Boston"]'],
  ]
  for (const [value, want] of cases) {
    const r = generateDsl([root(), cond('c1', 'city', '<in>', value)], [edge('c1', 'root', 'filter')])
    eq(`A7-3 splitList ${JSON.stringify(value)}`, r.dsl, want)
    eq(`A7-3 no spurious warning for ${JSON.stringify(value)}`, r.warnings.length, 0)
  }
  const unclosed = generateDsl(
    [root(), cond('c1', 'city', '<in>', 'a, "b,c')],
    [edge('c1', 'root', 'filter')],
  )
  hasWarning('A7-3 unclosed quote is reported', unclosed.warnings, 'Unclosed double quote')
}

// ---------------------------------------------------------------- A7-4
// A condition on a column literally named sort/page/load cannot be expressed
// when the operator starts with `=`; the parser reads it as a directive and
// drops both the predicate and the connector beside it.
{
  for (const field of ['sort', 'page', 'load']) {
    for (const op of ['=', '=^', '=~']) {
      const r = generateDsl([root(), cond('c1', field, op, 'active')], [edge('c1', 'root', 'filter')])
      eq(`A7-4 ${field}${op} emits nothing`, r.dsl, '')
      eq(`A7-4 ${field}${op} node is dimmed`, r.active.has('c1'), false)
      hasWarning(`A7-4 ${field}${op} warns`, r.warnings, `would be read as the ${field}= directive`)
    }
    // Operators that do NOT put an `=` straight after the name are ordinary
    // conditions on the Go side and must keep working.
    for (const op of ['!=', '>=', '<=', '.=^', '<null>']) {
      const r = generateDsl([root(), cond('c1', field, op, 'active')], [edge('c1', 'root', 'filter')])
      check(`A7-4 ${field}${op} still emitted`, r.dsl.startsWith(`${field}${op}`), `dsl=${r.dsl}`)
    }
  }
  // Directive matching is case-sensitive on the Go side.
  const upper = generateDsl([root(), cond('c1', 'Sort', '=', 'active')], [edge('c1', 'root', 'filter')])
  eq('A7-4 Sort= is an ordinary field', upper.dsl, 'Sort="active"')
  // The neighbouring connector must not be emitted for a dropped condition.
  const combined = generateDsl(
    [root(), logic('a1', 'and'), cond('c1', 'sort', '=', 'active', '', 10), cond('c2', 'name', '=', 'bob', '', 20)],
    [edge('c1', 'a1', 'in'), edge('c2', 'a1', 'in'), edge('a1', 'root', 'filter')],
  )
  eq('A7-4 AND drops only the unexpressible operand', combined.dsl, 'name="bob"')
}

// ---------------------------------------------------------------- A7-5
// A <bet> with one bound blank has no honest rendering: `(10..)` builds an
// executable query with an empty-string upper bound and `(..600)` is rejected
// by the parser so the predicate vanishes entirely.
{
  for (const [lo, hi] of [['10', ''], ['', '600'], ['', '']]) {
    const r = generateDsl([root(), cond('c1', 'price', '<bet>', lo, hi)], [edge('c1', 'root', 'filter')])
    eq(`A7-5 (${lo}..${hi}) emits nothing`, r.dsl, '')
    eq(`A7-5 (${lo}..${hi}) node is dimmed`, r.active.has('c1'), false)
    hasWarning(`A7-5 (${lo}..${hi}) warns`, r.warnings, 'needs both bounds')
  }
  const ok = generateDsl([root(), cond('c1', 'price', '<bet>', '10', '600')], [edge('c1', 'root', 'filter')])
  eq('A7-5 both bounds still work', ok.dsl, 'price<bet>(10..600)')
  eq('A7-5 both bounds warn-free', ok.warnings.length, 0)
}

// ---------------------------------------------------------------- A7-8
// figo normalises a negative skip/take to 0 with no diagnostic, so `take:-3`
// silently means "no row limit". Clamp in the emitter and say so.
{
  const neg = generateDsl(
    [root(), cond('c1', 'name', '<notnull>'), pageNode('p1', -5, -3)],
    [edge('c1', 'root', 'filter'), edge('p1', 'root', 'page')],
  )
  eq('A7-8 negative page clamped', neg.dsl, 'name<notnull> page=skip:0,take:0')
  hasWarning('A7-8 negative page warns', neg.warnings, 'cannot be negative')
  const pos = generateDsl(
    [root(), cond('c1', 'name', '<notnull>'), pageNode('p1', 5, 2)],
    [edge('c1', 'root', 'filter'), edge('p1', 'root', 'page')],
  )
  eq('A7-8 non-negative page untouched', pos.dsl, 'name<notnull> page=skip:5,take:2')
  eq('A7-8 non-negative page warn-free', pos.warnings.length, 0)
}

// ------------------------------------------------- CRITIQUE 1a (NOT arity)
// A NOT node fed more than one input keeps only the top-most, so dragging a
// node vertically changes the emitted query. It used to do that silently while
// every other drop in the same function warned.
{
  const nodesAt = (y2: number): AppNode[] => [
    root(),
    logic('n1', 'not'),
    cond('c1', 'deleted', '=', 'true', '', 10),
    cond('c2', 'tenant_id', '=', 'acme', '', y2),
  ]
  const wires = [edge('c1', 'n1', 'in'), edge('c2', 'n1', 'in'), edge('n1', 'root', 'filter')]
  const a = generateDsl(nodesAt(20), wires)
  const b = generateDsl(nodesAt(1), wires)
  check('CRITIQUE-1a Y position still decides', a.dsl !== b.dsl, `a=${a.dsl} b=${b.dsl}`)
  hasWarning('CRITIQUE-1a extra NOT input warns (a)', a.warnings, 'extra connection')
  hasWarning('CRITIQUE-1a extra NOT input warns (b)', b.warnings, 'extra connection')
  const single = generateDsl(
    [root(), logic('n1', 'not'), cond('c1', 'deleted', '=', 'true')],
    [edge('c1', 'n1', 'in'), edge('n1', 'root', 'filter')],
  )
  eq('CRITIQUE-1a single input warn-free', single.warnings.length, 0)
  eq('CRITIQUE-1a single input renders', single.dsl, 'not deleted=true')
}

// --------------------------------------- CRITIQUE 1a (filter silently lost)
// A cycle, or an AND whose children are all invalid, leaves the root filter
// null while sort=/page= still render — a DSL that looks complete and matches
// every row.
{
  const cyc = generateDsl(
    [root(), logic('a1', 'and', 10), logic('a2', 'and', 20), sortNode('s1', 'id')],
    [edge('a1', 'a2', 'in'), edge('a2', 'a1', 'in'), edge('a1', 'root', 'filter'), edge('s1', 'root', 'sort')],
  )
  eq('CRITIQUE-1a cycle leaves only directives', cyc.dsl, 'sort=id:asc')
  hasWarning('CRITIQUE-1a cycle warns about the missing filter', cyc.warnings, 'matches every row')

  const dead = generateDsl(
    [root(), logic('a1', 'and'), cond('c1', '', '=', 'x'), sortNode('s1', 'id')],
    [edge('c1', 'a1', 'in'), edge('a1', 'root', 'filter'), edge('s1', 'root', 'sort')],
  )
  eq('CRITIQUE-1a dead AND leaves only directives', dead.dsl, 'sort=id:asc')
  hasWarning('CRITIQUE-1a dead AND warns about the missing filter', dead.warnings, 'matches every row')

  // No filter handle wired at all is not the same thing and must stay quiet.
  const none = generateDsl([root(), sortNode('s1', 'id')], [edge('s1', 'root', 'sort')])
  eq('CRITIQUE-1a unwired filter handle is silent', none.warnings.length, 0)
}

console.log(`playground hunt#7 regression: ${checks - failures}/${checks} checks passed`)
if (failures > 0) throw new Error(`${failures} playground regression check(s) failed`)
