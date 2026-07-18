import { Handle, Position, useReactFlow, type NodeProps } from '@xyflow/react'

export const OPERATORS: { value: string; label: string }[] = [
  { value: '=', label: '=  equals' },
  { value: '!=', label: '!=  not equal' },
  { value: '>', label: '>  greater' },
  { value: '>=', label: '>=  greater / eq' },
  { value: '<', label: '<  less' },
  { value: '<=', label: '<=  less / eq' },
  { value: '=^', label: '=^  LIKE' },
  { value: '!=^', label: '!=^  NOT LIKE' },
  { value: '.=^', label: '.=^  ILIKE' },
  { value: '=~', label: '=~  regex' },
  { value: '!=~', label: '!=~  not regex' },
  { value: '<in>', label: '<in>  in list' },
  { value: '<nin>', label: '<nin>  not in list' },
  { value: '<bet>', label: '<bet>  between' },
  { value: '<null>', label: '<null>  is null' },
  { value: '<notnull>', label: '<notnull>  is not null' },
]

function valuePlaceholder(op: string): string {
  if (op === '<in>' || op === '<nin>') return 'a, b, c'
  if (op === '=^' || op === '!=^' || op === '.=^') return '%pattern%'
  if (op === '=~' || op === '!=~') return '^regex$'
  return 'value'
}

export function ConditionNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const op = String(data.op ?? '=')
  const isNull = op === '<null>' || op === '<notnull>'
  const isRange = op === '<bet>'
  return (
    <div className="node node-condition">
      <div className="node-head">Condition</div>
      <div className="node-body">
        <input
          className="nodrag"
          placeholder="field"
          value={String(data.field ?? '')}
          onChange={(e) => updateNodeData(id, { field: e.target.value })}
        />
        <select
          className="nodrag"
          value={op}
          onChange={(e) => updateNodeData(id, { op: e.target.value })}
        >
          {OPERATORS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        {!isNull && !isRange && (
          <input
            className="nodrag"
            placeholder={valuePlaceholder(op)}
            value={String(data.value ?? '')}
            onChange={(e) => updateNodeData(id, { value: e.target.value })}
          />
        )}
        {isRange && (
          <div className="range-row">
            <input
              className="nodrag"
              placeholder="min"
              value={String(data.value ?? '')}
              onChange={(e) => updateNodeData(id, { value: e.target.value })}
            />
            <span className="range-sep">..</span>
            <input
              className="nodrag"
              placeholder="max"
              value={String(data.value2 ?? '')}
              onChange={(e) => updateNodeData(id, { value2: e.target.value })}
            />
          </div>
        )}
      </div>
      <Handle type="source" position={Position.Right} id="expr" />
    </div>
  )
}

export function LogicNode({ data }: NodeProps) {
  const kind = String(data.kind ?? 'and')
  return (
    <div className={`node node-logic node-logic-${kind}`}>
      <Handle type="target" position={Position.Left} id="in" />
      <div className="logic-label">{kind}</div>
      <div className="logic-hint">
        {kind === 'not' ? 'one input' : 'inputs merge top → bottom'}
      </div>
      <Handle type="source" position={Position.Right} id="expr" />
    </div>
  )
}

export function SortNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  return (
    <div className="node node-sort">
      <div className="node-head">Sort</div>
      <div className="node-body row">
        <input
          className="nodrag"
          placeholder="field"
          value={String(data.field ?? '')}
          onChange={(e) => updateNodeData(id, { field: e.target.value })}
        />
        <select
          className="nodrag"
          value={String(data.dir ?? 'asc')}
          onChange={(e) => updateNodeData(id, { dir: e.target.value })}
        >
          <option value="asc">asc</option>
          <option value="desc">desc</option>
        </select>
      </div>
      <Handle type="source" position={Position.Right} id="sort" />
    </div>
  )
}

export function PageNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  return (
    <div className="node node-page">
      <div className="node-head">Page</div>
      <div className="node-body row">
        <label>
          skip
          <input
            className="nodrag"
            type="number"
            min={0}
            value={Number(data.skip ?? 0)}
            onChange={(e) => updateNodeData(id, { skip: Number(e.target.value) })}
          />
        </label>
        <label>
          take
          <input
            className="nodrag"
            type="number"
            min={0}
            value={Number(data.take ?? 20)}
            onChange={(e) => updateNodeData(id, { take: Number(e.target.value) })}
          />
        </label>
      </div>
      <Handle type="source" position={Position.Right} id="page" />
    </div>
  )
}

export function LoadNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  return (
    <div className="node node-load">
      <div className="node-head">Load (preload)</div>
      <div className="node-body">
        <input
          className="nodrag"
          placeholder="Relation (e.g. Orders)"
          value={String(data.relation ?? '')}
          onChange={(e) => updateNodeData(id, { relation: e.target.value })}
        />
        <div className="handle-row">
          <Handle type="target" position={Position.Left} id="filter" />
          <span>filter</span>
        </div>
      </div>
      <Handle type="source" position={Position.Right} id="load" />
    </div>
  )
}

export function RootNode(_: NodeProps) {
  return (
    <div className="node node-root">
      <div className="node-head">Query</div>
      <div className="node-body">
        <div className="handle-row">
          <Handle type="target" position={Position.Left} id="filter" />
          <span>filter</span>
        </div>
        <div className="handle-row">
          <Handle type="target" position={Position.Left} id="sort" />
          <span>sort</span>
        </div>
        <div className="handle-row">
          <Handle type="target" position={Position.Left} id="page" />
          <span>page</span>
        </div>
        <div className="handle-row">
          <Handle type="target" position={Position.Left} id="load" />
          <span>load</span>
        </div>
      </div>
    </div>
  )
}

export const nodeTypes = {
  condition: ConditionNode,
  logic: LogicNode,
  sort: SortNode,
  page: PageNode,
  load: LoadNode,
  root: RootNode,
}
