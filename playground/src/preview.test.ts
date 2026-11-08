import { previewDsl } from './preview.js'
import { CUSTOMERS } from './sampleData.js'
import { generateDsl } from './dsl.js'
import { sampleGraph, SAMPLES } from './samples.js'

let checks = 0
function equal(actual: unknown, expected: unknown, message: string) {
  checks++
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${message}: got ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`)
}
function ids(dsl: string) { return previewDsl(dsl, CUSTOMERS).rows.map((row) => row.id) }
function rejects(dsl: string) {
  checks++
  try { previewDsl(dsl, CUSTOMERS) } catch { return }
  throw new Error(`Invalid or unsupported DSL was accepted: ${dsl}`)
}

const full = '(status<in>["active","trialing"] or (plan="enterprise" and seats>=50)) and not (email=^"%@test.local" or name=^"QA %") and signup_date<bet>(2024-01-01..2024-12-31) and deleted_at<null> and last_login<notnull> and mrr>99.5 sort=mrr:desc,name:asc page=skip:0,take:20 load=[Orders:total>100 and status!="refunded"]'
const result = previewDsl(full, CUSTOMERS)
equal(result.rows.map((row) => row.id), [4, 15, 3, 2, 16, 18, 1, 14], 'Full user example: filtering and stable multi-column sorting')
equal(result.rows.find((row) => row.id === 1)?.Orders.map((order) => order.id), [12], 'Preload filters child orders')
equal(result.rows.find((row) => row.id === 14)?.Orders, [], 'Preload with no matching children keeps the parent account')
equal(CUSTOMERS[0].Orders.length, 3, 'Preview never mutates source orders')
equal(ids('').length, 18, 'Empty DSL shows the dataset')
equal(ids('page=skip:17,take:0'), [18], 'take:0 means no limit, with skip applied')
equal(ids('page=skip:100,take:3'), [], 'Out-of-range page is empty')
equal(ids('status="trialing" or status="paused" and seats>=50'), [2, 3, 11, 14], 'AND has higher precedence than OR')
equal(ids('(status="trialing" or status="paused") and seats>=50'), [3], 'Parentheses override precedence')
equal(ids('not last_login="2026-10-20"'), [], 'NOT preserves SQL UNKNOWN for NULL values')
equal(ids('last_login=null'), [8, 13], 'Equality to null is IS NULL')
equal(ids('last_login<in>[]'), [], 'Empty IN matches nothing, including NULL rows')
equal(ids('last_login<nin>[]').length, 18, 'Empty NOT IN matches everything')
equal(ids('last_login<notnull> last_login<nin>["different",null]'), [], 'NULL in NOT IN yields UNKNOWN')
equal(ids('name.=^"%AL%"'), [1, 7, 16], 'Case-insensitive LIKE')
equal(ids('name=^"Alice ____"'), [1], 'LIKE underscore matches one character')
equal(ids('signup_date<bet>(2024-01-01..2024-01-01)'), [16], 'BETWEEN bounds are inclusive')
equal(ids('mrr<bet>(299..299)'), [2, 16, 18], 'Numeric range is inclusive')
equal(ids('name="Alice Chen" or name="QA Kelly"'), [1, 10], 'Quoted strings with spaces')
equal(ids('id<in>[1,2] sort=name:desc page=skip:0,take:1'), [2], 'Filter, sort, and pagination execute in order')
equal(ids('id=14 load=[Orders:total>100]'), [14], 'Load does not become an EXISTS filter')

for (const sample of SAMPLES.filter((sample) => sample.id !== 'complex')) {
  const graph = sampleGraph(sample.id), generated = generateDsl(graph.nodes, graph.edges)
  equal(generated.warnings, [], `${sample.name}: valid graph DSL`)
  const preview = previewDsl(generated.dsl, CUSTOMERS)
  equal(preview.rows.length > 0, true, `${sample.name}: returns illustrative data`)
  if (sample.id === 'page') equal(preview.rows.map((row) => row.id), [7, 6, 16], 'Second-page preset is reproducible')
  if (sample.id === 'orders') {
    equal(preview.rows.find((row) => row.id === 1)?.Orders.length, 1, 'Orders preset applies preload')
    equal(preview.rows.find((row) => row.id === 18)?.Orders.length, 0, 'Orders preset includes a parent with no qualifying children')
  }
}

for (const dsl of [
  'unknown=1', 'name=~"(a+)+$"', 'name="unfinished', 'mrr<bet>(10..)',
  'status="active" and', 'page=skip:-1,take:3', 'page=skip:0,take:1.5',
  'sort=unknown:asc', 'sort=name:sideways', 'load=[Missing:total>1]',
  'load=[Orders:unknown=1]', 'mrr>1 trailing', 'id "=" 1', 'sort=name:"asc"',
  '('.repeat(45) + 'id=1' + ')'.repeat(45),
]) rejects(dsl)

console.log(`Live data preview: ${checks} checks passed`)
