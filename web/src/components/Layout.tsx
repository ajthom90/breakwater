import { Link, useRouterState } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { setToken } from '../lib/api'

const nav = [
  { to: '/', label: 'Dashboard' },
  { to: '/machines', label: 'Machines' },
  { to: '/activity', label: 'Activity' },
  { to: '/restore', label: 'Restore', stub: true },
  { to: '/audit', label: 'Audit' },
  { to: '/settings', label: 'Settings', stub: true },
] as const

export function Layout({ children }: { children: ReactNode }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })

  return (
    <div className="flex min-h-full">
      <aside className="flex w-56 shrink-0 flex-col border-r border-[var(--border)] bg-[var(--bg-elevated)]">
        <div className="border-b border-[var(--border)] px-5 py-5">
          <div className="text-lg font-semibold tracking-tight">Breakwater</div>
          <div className="mt-0.5 text-xs text-[var(--text-muted)]">Backup appliance</div>
        </div>
        <nav className="flex flex-1 flex-col gap-0.5 p-3">
          {nav.map((item) => {
            const active =
              item.to === '/'
                ? pathname === '/'
                : pathname === item.to || pathname.startsWith(item.to + '/')
            return (
              <Link
                key={item.to}
                to={item.to}
                className={`rounded-md px-3 py-2 text-sm transition-colors ${
                  active
                    ? 'bg-[var(--accent-dim)]/40 text-white'
                    : 'text-[var(--text-muted)] hover:bg-[var(--bg-hover)] hover:text-[var(--text)]'
                }`}
              >
                {item.label}
                {'stub' in item && item.stub ? (
                  <span className="ml-2 text-[10px] uppercase tracking-wide text-[var(--placeholder)]">
                    stub
                  </span>
                ) : null}
              </Link>
            )
          })}
        </nav>
        <div className="border-t border-[var(--border)] p-3 text-[10px] leading-relaxed text-[var(--text-muted)]">
          M2 shell · dev API token auth
          <br />
          Real sessions land in M6
        </div>
      </aside>
      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-[var(--border)] px-8 py-4">
          <h1 className="text-base font-medium text-[var(--text-muted)]">
            {nav.find((n) =>
              n.to === '/' ? pathname === '/' : pathname.startsWith(n.to),
            )?.label ?? 'Breakwater'}
          </h1>
        </header>
        <div className="flex-1 overflow-auto p-8">{children}</div>
      </main>
    </div>
  )
}

export function Placeholder({ title, reason }: { title: string; reason?: string }) {
  return (
    <div className="rounded-lg border border-dashed border-[var(--placeholder)]/60 bg-[var(--placeholder)]/10 p-8">
      <div className="text-sm font-semibold uppercase tracking-wide text-[var(--placeholder)]">
        Not implemented in M2
      </div>
      <h2 className="mt-2 text-xl font-semibold">{title}</h2>
      <p className="mt-2 max-w-xl text-sm text-[var(--text-muted)]">
        {reason ??
          'This screen is intentionally stubbed. PLAN M2 is "UI shell against fake API" — real restore, settings, and multi-admin arrive in later milestones.'}
      </p>
    </div>
  )
}

export function Tile({
  label,
  value,
  hint,
  placeholder,
}: {
  label: string
  value: string | number
  hint?: string
  placeholder?: boolean
}) {
  return (
    <div
      className={`rounded-lg border bg-[var(--bg-elevated)] p-5 ${
        placeholder ? 'border-[var(--placeholder)]/50' : 'border-[var(--border)]'
      }`}
    >
      <div className="text-xs font-medium uppercase tracking-wide text-[var(--text-muted)]">
        {label}
        {placeholder ? (
          <span className="ml-2 text-[var(--placeholder)]">placeholder</span>
        ) : null}
      </div>
      <div className="mt-2 text-3xl font-semibold tabular-nums">{value}</div>
      {hint ? <div className="mt-1 text-xs text-[var(--text-muted)]">{hint}</div> : null}
    </div>
  )
}

export function StatusPill({ status }: { status: string }) {
  const color =
    status === 'active' || status === 'success'
      ? 'bg-[var(--success)]/20 text-[var(--success)]'
      : status === 'failed' || status === 'removed'
        ? 'bg-[var(--danger)]/20 text-[var(--danger)]'
        : status === 'running' || status === 'cancelling'
          ? 'bg-[var(--accent)]/20 text-[var(--accent)]'
          : 'bg-[var(--bg-hover)] text-[var(--text-muted)]'
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium capitalize ${color}`}>
      {status}
    </span>
  )
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MiB`
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GiB`
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

export function ErrorBox({ error }: { error: Error }) {
  const unauthorized = 'status' in error && (error as { status?: number }).status === 401
  return (
    <div className="rounded-lg border border-[var(--danger)]/40 bg-[var(--danger)]/10 p-4 text-sm">
      <div className="font-medium text-[var(--danger)]">Request failed</div>
      <div className="mt-1 text-[var(--text-muted)]">{error.message}</div>
      {unauthorized ? (
        <div className="mt-3 text-xs text-[var(--text-muted)]">
          Set the dev API token (from <code className="text-[var(--text)]">&lt;dataDir&gt;/api-token</code>
          ) below, or open{' '}
          <Link to="/settings" className="text-[var(--accent)] underline">
            Settings
          </Link>
          .
          <TokenForm />
        </div>
      ) : null}
    </div>
  )
}

function TokenForm() {
  return (
    <form
      className="mt-3 flex gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        const fd = new FormData(e.currentTarget)
        const t = String(fd.get('token') ?? '').trim()
        setTokenFromForm(t)
        window.location.reload()
      }}
    >
      <input
        name="token"
        type="password"
        placeholder="API token"
        className="flex-1 rounded border border-[var(--border)] bg-[var(--bg)] px-2 py-1.5 text-[var(--text)]"
        autoComplete="off"
      />
      <button
        type="submit"
        className="rounded bg-[var(--accent)] px-3 py-1.5 text-white hover:bg-[var(--accent-dim)]"
      >
        Save
      </button>
    </form>
  )
}

function setTokenFromForm(t: string) {
  setToken(t)
}
