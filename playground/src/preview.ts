import { CUSTOMER_FIELDS, ORDER_FIELDS, type Customer, type Order } from './sampleData.js'

type Scalar = string | number | boolean | null
type Truth = boolean | null
type Row = Customer | Order
type Predicate = (row: Row) => Truth
type Token = { text: string; quoted: boolean; at: number }
type Sort = { field: string; descending: boolean }

export interface Preview {
  rows: Customer[]
  matchingIds: Set<number>
  visibleIds: Set<number>
  matched: number
  skip: number
  take: number
  loadsOrders: boolean
}

const OPS = ['<notnull>', '<null>', '<nin>', '<in>', '<bet>', '!=^', '.=^', '=^', '!=~', '=~', '>=', '<=', '!=', '>', '<', '=']
const DIRECTIVES = ['sort', 'page', 'load']

function tokenize(dsl: string): Token[] {
  if (dsl.length > 12000) throw new Error('The demo accepts at most 12,000 characters of DSL.')
  const tokens: Token[] = []
  let i = 0
  while (i < dsl.length) {
    if (/\s/.test(dsl[i])) { i++; continue }
    const at = i
    if (dsl[i] === '"') {
      const end = dsl.indexOf('"', i + 1)
      if (end < 0) throw new Error(`Unclosed quoted value at character ${i + 1}.`)
      tokens.push({ text: dsl.slice(i + 1, end), quoted: true, at }); i = end + 1
    } else {
      const op = OPS.find((candidate) => dsl.startsWith(candidate, i))
      if (op) { tokens.push({ text: op, quoted: false, at }); i += op.length }
      else if (dsl.startsWith('..', i)) { tokens.push({ text: '..', quoted: false, at }); i += 2 }
      else if ('()[],:|'.includes(dsl[i])) { tokens.push({ text: dsl[i++], quoted: false, at }) }
      else {
        while (i < dsl.length && !/[\s"()\[\],:|<>=!~^]/.test(dsl[i]) && !dsl.startsWith('..', i) && !dsl.startsWith('.=^', i)) i++
        if (i === at) throw new Error(`Unexpected character at position ${i + 1}.`)
        tokens.push({ text: dsl.slice(at, i), quoted: false, at })
      }
    }
    if (tokens.length > 1000) throw new Error('This filter is too large for the sample preview.')
  }
  return tokens
}

function fieldValue(row: Row, field: string): Scalar {
  return (row as unknown as Record<string, Scalar>)[field]
}

function and(a: Truth, b: Truth): Truth { return a === false || b === false ? false : a === null || b === null ? null : true }
function or(a: Truth, b: Truth): Truth { return a === true || b === true ? true : a === null || b === null ? null : false }
function not(a: Truth): Truth { return a === null ? null : !a }

// SQL LIKE matching in bounded O(pattern × text) time, without evaluating user regexes.
function like(text: string, pattern: string, insensitive: boolean): boolean {
  if (pattern.length > 256) throw new Error('LIKE patterns in the demo are limited to 256 characters.')
  if (insensitive) { text = text.toLowerCase(); pattern = pattern.toLowerCase() }
  const chars = Array.from(text)
  let previous = Array<boolean>(chars.length + 1).fill(false)
  previous[0] = true
  for (const char of pattern) {
    const next = Array<boolean>(chars.length + 1).fill(false)
    next[0] = char === '%' && previous[0]
    for (let i = 1; i <= chars.length; i++) {
      next[i] = char === '%' ? previous[i] || next[i - 1] : previous[i - 1] && (char === '_' || char === chars[i - 1])
    }
    previous = next
  }
  return previous[chars.length]
}

function compare(a: Scalar, b: Scalar): number | null {
  if (a === null || b === null || typeof a !== typeof b) return null
  return a === b ? 0 : a < b ? -1 : 1
}

class Parser {
  private pos = 0
  private depth = 0
  constructor(private tokens: Token[]) {}
  private peek(offset = 0) { return this.tokens[this.pos + offset] }
  private is(text: string) { const t = this.peek(); return t !== undefined && !t.quoted && t.text === text }
  private take(text: string) { if (!this.is(text)) return false; this.pos++; return true }
  private expect(text: string) {
    if (!this.take(text)) throw new Error(`Expected ${text} near character ${(this.peek()?.at ?? 0) + 1}.`)
  }
  private directive() { return this.peek() && !this.peek().quoted && DIRECTIVES.includes(this.peek().text) && this.peek(1)?.text === '=' && !this.peek(1).quoted }
  private field(fields: readonly string[]) {
    const token = this.tokens[this.pos++]
    if (!token || token.quoted || !fields.includes(token.text)) {
      throw new Error(`Unknown field ${JSON.stringify(token?.text ?? '')}. Available fields: ${fields.join(', ')}.`)
    }
    return token.text
  }
  private scalar(): Scalar {
    const token = this.tokens[this.pos++]
    if (!token || (!token.quoted && ['(', ')', '[', ']', ',', ':', '|', '..', ...OPS].includes(token.text))) {
      throw new Error('A comparison value is missing or incomplete.')
    }
    if (token.quoted) return token.text
    if (token.text === 'null') return null
    if (token.text === 'true' || token.text === 'false') return token.text === 'true'
    if (/^-?\d+(\.\d+)?$/.test(token.text)) {
      const value = Number(token.text)
      if (!Number.isFinite(value)) throw new Error('Numeric values must be finite.')
      return value
    }
    return token.text
  }
  private unary(fields: readonly string[]): Predicate {
    if (++this.depth > 40) throw new Error('The demo supports at most 40 levels of grouping.')
    try {
      if (this.take('not')) { const expr = this.unary(fields); return (row) => not(expr(row)) }
      if (this.take('(')) { const expr = this.expr(fields); this.expect(')'); return expr }
      const field = this.field(fields)
      const operator = this.tokens[this.pos++]
      const op = operator?.text
      if (!op || operator.quoted || !OPS.includes(op)) throw new Error(`Choose a comparison operator for ${field}.`)
      if (op === '=~' || op === '!=~') throw new Error('Regex preview is not supported. Use LIKE (=^) or the Go adapters for regular expressions.')
      if (op === '<null>' || op === '<notnull>') return (row) => (fieldValue(row, field) === null) === (op === '<null>')
      let values: Scalar[]
      if (op === '<in>' || op === '<nin>') {
        this.expect('['); values = []
        if (!this.take(']')) {
          do { values.push(this.scalar()) } while (this.take(','))
          this.expect(']')
        }
      } else if (op === '<bet>') {
        this.expect('('); const lo = this.scalar(); this.expect('..'); const hi = this.scalar(); this.expect(')'); values = [lo, hi]
      } else values = [this.scalar()]
      if (['=^', '!=^', '.=^'].includes(op) && (typeof values[0] !== 'string' || values[0].length > 256)) {
        throw new Error('LIKE needs a string pattern of at most 256 characters.')
      }
      return (row) => {
        const actual = fieldValue(row, field), value = values[0]
        if (op === '<in>' || op === '<nin>') {
          if (values.length === 0) return op === '<nin>'
          const found: Truth = actual === null ? null : values.includes(actual) ? true : values.includes(null) ? null : false
          return op === '<nin>' ? not(found) : found
        }
        if ((op === '=' || op === '!=') && value === null) return (actual === null) === (op === '=')
        if (actual === null || value === null) return null
        if (['=^', '!=^', '.=^'].includes(op)) {
          const matched = typeof actual === 'string' && like(actual, value as string, op === '.=^')
          return op === '!=^' ? !matched : matched
        }
        if (op === '<bet>') {
          const lo = compare(actual, values[0]), hi = compare(actual, values[1])
          return lo === null || hi === null ? null : lo >= 0 && hi <= 0
        }
        if (op === '=') return actual === value
        if (op === '!=') return actual !== value
        const c = compare(actual, value)
        if (c === null) return false
        return op === '>' ? c > 0 : op === '>=' ? c >= 0 : op === '<' ? c < 0 : c <= 0
      }
    } finally { this.depth-- }
  }
  private conjunction(fields: readonly string[]): Predicate {
    let left = this.unary(fields)
    while (this.peek() && !this.is('or') && !this.is(')') && !this.is(']') && !this.is('|') && !this.directive()) {
      this.take('and') // Adjacent predicates also mean AND in figo.
      const a = left, b = this.unary(fields)
      left = (row) => and(a(row), b(row))
    }
    return left
  }
  private expr(fields: readonly string[]): Predicate {
    let left = this.conjunction(fields)
    while (this.take('or')) { const a = left, b = this.conjunction(fields); left = (row) => or(a(row), b(row)) }
    return left
  }
  parse() {
    const filter = this.peek() && !this.directive() ? this.expr(CUSTOMER_FIELDS) : () => true
    const sort: Sort[] = []
    let skip = 0, take = 0, orders: Predicate | null = null
    while (this.peek()) {
      const directive = this.peek().text
      if (!this.directive()) throw new Error(`Unexpected token ${JSON.stringify(directive)}.`)
      this.pos += 2
      if (directive === 'sort') {
        do {
          const field = this.field(CUSTOMER_FIELDS); this.expect(':')
          const direction = this.tokens[this.pos++]
          if (!direction || direction.quoted || (direction.text !== 'asc' && direction.text !== 'desc')) throw new Error('Sort direction must be asc or desc.')
          sort.push({ field, descending: direction.text === 'desc' })
        } while (this.take(','))
      } else if (directive === 'page') {
        do {
          const token = this.tokens[this.pos++]
          const key = token?.text
          if (!token || token.quoted || (key !== 'skip' && key !== 'take')) throw new Error('Page accepts skip and take.')
          this.expect(':'); const value = this.scalar()
          if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) throw new Error('Page skip/take must be non-negative whole numbers; take:0 means all rows.')
          if (key === 'skip') skip = value; else take = value
        } while (this.take(','))
      } else {
        this.expect('[')
        if (!this.take('Orders')) throw new Error('The sample dataset has one relation: Orders (case-sensitive).')
        if (orders) throw new Error('Use one Orders preload in the sample preview.')
        this.expect(':'); orders = this.expr(ORDER_FIELDS); this.expect(']')
      }
    }
    return { filter, sort, skip, take, orders }
  }
}

/** Parse the emitted DSL string, not the diagram, then filter → sort → page → preload. */
export function previewDsl(dsl: string, source: Customer[]): Preview {
  const query = new Parser(tokenize(dsl)).parse()
  const matches = source.filter((row) => query.filter(row) === true)
  matches.sort((a, b) => {
    for (const sort of query.sort) {
      const av = fieldValue(a, sort.field), bv = fieldValue(b, sort.field)
      // The demo consistently places NULLs last; database defaults vary.
      const c = av === bv ? 0 : av === null ? 1 : bv === null ? -1 : (compare(av, bv) ?? 0) * (sort.descending ? -1 : 1)
      if (c) return c
    }
    return 0
  })
  const rows = matches.slice(query.skip, query.take ? query.skip + query.take : undefined).map((row) => ({
    ...row, Orders: row.Orders.filter((order) => !query.orders || query.orders(order) === true),
  }))
  return {
    rows, matchingIds: new Set(matches.map((row) => row.id)), visibleIds: new Set(rows.map((row) => row.id)),
    matched: matches.length, skip: query.skip, take: query.take, loadsOrders: query.orders !== null,
  }
}
