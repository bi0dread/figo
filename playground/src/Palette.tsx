export interface PaletteItem {
  type: 'condition' | 'logic' | 'sort' | 'page' | 'load'
  kind?: 'and' | 'or' | 'not'
  label: string
  hint: string
  className: string
}

export const PALETTE: { section: string; items: PaletteItem[] }[] = [
  {
    section: 'Filter',
    items: [
      {
        type: 'condition',
        label: 'Condition',
        hint: 'field <op> value',
        className: 'pal-condition',
      },
      { type: 'logic', kind: 'and', label: 'AND', hint: 'all must match', className: 'pal-and' },
      { type: 'logic', kind: 'or', label: 'OR', hint: 'any may match', className: 'pal-or' },
      { type: 'logic', kind: 'not', label: 'NOT', hint: 'negate input', className: 'pal-not' },
    ],
  },
  {
    section: 'Directives',
    items: [
      { type: 'sort', label: 'Sort', hint: 'sort=field:dir', className: 'pal-sort' },
      { type: 'page', label: 'Page', hint: 'page=skip,take', className: 'pal-page' },
      { type: 'load', label: 'Load', hint: 'load=[Rel:filter]', className: 'pal-load' },
    ],
  },
]

export function encodeItem(item: PaletteItem): string {
  return JSON.stringify({ type: item.type, kind: item.kind })
}

interface Props {
  onAdd: (item: PaletteItem) => void
}

export function Palette({ onAdd }: Props) {
  return (
    <aside className="palette">
      {PALETTE.map((group) => (
        <div key={group.section} className="palette-group">
          <div className="palette-title">{group.section}</div>
          {group.items.map((item) => (
            <div
              key={item.label}
              className={`palette-item ${item.className}`}
              draggable
              onDragStart={(e) => {
                e.dataTransfer.setData('application/figo-node', encodeItem(item))
                e.dataTransfer.effectAllowed = 'move'
              }}
              onClick={() => onAdd(item)}
              title="Drag onto the canvas (or click to add)"
            >
              <span className="palette-label">{item.label}</span>
              <span className="palette-hint">{item.hint}</span>
            </div>
          ))}
        </div>
      ))}
      <div className="palette-help">
        Drag blocks onto the canvas, wire them into the <b>Query</b> node, and the DSL updates
        live. Inputs merge top&nbsp;→&nbsp;bottom. Backspace deletes a selection.
      </div>
    </aside>
  )
}
