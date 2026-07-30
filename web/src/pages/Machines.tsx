import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { api } from '../lib/api'
import { ErrorBox, StatusPill, formatBytes, formatTime } from '../components/Layout'

export function MachinesPage() {
  const q = useQuery({ queryKey: ['machines'], queryFn: api.machines, refetchInterval: 10_000 })

  if (q.isError) return <ErrorBox error={q.error} />
  if (q.isLoading || !q.data) {
    return <div className="text-sm text-[var(--text-muted)]">Loading machines…</div>
  }

  const machines = q.data.machines
  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-xl font-semibold">Machines</h2>
        <p className="mt-1 text-sm text-[var(--text-muted)]">
          Enrolled agents from the real catalog. Empty means no agents have enrolled yet.
        </p>
      </div>

      {machines.length === 0 ? (
        <div className="rounded-lg border border-[var(--border)] bg-[var(--bg-elevated)] p-8 text-sm text-[var(--text-muted)]">
          No machines enrolled. Install the agent with an enrollment token; it will appear here when
          the control channel connects.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-[var(--border)]">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
              <tr>
                <th className="px-4 py-3 font-medium">Hostname</th>
                <th className="px-4 py-3 font-medium">OS</th>
                <th className="px-4 py-3 font-medium">Status</th>
                <th className="px-4 py-3 font-medium">Last seen</th>
                <th className="px-4 py-3 font-medium">Agent</th>
              </tr>
            </thead>
            <tbody>
              {machines.map((m) => (
                <tr key={m.id} className="border-b border-[var(--border)] last:border-0 hover:bg-[var(--bg-hover)]">
                  <td className="px-4 py-3">
                    <Link to="/machines/$id" params={{ id: m.id }} className="font-medium text-[var(--accent)] hover:underline">
                      {m.hostname || m.id}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-[var(--text-muted)]">{m.os || '—'}</td>
                  <td className="px-4 py-3">
                    <StatusPill status={m.status} />
                  </td>
                  <td className="px-4 py-3 text-[var(--text-muted)]">{formatTime(m.last_seen)}</td>
                  <td className="px-4 py-3 text-[var(--text-muted)]">{m.agent_version || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

export function MachineDetailPage() {
  const { id } = useParams({ from: '/machines/$id' })
  const machine = useQuery({ queryKey: ['machine', id], queryFn: () => api.machine(id) })
  const jobs = useQuery({
    queryKey: ['jobs', id],
    queryFn: () => api.jobs({ machine_id: id, limit: 20 }),
  })
  const snaps = useQuery({
    queryKey: ['snapshots', id],
    queryFn: () => api.snapshots({ machine_id: id, limit: 20 }),
  })

  if (machine.isError) return <ErrorBox error={machine.error} />
  if (machine.isLoading || !machine.data) {
    return <div className="text-sm text-[var(--text-muted)]">Loading machine…</div>
  }
  const m = machine.data

  return (
    <div className="space-y-8">
      <div>
        <Link to="/machines" className="text-xs text-[var(--accent)] hover:underline">
          ← Machines
        </Link>
        <div className="mt-2 flex items-center gap-3">
          <h2 className="text-xl font-semibold">{m.hostname || m.id}</h2>
          <StatusPill status={m.status} />
        </div>
        <p className="mt-1 text-sm text-[var(--text-muted)]">
          {m.os || 'unknown OS'} · agent {m.agent_version || '—'} · last seen {formatTime(m.last_seen)}
        </p>
        <p className="mt-0.5 font-mono text-xs text-[var(--text-muted)]">{m.id}</p>
      </div>

      <section>
        <h3 className="mb-3 text-sm font-semibold uppercase tracking-wide text-[var(--text-muted)]">
          Inventory
        </h3>
        {m.inventory?.length ? (
          <div className="overflow-hidden rounded-lg border border-[var(--border)]">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
                <tr>
                  <th className="px-4 py-2">Kind</th>
                  <th className="px-4 py-2">Name</th>
                  <th className="px-4 py-2">External ID</th>
                </tr>
              </thead>
              <tbody>
                {m.inventory.map((it) => (
                  <tr key={`${it.kind}:${it.external_id}`} className="border-b border-[var(--border)] last:border-0">
                    <td className="px-4 py-2">{it.kind}</td>
                    <td className="px-4 py-2">{it.name}</td>
                    <td className="px-4 py-2 font-mono text-xs text-[var(--text-muted)]">{it.external_id}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-sm text-[var(--text-muted)]">No inventory reported yet.</p>
        )}
      </section>

      <section>
        <h3 className="mb-3 text-sm font-semibold uppercase tracking-wide text-[var(--text-muted)]">
          Job history
        </h3>
        {jobs.isError ? (
          <ErrorBox error={jobs.error} />
        ) : jobs.data?.jobs.length ? (
          <div className="overflow-hidden rounded-lg border border-[var(--border)]">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
                <tr>
                  <th className="px-4 py-2">Type</th>
                  <th className="px-4 py-2">State</th>
                  <th className="px-4 py-2">Stored</th>
                  <th className="px-4 py-2">Created</th>
                </tr>
              </thead>
              <tbody>
                {jobs.data.jobs.map((j) => (
                  <tr key={j.id} className="border-b border-[var(--border)] last:border-0">
                    <td className="px-4 py-2">{j.type}</td>
                    <td className="px-4 py-2">
                      <StatusPill status={j.state} />
                    </td>
                    <td className="px-4 py-2 tabular-nums">{formatBytes(j.bytes_stored)}</td>
                    <td className="px-4 py-2 text-[var(--text-muted)]">{formatTime(j.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-sm text-[var(--text-muted)]">No jobs yet.</p>
        )}
      </section>

      <section>
        <h3 className="mb-3 text-sm font-semibold uppercase tracking-wide text-[var(--text-muted)]">
          Snapshot timeline
        </h3>
        {snaps.isError ? (
          <ErrorBox error={snaps.error} />
        ) : snaps.data?.snapshots.length ? (
          <div className="overflow-hidden rounded-lg border border-[var(--border)]">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
                <tr>
                  <th className="px-4 py-2">Kind</th>
                  <th className="px-4 py-2">Source</th>
                  <th className="px-4 py-2">Stored</th>
                  <th className="px-4 py-2">Created</th>
                </tr>
              </thead>
              <tbody>
                {snaps.data.snapshots.map((s) => (
                  <tr key={s.id} className="border-b border-[var(--border)] last:border-0">
                    <td className="px-4 py-2">{s.kind}</td>
                    <td className="px-4 py-2 font-mono text-xs">{s.source || '—'}</td>
                    <td className="px-4 py-2 tabular-nums">{formatBytes(s.bytes_stored)}</td>
                    <td className="px-4 py-2 text-[var(--text-muted)]">{formatTime(s.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-sm text-[var(--text-muted)]">No snapshots yet.</p>
        )}
      </section>
    </div>
  )
}
