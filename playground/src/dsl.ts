import type { Edge, Node } from '@xyflow/react'

export type AppNode = Node<Record<string, any>>

export interface DslResult {
  dsl: string
  /** ids of nodes that contributed to the generated DSL (root included) */
  active: Set<string>
  warnings: string[]
}

/** Operators that take no value. */
export const NULL_OPS = ['<null>', '<notnull>']
/** Operators that take a comma-separated list. */
export const LIST_OPS = ['<in>', '<nin>']
/** Operator that takes a min..max range. */
export const RANGE_OP = '<bet>'

const NUMBER_RE = /^-?\d+(\.\d+)?$/
const DATE_RE = /^\d{4}-\d{2}-\d{2}(T[\d:.+Z-]+)?$/
const WRAPPED_IN_QUOTES_RE = /^"[\s\S]*"$/

/**
 * Format a single scalar the way the figo DSL types it: numbers, booleans,
 * null and ISO dates stay bare; everything else is double-quoted so it is
 * parsed as a string (and so whitespace/operator characters stay literal).
 * Wrapping the input in quotes yourself forces string typing (`"0123"`).
 * The DSL has no escape sequence for embedded double quotes, so those are
 * stripped (with a warning).
 */
export function formatValue(raw: string, warn?: (msg: string) => void): string {
  let t = raw.trim()
  const wasQuoted = WRAPPED_IN_QUOTES_RE.test(t) && t.length >= 2
  if (wasQuoted) t = t.slice(1, -1)
  if (t.includes('"')) {
    warn?.('Double quotes inside values are not supported by the DSL and were removed')
    t = t.replace(/"/g, '')
  }
  if (!wasQuoted) {
    if (t === '') return '""'
    if (NUMBER_RE.test(t)) return t
    if (t === 'true' || t === 'false' || t === 'null') return t
    if (DATE_RE.test(t)) return t
  }
  return `"${t}"`
}

/** Range bounds live inside `(..)` — numbers and dates go bare, strings quoted. */
function formatRangeBound(raw: string, warn?: (msg: string) => void): string {
  const t = raw.trim()
  if (t === '' || NUMBER_RE.test(t) || DATE_RE.test(t)) return t
  return formatValue(t, warn)
}

/**
 * Characters that cannot appear in a bare DSL identifier.
 *
 * The operator/whitespace set is what the tokenizer would otherwise read as the
 * end of the field name. The `\u0000-\u001f` + `\u007f` range is separate and
 * load-bearing: `\s` only covers 0x09-0x0d, so NUL and the rest of the C0
 * controls used to survive sanitisation, and the Go raw adapter's `validateIdent`
 * (adapters/dialect.go) REJECTS a control byte in a field name, sort key,
 * projection column or table name — `GetSqlString` returns ok=false and
 * `GetQuery` returns nil for the whole query. Emitting such a name handed the
 * user a string the playground presented as valid DSL and the library then
 * refused to render at all.
 */
const IDENT_STRIP_RE = /[\s"<>=!~^()[\],:|\u0000-\u001f\u007f]/g

/**
 * Dots separate the segments of a qualified name (`orders.total`), so they are
 * NOT stripped — but an EMPTY segment is rejected by the same `validateIdent`
 * as a control byte ("" / "..." / "a..b" / ".a" / "a." all fail the render under
 * `NoChangeNaming`, and "..." fails even under the default `SnakeCaseNaming`).
 * Collapsing runs of dots and trimming the ends keeps every legitimate qualified
 * name intact while making an empty segment unrepresentable.
 */
function collapseDotSegments(ident: string): string {
  return ident.replace(/\.{2,}/g, '.').replace(/^\.+/, '').replace(/\.+$/, '')
}

/**
 * Tokens the DSL reads as directives rather than as conditions. The parser only
 * treats them that way when an `=` follows the keyword immediately (README
 * "Directives"), so `sort!=x`, `sort>=x`, `sort.=^x` and `sort<in>[…]` are
 * ordinary conditions on a column named `sort` and stay allowed — it is the
 * operators that begin with `=` that collide. Matching is case-sensitive on the
 * Go side: `Sort="x"` is an ordinary field.
 */
const DIRECTIVE_IDENTS = ['sort', 'page', 'load']

/**
 * Field / relation names are bare tokens in the DSL — whitespace or operator
 * characters inside them cannot be expressed, so they are stripped, and an empty
 * dot segment is collapsed away.
 *
 * The invariant this function owes the rest of the app: whatever it returns must
 * be something the Go library will actually RENDER. The playground presents its
 * output as valid DSL, so a name that survives sanitisation but trips
 * `validateIdent` on the Go side turns the whole query into ok=false / a nil
 * `Query` — a strictly worse outcome than the dimmed node the empty-name path
 * already produces. `examples/dslcheck` + the Playground CI cross-check step pin
 * that invariant against the real parser and all four adapters.
 */
function sanitizeIdent(raw: string, what: string, warn: (msg: string) => void): string {
  const t = raw.trim()
  const clean = collapseDotSegments(t.replace(IDENT_STRIP_RE, ''))
  if (clean !== t) warn(`${what} "${t}" contains characters the DSL cannot parse — they were removed`)
  // A '$'-leading name is legal on all three SQL dialects and is REJECTED by the
  // MongoDB adapter, which would otherwise execute it as an operator. The
  // playground does not know the target backend, so stripping it would break SQL
  // users and keeping it silently breaks Mongo ones — say so instead. This is the
  // one divergence examples/dslcheck exempts, and this warning is why.
  if (clean.split('.').some((seg) => seg.startsWith('$'))) {
    warn(`${what} "${clean}" starts with "$" — valid on SQL, but the MongoDB adapter rejects it as an operator`)
  }
  return clean
}

/**
 * Split an IN/NIN list on commas, keeping quoted segments intact.
 *
 * This is a left-to-right scanner rather than a `match()` alternation on
 * purpose: an alternation only reaches its quoted branch when a `"` sits at the
 * current scan index, so `a, "b,c", d` — the comma-space spacing the value box's
 * own `a, b, c` placeholder teaches — fell through to `[^,]+` and tore the
 * quoted item in two, emitting a four-value IN list for a three-item input.
 */
function splitList(raw: string, warn?: (msg: string) => void): string[] {
  const parts: string[] = []
  let cur = ''
  let inQuotes = false
  for (const ch of raw) {
    if (ch === '"') {
      inQuotes = !inQuotes
      cur += ch
    } else if (ch === ',' && !inQuotes) {
      parts.push(cur)
      cur = ''
    } else {
      cur += ch
    }
  }
  parts.push(cur)
  if (inQuotes) warn?.('Unclosed double quote in the list — everything after it was read as one item')
  return parts.map((p) => p.trim()).filter((p) => p !== '')
}

export function generateDsl(nodes: AppNode[], edges: Edge[]): DslResult {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const active = new Set<string>()
  const warnings: string[] = []
  const warn = (msg: string) => {
    if (!warnings.includes(msg)) warnings.push(msg)
  }

  /** Source nodes feeding a given handle, ordered top→bottom on the canvas. */
  const incoming = (targetId: string, handle: string): AppNode[] =>
    edges
      .filter((e) => e.target === targetId && e.targetHandle === handle)
      .map((e) => byId.get(e.source))
      .filter((n): n is AppNode => n !== undefined)
      .sort((a, b) => a.position.y - b.position.y || a.position.x - b.position.x)

  const renderCondition = (node: AppNode): string | null => {
    const d = node.data
    const field = sanitizeIdent(String(d.field ?? ''), 'Field', warn)
    if (field === '') {
      warn('A condition node is missing its field name')
      return null
    }
    const op = String(d.op ?? '=')
    // A condition on a column literally named sort/page/load is unexpressible
    // when the operator starts with `=`: the parser reads `sort="active"` as a
    // malformed sort= directive, drops the predicate AND the connector next to
    // it, and the emitted string would otherwise look like a valid filter.
    if (DIRECTIVE_IDENTS.includes(field) && op.startsWith('=')) {
      warn(
        `Field "${field}" with "${op}" would be read as the ${field}= directive — the DSL cannot express it (use a different operator, e.g. ${field}!=…)`,
      )
      return null
    }
    let text: string
    if (NULL_OPS.includes(op)) {
      text = `${field}${op}`
    } else if (LIST_OPS.includes(op)) {
      const items = splitList(String(d.value ?? ''), warn)
      text = `${field}${op}[${items.map((v) => formatValue(v, warn)).join(',')}]`
    } else if (op === RANGE_OP) {
      const lo = String(d.value ?? '').trim()
      const hi = String(d.value2 ?? '').trim()
      // A half-filled BETWEEN has no honest rendering: `price<bet>(10..)` builds
      // an executable query with an empty-string upper bound, and
      // `price<bet>(..600)` is rejected by the parser so the predicate vanishes
      // and the query matches every row. Emit nothing and dim the node instead.
      if (lo === '' || hi === '') {
        warn(`BETWEEN on "${field}" needs both bounds — the condition was left out`)
        return null
      }
      text = `${field}<bet>(${formatRangeBound(lo, warn)}..${formatRangeBound(hi, warn)})`
    } else {
      text = `${field}${op}${formatValue(String(d.value ?? ''), warn)}`
    }
    active.add(node.id)
    return text
  }

  // stack guards against cycles on the current recursion path; reuse of a
  // node across two branches (a DAG) is fine and intentionally allowed.
  const stack = new Set<string>()

  const renderExpr = (node: AppNode, nested: boolean): string | null => {
    if (stack.has(node.id)) {
      warn('Cycle detected — a node cannot feed into itself')
      return null
    }
    if (node.type === 'condition') return renderCondition(node)
    if (node.type !== 'logic') return null

    const kind = String(node.data.kind) // 'and' | 'or' | 'not'
    stack.add(node.id)
    try {
      const operands = incoming(node.id, 'in')
      if (kind === 'not') {
        // The canvas keeps a NOT node to one input (App.tsx isSingleInput), but
        // if a graph ever carries more, only the top-most one survives — which
        // means dragging a node vertically would change the query. Every other
        // drop in this function warns; this one used to be silent.
        if (operands.length > 1) {
          warn(
            `A NOT node takes one input — ${operands.length - 1} extra connection(s) were ignored (the top-most input is used)`,
          )
        }
        const first = operands[0]
        const inner = first ? renderExpr(first, true) : null
        if (inner === null) {
          warn('A NOT node has no input connected')
          return null
        }
        active.add(node.id)
        return `not ${inner}`
      }
      const parts = operands
        .map((op) => renderExpr(op, true))
        .filter((p): p is string => p !== null)
      if (parts.length === 0) {
        warn(`An ${kind.toUpperCase()} node has no inputs connected`)
        return null
      }
      active.add(node.id)
      if (parts.length === 1) return parts[0]
      const joined = parts.join(` ${kind} `)
      return nested ? `(${joined})` : joined
    } finally {
      stack.delete(node.id)
    }
  }

  const root = nodes.find((n) => n.type === 'root')
  if (!root) return { dsl: '', active, warnings }
  active.add(root.id)

  // filter expression
  const filterSrc = incoming(root.id, 'filter')[0]
  const filterText = filterSrc ? renderExpr(filterSrc, false) : null
  // Something IS wired into the filter handle but nothing came back (a cycle, an
  // empty group, an all-invalid AND). The sort=/page=/load= directives below
  // still render, so the DSL looks complete while selecting every row — say so.
  if (filterSrc && filterText === null) {
    warn('The filter could not be rendered — the DSL below has NO filter and matches every row')
  }

  // sort= (multiple sort nodes merge, top→bottom)
  const sortEntries: string[] = []
  for (const n of incoming(root.id, 'sort')) {
    const field = sanitizeIdent(String(n.data.field ?? ''), 'Sort field', warn)
    if (field === '') {
      warn('A sort node is missing its field name')
      continue
    }
    sortEntries.push(`${field}:${n.data.dir === 'desc' ? 'desc' : 'asc'}`)
    active.add(n.id)
  }
  const sortText = sortEntries.length > 0 ? `sort=${sortEntries.join(',')}` : null

  // page=
  const pageNode = incoming(root.id, 'page')[0]
  let pageText: string | null = null
  if (pageNode) {
    const rawSkip = Number(pageNode.data.skip) || 0
    const rawTake = Number(pageNode.data.take) || 0
    // A negative operand is normalised to 0 by the parser with no diagnostic, so
    // `take:-3` silently means "no row limit" — the opposite of what the box
    // shows. Clamp here and say what happened.
    const skip = Math.max(0, rawSkip)
    const take = Math.max(0, rawTake)
    if (rawSkip < 0 || rawTake < 0) {
      warn('Page skip/take cannot be negative — clamped to 0 (take:0 means no row limit)')
    }
    pageText = `page=skip:${skip},take:${take}`
    active.add(pageNode.id)
  }

  // load=[Rel:filter | Rel:filter]
  const loadSegments: string[] = []
  for (const n of incoming(root.id, 'load')) {
    const relation = sanitizeIdent(String(n.data.relation ?? ''), 'Relation', warn)
    if (relation === '') {
      warn('A load node is missing its relation name')
      continue
    }
    const filterNode = incoming(n.id, 'filter')[0]
    const inner = filterNode ? renderExpr(filterNode, false) : null
    if (inner === null) {
      warn(`Load "${relation}" needs a filter connected to its filter handle`)
      continue
    }
    loadSegments.push(`${relation}:${inner}`)
    active.add(n.id)
  }
  const loadText = loadSegments.length > 0 ? `load=[${loadSegments.join(' | ')}]` : null

  const dsl = [filterText, sortText, pageText, loadText]
    .filter((p): p is string => p !== null && p !== '')
    .join(' ')

  return { dsl, active, warnings }
}
