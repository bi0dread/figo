import type { Edge } from '@xyflow/react'
import type { AppNode } from './dsl.js'

export const SAMPLES = [
  { id: 'complex', name: '01 · Nested conditions + orders', description: 'The full example: grouped conditions, NOT, dates, NULL checks, sorting, pagination, and filtered orders.' },
  { id: 'active', name: '02 · Active customers', description: 'Keep active, undeleted accounts and sort by monthly revenue.' },
  { id: 'revenue', name: '03 · Revenue + signup dates', description: 'Active or trialing accounts with at least $250 MRR that signed up during 2024.' },
  { id: 'names', name: '04 · Case-insensitive name search', description: 'Find names containing “al”, regardless of letter case.' },
  { id: 'page', name: '05 · Second page of results', description: 'Sort active customers, skip the first three matches, and return the next three.' },
  { id: 'orders', name: '06 · Filter related orders', description: 'Keep active customers, then load only orders above $100 that were not refunded. Customers with no qualifying orders remain visible.' },
] as const

type Condition = { field: string; op: string; value?: string; value2?: string }
const presets: Record<string, { conditions: Condition[]; sort: [string, string][]; page?: [number, number]; orders?: Condition[] }> = {
  active: {
    conditions: [{ field: 'status', op: '=', value: 'active' }, { field: 'deleted_at', op: '<null>' }],
    sort: [['mrr', 'desc']],
  },
  revenue: {
    conditions: [{ field: 'status', op: '<in>', value: 'active, trialing' }, { field: 'mrr', op: '>=', value: '250' }, { field: 'signup_date', op: '<bet>', value: '2024-01-01', value2: '2024-12-31' }],
    sort: [['mrr', 'desc'], ['name', 'asc']],
  },
  names: { conditions: [{ field: 'name', op: '.=^', value: '%al%' }], sort: [['name', 'asc']] },
  page: { conditions: [{ field: 'status', op: '=', value: 'active' }], sort: [['mrr', 'desc'], ['name', 'asc']], page: [3, 3] },
  orders: {
    conditions: [{ field: 'status', op: '=', value: 'active' }, { field: 'deleted_at', op: '<null>' }],
    sort: [['name', 'asc']],
    orders: [{ field: 'total', op: '>', value: '100' }, { field: 'status', op: '!=', value: 'refunded' }],
  },
}

export function sampleGraph(id: string): { nodes: AppNode[]; edges: Edge[] } {
  const preset = presets[id]
  if (!preset) throw new Error(`Unknown sample: ${id}`)
  const nodes: AppNode[] = [{ id: 'root', type: 'root', position: { x: 960, y: 190 }, deletable: false, data: {} }]
  const edges: Edge[] = []
  const connect = (source: string, target: string, targetHandle: string, color = '#8b9cc0') => {
    edges.push({ id: `sample-${source}-${target}-${targetHandle}`, source, target, targetHandle, type: 'pip', style: { stroke: color, strokeWidth: 1.6 } })
  }
  const conditions = (items: Condition[], prefix: string, y: number, target: string, handle: string) => {
    const group = items.length > 1 ? `${prefix}-and` : null
    items.forEach((data, i) => {
      const id = `${prefix}-${i}`
      nodes.push({ id, type: 'condition', position: { x: 20, y: y + i * 125 }, data: { value: '', value2: '', ...data } })
      connect(id, group ?? target, group ? 'in' : handle)
    })
    if (group) {
      nodes.push({ id: group, type: 'logic', position: { x: 420, y: y + (items.length - 1) * 62 }, data: { kind: 'and' } })
      connect(group, target, handle)
    }
  }
  conditions(preset.conditions, 'filter', 0, 'root', 'filter')
  preset.sort.forEach(([field, dir], i) => {
    const id = `sample-sort-${i}`
    nodes.push({ id, type: 'sort', position: { x: 710, y: 420 + i * 120 }, data: { field, dir } })
    connect(id, 'root', 'sort', '#2dd4bf')
  })
  if (preset.page) {
    nodes.push({ id: 'sample-page', type: 'page', position: { x: 1000, y: 430 }, data: { skip: preset.page[0], take: preset.page[1] } })
    connect('sample-page', 'root', 'page', '#38bdf8')
  }
  if (preset.orders) {
    nodes.push({ id: 'sample-load', type: 'load', position: { x: 710, y: 720 }, data: { relation: 'Orders' } })
    conditions(preset.orders, 'orders', 650, 'sample-load', 'filter')
    connect('sample-load', 'root', 'load', '#4ade80')
  }
  return { nodes, edges }
}
