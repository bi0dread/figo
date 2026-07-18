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

const IDENT_STRIP_RE = /[\s"<>=!~^()[\],:|]/g

/**
 * Field / relation names are bare tokens in the DSL — whitespace or operator
 * characters inside them cannot be expressed, so they are stripped.
 */
function sanitizeIdent(raw: string, what: string, warn: (msg: string) => void): string {
  const t = raw.trim()
  const clean = t.replace(IDENT_STRIP_RE, '')
  if (clean !== t) warn(`${what} "${t}" contains characters the DSL cannot parse — they were removed`)
  return clean
}

/** Split an IN/NIN list on commas, keeping quoted segments intact. */
function splitList(raw: string): string[] {
  const parts = raw.match(/"[^"]*"|[^,]+/g) ?? []
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
    let text: string
    if (NULL_OPS.includes(op)) {
      text = `${field}${op}`
    } else if (LIST_OPS.includes(op)) {
      const items = splitList(String(d.value ?? ''))
      text = `${field}${op}[${items.map((v) => formatValue(v, warn)).join(',')}]`
    } else if (op === RANGE_OP) {
      const lo = String(d.value ?? '').trim()
      const hi = String(d.value2 ?? '').trim()
      if (lo === '' || hi === '') warn(`BETWEEN on "${field}" needs both bounds`)
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
    const skip = Number(pageNode.data.skip) || 0
    const take = Number(pageNode.data.take) || 0
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
