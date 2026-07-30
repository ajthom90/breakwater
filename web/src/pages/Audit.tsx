import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { ErrorBox } from '../components/Layout'

/** Audit screen — cheap table over the real audit API (not a stub). */
export function AuditPage() {
  const q = useQuery({
    queryKey: ['audit'],
    queryFn: () => api.audit(100),
    refetchInterval: 30_000,
  })

  if (q.isError) return <ErrorBox error={q.error} />
  if (q.isLoading || !q.data) {
    return <div className="text-sm text-[var(--text-muted)]">Loading audit events…</div>
  }

  const { events, chain_ok, chain_err } = q.data

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-xl font-semibold">Audit</h2>
          <p className="mt-1 text-sm text-[var(--text-muted)]">
            Hash-chained admin/security events. Read-only GETs are not written here (noise policy).
          </p>
        </div>
        <span
          className={`rounded-full px-3 py-1 text-xs font-medium ${
            chain_ok
              ? 'bg-[var(--success)]/20 text-[var(--success)]'
              : 'bg-[var(--danger)]/20 text-[var(--danger)]'
          }`}
        >
          {chain_ok ? 'Chain OK' : 'Chain broken'}
        </span>
      </div>

      {!chain_ok && chain_err ? (
        <div className="rounded border border-[var(--danger)]/40 bg-[var(--danger)]/10 p-3 text-sm text-[var(--danger)]">
          {chain_err}
        </div>
      ) : null}

      {events.length === 0 ? (
        <p className="text-sm text-[var(--text-muted)]">No audit events yet.</p>
      ) : (
        <div className="overflow-hidden rounded-lg border border-[var(--border)]">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
              <tr>
                <th className="px-4 py-2">Time</th>
                <th className="px-4 py-2">Action</th>
                <th className="px-4 py-2">Actor</th>
                <th className="px-4 py-2">Target</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id} className="border-b border-[var(--border)] last:border-0">
                  <td className="px-4 py-2 whitespace-nowrap text-[var(--text-muted)]">{e.ts}</td>
                  <td className="px-4 py-2 font-medium">{e.action}</td>
                  <td className="px-4 py-2 text-xs">
                    <span className="text-[var(--text-muted)]">{e.actor_type}:</span> {e.actor}
                  </td>
                  <td className="max-w-xs truncate px-4 py-2 font-mono text-xs text-[var(--text-muted)]">
                    {e.target}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
