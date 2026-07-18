import { useEffect, useState, type ReactNode } from 'react'
import type { DslResult } from './dsl'

const TOKEN_RE =
  /("(?:[^"\\]|\\.)*")|(\b(?:sort|page|load)=)|(\b(?:and|or|not)\b)|(<in>|<nin>|<bet>|<null>|<notnull>|!=\^|\.=\^|=\^|!=~|=~|!=|>=|<=|>|<|=)|(-?\d+(?:\.\d+)?)/g

function highlight(dsl: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0
  let key = 0
  for (const m of dsl.matchAll(TOKEN_RE)) {
    const idx = m.index ?? 0
    if (idx > last) out.push(<span key={key++}>{dsl.slice(last, idx)}</span>)
    const cls = m[1] ? 'tok-str' : m[2] ? 'tok-dir' : m[3] ? 'tok-kw' : m[4] ? 'tok-op' : 'tok-num'
    out.push(
      <span key={key++} className={cls}>
        {m[0]}
      </span>,
    )
    last = idx + m[0].length
  }
  if (last < dsl.length) out.push(<span key={key++}>{dsl.slice(last)}</span>)
  return out
}

export function DslPanel({ result }: { result: DslResult }) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 1200)
    return () => clearTimeout(t)
  }, [copied])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(result.dsl)
      setCopied(true)
    } catch {
      /* clipboard unavailable (e.g. insecure context) — nothing to do */
    }
  }

  return (
    <div className="dsl-panel">
      <div className="dsl-row">
        <span className="dsl-tag">DSL</span>
        <code className="dsl-code">
          {result.dsl === '' ? (
            <span className="dsl-empty">
              connect blocks to the Query node to build a filter…
            </span>
          ) : (
            highlight(result.dsl)
          )}
        </code>
        <button className="dsl-copy" onClick={copy} disabled={result.dsl === ''}>
          {copied ? 'copied ✓' : 'copy'}
        </button>
      </div>
      {result.warnings.length > 0 && (
        <div className="dsl-warnings">
          {result.warnings.map((w) => (
            <span key={w}>⚠ {w}</span>
          ))}
        </div>
      )}
    </div>
  )
}
