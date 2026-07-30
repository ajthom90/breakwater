import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { ErrorBox, Tile } from '../components/Layout'

export function DashboardPage() {
  const q = useQuery({ queryKey: ['summary'], queryFn: api.summary, refetchInterval: 10_000 })

  if (q.isError) return <ErrorBox error={q.error} />
  if (q.isLoading || !q.data) {
    return <div className="text-sm text-[var(--text-muted)]">Loading fleet summary…</div>
  }
  const s = q.data

  return (
    <div className="space-y-8">
      <div>
        <h2 className="text-xl font-semibold">Fleet health</h2>
        <p className="mt-1 text-sm text-[var(--text-muted)]">
          Live counts from the catalog via <code className="text-xs">GET /api/v1/summary</code>.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Machines online" value={s.machines_online} hint={`${s.machines_total} total`} />
        <Tile label="Machines offline" value={s.machines_offline} />
        <Tile
          label="Jobs last 24h"
          value={s.jobs_last_24h}
          hint={`${s.jobs_success_24h} ok · ${s.jobs_failed_24h} failed · ${s.jobs_running} running`}
        />
        <Tile label="Snapshots" value={s.snapshots_total} />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <Tile
          label="Storage capacity"
          value="—"
          placeholder
          hint={s.capacity_note || 'Not implemented in M2'}
        />
        <Tile
          label="Dedup ratio (fleet)"
          value="—"
          placeholder
          hint="Not implemented in M2 — per-job bytes_stored available on Activity"
        />
      </div>
    </div>
  )
}
