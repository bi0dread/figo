import { useCallback, useMemo, useRef } from 'react'
import {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type Edge,
  type IsValidConnection,
} from '@xyflow/react'
import { nodeTypes } from './nodes'
import { edgeTypes } from './edges'
import { Palette, type PaletteItem } from './Palette'
import { DslPanel } from './DslPanel'
import { generateDsl, type AppNode } from './dsl'

type Kind = 'expr' | 'sort' | 'page' | 'load'

const EDGE_COLORS: Record<Kind, string> = {
  expr: '#8b9cc0',
  sort: '#2dd4bf',
  page: '#38bdf8',
  load: '#4ade80',
}

/** What a node's source handle emits. */
function outputKind(node: AppNode): Kind | null {
  switch (node.type) {
    case 'condition':
    case 'logic':
      return 'expr'
    case 'sort':
      return 'sort'
    case 'page':
      return 'page'
    case 'load':
      return 'load'
    default:
      return null
  }
}

/** What a target handle accepts. */
function acceptedKind(node: AppNode, handle: string | null | undefined): Kind | null {
  if (node.type === 'logic' && handle === 'in') return 'expr'
  if (node.type === 'load' && handle === 'filter') return 'expr'
  if (node.type === 'root') {
    if (handle === 'filter') return 'expr'
    if (handle === 'sort') return 'sort'
    if (handle === 'page') return 'page'
    if (handle === 'load') return 'load'
  }
  return null
}

/** Handles that accept only a single incoming edge (new connection replaces old). */
function isSingleInput(node: AppNode, handle: string | null | undefined): boolean {
  if (node.type === 'root' && (handle === 'filter' || handle === 'page')) return true
  if (node.type === 'load' && handle === 'filter') return true
  if (node.type === 'logic' && node.data.kind === 'not') return true
  return false
}

let edgeSeq = 0
function mkEdge(source: string, target: string, targetHandle: string, kind: Kind): Edge {
  return {
    id: `e-${source}-${target}-${targetHandle}-${edgeSeq++}`,
    source,
    target,
    targetHandle,
    type: 'pip',
    style: { stroke: EDGE_COLORS[kind], strokeWidth: 1.6 },
  }
}

function presetNodes(): AppNode[] {
  return [
    { id: 'root', type: 'root', position: { x: 880, y: 170 }, deletable: false, data: {} },
    {
      id: 'c-status',
      type: 'condition',
      position: { x: 30, y: 10 },
      data: { field: 'status', op: '=', value: 'active', value2: '' },
    },
    {
      id: 'c-age',
      type: 'condition',
      position: { x: 30, y: 175 },
      data: { field: 'age', op: '<bet>', value: '18', value2: '65' },
    },
    { id: 'and-1', type: 'logic', position: { x: 360, y: 90 }, data: { kind: 'and' } },
    {
      id: 'c-vip',
      type: 'condition',
      position: { x: 30, y: 350 },
      data: { field: 'vip', op: '=', value: 'true', value2: '' },
    },
    { id: 'or-1', type: 'logic', position: { x: 590, y: 190 }, data: { kind: 'or' } },
    {
      id: 's-created',
      type: 'sort',
      position: { x: 590, y: 340 },
      data: { field: 'created_at', dir: 'desc' },
    },
    { id: 'p-1', type: 'page', position: { x: 590, y: 450 }, data: { skip: 0, take: 20 } },
    {
      id: 'c-total',
      type: 'condition',
      position: { x: 30, y: 520 },
      data: { field: 'total', op: '>', value: '100', value2: '' },
    },
    { id: 'l-orders', type: 'load', position: { x: 360, y: 540 }, data: { relation: 'Orders' } },
  ]
}

function presetEdges(): Edge[] {
  return [
    mkEdge('c-status', 'and-1', 'in', 'expr'),
    mkEdge('c-age', 'and-1', 'in', 'expr'),
    mkEdge('and-1', 'or-1', 'in', 'expr'),
    mkEdge('c-vip', 'or-1', 'in', 'expr'),
    mkEdge('or-1', 'root', 'filter', 'expr'),
    mkEdge('s-created', 'root', 'sort', 'sort'),
    mkEdge('p-1', 'root', 'page', 'page'),
    mkEdge('c-total', 'l-orders', 'filter', 'expr'),
    mkEdge('l-orders', 'root', 'load', 'load'),
  ]
}

function defaultData(item: PaletteItem): Record<string, any> {
  switch (item.type) {
    case 'condition':
      return { field: '', op: '=', value: '', value2: '' }
    case 'logic':
      return { kind: item.kind ?? 'and' }
    case 'sort':
      return { field: '', dir: 'asc' }
    case 'page':
      return { skip: 0, take: 20 }
    case 'load':
      return { relation: '' }
  }
}

function Flow() {
  const [nodes, setNodes, onNodesChange] = useNodesState<AppNode>(presetNodes())
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>(presetEdges())
  const { screenToFlowPosition, getNode } = useReactFlow()
  const wrapRef = useRef<HTMLDivElement>(null)
  const nodeSeq = useRef(0)

  const result = useMemo(() => generateDsl(nodes, edges), [nodes, edges])

  // Nodes not contributing to the DSL are dimmed so the query shape reads at a glance.
  const displayNodes = useMemo(
    () =>
      nodes.map((n) => (result.active.has(n.id) ? n : { ...n, className: 'dimmed' })),
    [nodes, result],
  )

  const wouldCycle = useCallback(
    (source: string, target: string): boolean => {
      // adding source→target cycles iff target already reaches source
      const seen = new Set<string>([target])
      const queue = [target]
      while (queue.length > 0) {
        const cur = queue.pop()!
        if (cur === source) return true
        for (const e of edges) {
          if (e.source === cur && !seen.has(e.target)) {
            seen.add(e.target)
            queue.push(e.target)
          }
        }
      }
      return false
    },
    [edges],
  )

  const isValidConnection: IsValidConnection = useCallback(
    (conn) => {
      if (!conn.source || !conn.target || conn.source === conn.target) return false
      const src = getNode(conn.source) as AppNode | undefined
      const tgt = getNode(conn.target) as AppNode | undefined
      if (!src || !tgt) return false
      const out = outputKind(src)
      if (out === null || out !== acceptedKind(tgt, conn.targetHandle)) return false
      return !wouldCycle(conn.source, conn.target)
    },
    [getNode, wouldCycle],
  )

  const onConnect = useCallback(
    (conn: Connection) => {
      const src = getNode(conn.source) as AppNode | undefined
      const tgt = getNode(conn.target) as AppNode | undefined
      if (!src || !tgt) return
      const kind = outputKind(src)
      if (kind === null) return
      setEdges((eds) => {
        let next = eds
        if (isSingleInput(tgt, conn.targetHandle)) {
          next = next.filter(
            (e) => !(e.target === conn.target && e.targetHandle === conn.targetHandle),
          )
        }
        return addEdge(
          { ...conn, type: 'pip', style: { stroke: EDGE_COLORS[kind], strokeWidth: 1.6 } },
          next,
        )
      })
    },
    [getNode, setEdges],
  )

  const addNode = useCallback(
    (item: PaletteItem, position: { x: number; y: number }) => {
      const id = `n-${item.type}-${nodeSeq.current++}`
      setNodes((nds) => [
        ...nds,
        { id, type: item.type, position, data: defaultData(item) },
      ])
    },
    [setNodes],
  )

  const onDrop = useCallback(
    (event: React.DragEvent) => {
      event.preventDefault()
      const payload = event.dataTransfer.getData('application/figo-node')
      if (payload === '') return
      const item = JSON.parse(payload) as PaletteItem
      addNode(item, screenToFlowPosition({ x: event.clientX, y: event.clientY }))
    },
    [addNode, screenToFlowPosition],
  )

  const onDragOver = useCallback((event: React.DragEvent) => {
    event.preventDefault()
    event.dataTransfer.dropEffect = 'move'
  }, [])

  const addAtCenter = useCallback(
    (item: PaletteItem) => {
      const rect = wrapRef.current?.getBoundingClientRect()
      const center = rect
        ? { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 }
        : { x: 400, y: 300 }
      const offset = (nodeSeq.current % 5) * 28
      const pos = screenToFlowPosition(center)
      addNode(item, { x: pos.x - 100 + offset, y: pos.y - 40 + offset })
    },
    [addNode, screenToFlowPosition],
  )

  const loadExample = useCallback(() => {
    setNodes(presetNodes())
    setEdges(presetEdges())
  }, [setNodes, setEdges])

  const clearAll = useCallback(() => {
    setNodes((nds) => nds.filter((n) => n.type === 'root'))
    setEdges([])
  }, [setNodes, setEdges])

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">🧩 figo playground</span>
        <span className="tagline">drag figo operations, wire them up, read the DSL</span>
        <span className="spacer" />
        <button onClick={loadExample}>Example</button>
        <button onClick={clearAll}>Clear</button>
        <a href="https://github.com/bi0dread/figo" target="_blank" rel="noreferrer">
          GitHub ↗
        </a>
      </header>
      <div className="main">
        <Palette onAdd={addAtCenter} />
        <div className="flow-wrap" ref={wrapRef} onDrop={onDrop} onDragOver={onDragOver}>
          <ReactFlow
            colorMode="dark"
            nodes={displayNodes}
            edges={edges}
            nodeTypes={nodeTypes}
            edgeTypes={edgeTypes}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            isValidConnection={isValidConnection}
            deleteKeyCode={['Backspace', 'Delete']}
            fitView
            fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
            proOptions={{ hideAttribution: false }}
          >
            <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} />
            <Controls />
            <MiniMap pannable zoomable />
          </ReactFlow>
        </div>
      </div>
      <DslPanel result={result} />
    </div>
  )
}

export default function App() {
  return (
    <ReactFlowProvider>
      <Flow />
    </ReactFlowProvider>
  )
}
