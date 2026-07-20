import {
  BaseEdge,
  EdgeLabelRenderer,
  getBezierPath,
  useReactFlow,
  type EdgeProps,
} from '@xyflow/react'

/** Bezier edge with a small × pip at its midpoint; clicking it removes the connection. */
export function PipEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
}: EdgeProps) {
  const { setEdges } = useReactFlow()
  const [path, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  })
  return (
    <>
      <BaseEdge id={id} path={path} style={style} markerEnd={markerEnd} />
      <EdgeLabelRenderer>
        <button
          className="edge-del nodrag nopan"
          style={{ transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)` }}
          title="Remove connection"
          onClick={(e) => {
            e.stopPropagation()
            setEdges((eds) => eds.filter((edge) => edge.id !== id))
          }}
        >
          ×
        </button>
      </EdgeLabelRenderer>
    </>
  )
}

export const edgeTypes = {
  pip: PipEdge,
}
