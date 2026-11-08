import { Fragment, useEffect, useMemo, useState } from 'react'
import { CUSTOMER_FIELDS, CUSTOMERS, type Customer } from './sampleData'
import { previewDsl } from './preview'
import { SAMPLES } from './samples'

const dollars = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 })
const nullable = (value: string | null) => value ?? 'NULL'

export function DataPreview({ dsl, sampleId, revision, onSample }: {
  dsl: string; sampleId: string; revision: number; onSample: (id: string) => void
}) {
  const [editedDsl, setEditedDsl] = useState<string | null>(null)
  const [showAll, setShowAll] = useState(false)
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  useEffect(() => { setEditedDsl(null) }, [revision])
  const activeDsl = editedDsl ?? dsl
  const result = useMemo(() => {
    try { return { preview: previewDsl(activeDsl, CUSTOMERS), error: null } }
    catch (error) { return { preview: null, error: error instanceof Error ? error.message : 'This filter could not be applied.' } }
  }, [activeDsl])
  const { preview, error } = result
  const rows = preview ? showAll ? CUSTOMERS : preview.rows : []
  const sample = SAMPLES.find((item) => item.id === sampleId) ?? SAMPLES[0]
  const toggle = (id: number) => setExpanded((previous) => {
    const next = new Set(previous); if (next.has(id)) next.delete(id); else next.add(id); return next
  })
  const matchLabel = (row: Customer) => !preview?.matchingIds.has(row.id) ? 'Filtered out' : !preview.visibleIds.has(row.id) ? 'Outside page' : 'In results'

  return (
    <section id="live-data" className="data-preview" aria-labelledby="data-preview-title">
      <div className="preview-heading">
        <div>
          <p className="preview-eyebrow">DSL → sample data</p>
          <h2 id="data-preview-title">See your filter in action</h2>
          <p>Change the blocks above or edit the DSL below. The table updates immediately.</p>
        </div>
        <label className="sample-picker">
          Load an example
          <select value={sampleId} onChange={(event) => {
            setEditedDsl(null); setShowAll(false); onSample(event.target.value)
          }}>
            {SAMPLES.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
          </select>
        </label>
      </div>
      <p className="sample-description">{sample.description}</p>
      <div className="preview-editor-heading">
        <label htmlFor="preview-dsl">DSL applied to the table</label>
        <label className="follow-builder">
          <input type="checkbox" checked={editedDsl === null} onChange={(event) => setEditedDsl(event.target.checked ? null : dsl)} />
          Follow visual builder
        </label>
      </div>
      <textarea id="preview-dsl" value={activeDsl} maxLength={12000} rows={3} spellCheck={false}
        placeholder='status="active" sort=mrr:desc page=skip:0,take:5'
        onChange={(event) => setEditedDsl(event.target.value)} />
      <div className="preview-summary" aria-live="polite" aria-atomic="true">
        {error ? <p className="preview-error" role="alert">{error}</p> : preview && <>
          <span><strong>{preview.matched}</strong> of {CUSTOMERS.length} accounts match</span>
          <span><strong>{preview.rows.length}</strong> returned after pagination</span>
          <span>skip: {preview.skip} · take: {preview.take || 'all'}</span>
        </>}
      </div>
      <div className="preview-toolbar">
        <div className="preview-tabs" role="group" aria-label="Table view">
          <button aria-pressed={!showAll} onClick={() => setShowAll(false)}>Query results</button>
          <button aria-pressed={showAll} onClick={() => setShowAll(true)}>All source rows</button>
        </div>
        <span>{showAll ? 'Original data; badges show which rows the query returns.' : preview?.loadsOrders ? 'Expand Orders to see the filtered relation.' : 'Expand Orders to inspect related data.'}</span>
      </div>
      <div className="preview-table-scroll" tabIndex={0} role="region" aria-label="Customer sample table">
        <table className="preview-table">
          <caption className="visually-hidden">{showAll ? 'Unfiltered source accounts with query membership' : 'Accounts returned by the current DSL'}</caption>
          <thead><tr>
            <th scope="col">Account</th><th scope="col">Status</th><th scope="col">Plan / seats</th>
            <th scope="col">MRR</th><th scope="col">Signup date</th><th scope="col">Last login</th>
            <th scope="col">Deleted at</th><th scope="col">Orders</th>{showAll && <th scope="col">Query result</th>}
          </tr></thead>
          <tbody>
            {rows.map((row) => <Fragment key={row.id}>
              <tr className={showAll && !preview?.visibleIds.has(row.id) ? 'row-excluded' : undefined}>
                <th scope="row"><span>{row.name}</span><small>#{row.id} · {row.email}</small></th>
                <td><span className={`status-badge status-${row.status}`}>{row.status}</span></td>
                <td>{row.plan}<small>{row.seats} seats</small></td>
                <td className="numeric">{dollars.format(row.mrr)}</td>
                <td>{row.signup_date}</td><td className={row.last_login === null ? 'null-value' : undefined}>{nullable(row.last_login)}</td>
                <td className={row.deleted_at === null ? 'null-value' : undefined}>{nullable(row.deleted_at)}</td>
                <td><button className="orders-toggle" aria-expanded={expanded.has(row.id)} aria-controls={`orders-${row.id}`} onClick={() => toggle(row.id)}>
                  {row.Orders.length} {preview?.loadsOrders && !showAll ? 'loaded' : 'orders'} {expanded.has(row.id) ? '−' : '+'}
                </button></td>
                {showAll && <td><span className={`match-badge ${preview?.visibleIds.has(row.id) ? 'match-yes' : ''}`}>{matchLabel(row)}</span></td>}
              </tr>
              {expanded.has(row.id) && <tr id={`orders-${row.id}`} className="orders-detail"><td colSpan={showAll ? 9 : 8}>
                <strong>{row.name} · Orders</strong>
                {!showAll && preview?.loadsOrders && <p>Only related orders matching the load filter are shown. The customer stays in the result even if no orders match.</p>}
                {row.Orders.length ? <table aria-label={`Orders for ${row.name}`}>
                  <thead><tr><th scope="col">Order</th><th scope="col">Total</th><th scope="col">Status</th></tr></thead>
                  <tbody>{row.Orders.map((order) => <tr key={order.id}><td>#{order.id}</td><td>{dollars.format(order.total)}</td><td>{order.status}</td></tr>)}</tbody>
                </table> : <p>No orders to display.</p>}
              </td></tr>}
            </Fragment>)}
            {rows.length === 0 && <tr><td colSpan={showAll ? 9 : 8} className="preview-empty">
              {error ? 'Fix the DSL above to see results.' : preview?.matched ? 'No rows on this page. Reduce skip or change take.' : 'No accounts match. Try a different condition or clear the DSL to show all accounts.'}
            </td></tr>}
          </tbody>
        </table>
      </div>
      <details className="preview-fields"><summary>Available fields and demo behavior</summary>
        <p>Accounts: <code>{CUSTOMER_FIELDS.join(', ')}</code>. Orders: <code>id, total, status</code>.</p>
        <p>This browser demo parses the DSL and uses typed comparisons, SQL-style NULL rules, and NULLs-last sorting. It supports comparisons, IN / NOT IN, BETWEEN, LIKE, AND / OR / NOT, sorting, pagination, and one Orders preload. Regex operators are not evaluated. Your database adapter can have different collation, date, or NULL ordering behavior.</p>
      </details>
    </section>
  )
}
