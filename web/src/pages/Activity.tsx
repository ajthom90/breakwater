import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api, openJobEvents, type JobEvent } from '../lib/api'
import { ErrorBox, StatusPill, formatBytes, formatTime } from '../components/Layout'

export function ActivityPage() {
  const qc = useQueryClient()
  const [live, setLive] = useState<JobEvent[]>([])
  const [sseState, setSseState] = useState<'connecting' | 'live' | 'error'>('connecting')

  const jobs = useQuery({
    queryKey: ['jobs', 'all'],
    queryFn: () => api.jobs({ limit: 50 }),
    refetchInterval: 15_000,
  })

  useEffect(() => {
    setSseState('connecting')
    const close = openJobEvents(
      (ev) => {
        setSseState('live')
        setLive((prev) => [ev, ...prev].slice(0, 40))
        void qc.invalidateQueries({ queryKey: ['jobs'] })
        void qc.invalidateQueries({ queryKey: ['summary'] })
      },
      () => setSseState('error'),
    )
    // EventSource fires open implicitly; mark live after first successful open delay
    const t = window.setTimeout(() => setSseState((s) => (s === 'connecting' ? 'live' : s)), 500)
    return () => {
      window.clearTimeout(t)
      close()
    }
  }, [qc])

  return (
    <div className="space-y-8">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-xl font-semibold">Activity</h2>
          <p className="mt-1 text-sm text-[var(--text-muted)]">
            Live job stream via SSE <code className="text-xs">GET /api/v1/events</code> plus job
            history from the catalog.
          </p>
        </div>
        <SseBadge state={sseState} />
      </div>

      <section>
        <h3 className="mb-3 text-sm font-semibold uppercase tracking-wide text-[var(--text-muted)]">
          Live events
        </h3>
        {live.length === 0 ? (
          <div className="rounded-lg border border-dashed border-[var(--border)] p-6 text-sm text-[var(--text-muted)]">
            Waiting for job state changes… Submit a backup or wait for a scheduled job.
          </div>
        ) : (
          <ul className="space-y-2">
            {live.map((ev, i) => (
              <li
                key={`${ev.job_id}-${ev.state}-${i}`}
                className="flex flex-wrap items-center gap-3 rounded-md border border-[var(--border)] bg-[var(--bg-elevated)] px-4 py-2 text-sm"
              >
                <StatusPill status={ev.state} />
                <span className="font-mono text-xs text-[var(--text-muted)]">{ev.job_id.slice(0, 10)}…</span>
                <span>{ev.type}</span>
                {ev.bytes_stored != null && ev.bytes_stored > 0 ? (
                  <span className="tabular-nums text-[var(--text-muted)]">
                    {formatBytes(ev.bytes_stored)} stored
                  </span>
                ) : null}
                {ev.error_message ? (
                  <span className="text-[var(--danger)]">{ev.error_message}</span>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section>
        <h3 className="mb-3 text-sm font-semibold uppercase tracking-wide text-[var(--text-muted)]">
          Job history
        </h3>
        {jobs.isError ? (
          <ErrorBox error={jobs.error} />
        ) : jobs.isLoading || !jobs.data ? (
          <div className="text-sm text-[var(--text-muted)]">Loading jobs…</div>
        ) : jobs.data.jobs.length === 0 ? (
          <p className="text-sm text-[var(--text-muted)]">No jobs recorded yet.</p>
        ) : (
          <div className="overflow-hidden rounded-lg border border-[var(--border)]">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-[var(--border)] bg-[var(--bg-elevated)] text-xs uppercase text-[var(--text-muted)]">
                <tr>
                  <th className="px-4 py-2">Type</th>
                  <th className="px-4 py-2">State</th>
                  <th className="px-4 py-2">Machine</th>
                  <th className="px-4 py-2">Stored</th>
                  <th className="px-4 py-2">Created</th>
                  <th className="px-4 py-2">Error</th>
                </tr>
              </thead>
              <tbody>
                {jobs.data.jobs.map((j) => (
                  <tr key={j.id} className="border-b border-[var(--border)] last:border-0">
                    <td className="px-4 py-2">{j.type}</td>
                    <td className="px-4 py-2">
                      <StatusPill status={j.state} />
                    </td>
                    <td className="px-4 py-2 font-mono text-xs text-[var(--text-muted)]">
                      {j.machine_id ? `${j.machine_id.slice(0, 8)}…` : '—'}
                    </td>
                    <td className="px-4 py-2 tabular-nums">{formatBytes(j.bytes_stored)}</td>
                    <td className="px-4 py-2 text-[var(--text-muted)]">{formatTime(j.created_at)}</td>
                    <td className="max-w-xs truncate px-4 py-2 text-xs text-[var(--danger)]">
                      {j.error_message || ''}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}

function SseBadge({ state }: { state: 'connecting' | 'live' | 'error' }) {
  const label = state === 'live' ? 'SSE live' : state === 'error' ? 'SSE error' : 'SSE connecting'
  const color =
    state === 'live'
      ? 'bg-[var(--success)]/20 text-[var(--success)]'
      : state === 'error'
        ? 'bg-[var(--danger)]/20 text-[var(--danger)]'
        : 'bg-[var(--bg-hover)] text-[var(--text-muted)]'
  return <span className={`rounded-full px-3 py-1 text-xs font-medium ${color}`}>{label}</span>
}
