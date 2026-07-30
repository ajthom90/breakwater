import { useState } from 'react'
import { getToken, setToken } from '../lib/api'
import { Placeholder } from '../components/Layout'

export function RestorePage() {
  return (
    <Placeholder
      title="Restore"
      reason="Snapshot browser, file restore, and BMR token generation are Phase 1 later milestones. Not implemented in M2."
    />
  )
}

export function SettingsPage() {
  const [token, setLocal] = useState(getToken())
  const [saved, setSaved] = useState(false)

  return (
    <div className="space-y-8">
      <Placeholder
        title="Settings"
        reason="Users, TOTP, policy editor, alerts, and replication peers are M6 / Phase 2. Not implemented in M2."
      />

      <section className="rounded-lg border border-[var(--border)] bg-[var(--bg-elevated)] p-6">
        <h3 className="text-sm font-semibold">Dev API token (M2 only)</h3>
        <p className="mt-1 text-sm text-[var(--text-muted)]">
          Read the token from <code className="text-[var(--text)]">&lt;dataDir&gt;/api-token</code> on
          the server host (generated on first boot, mode 0600). This is the single middleware
          attachment point for real sessions in M6 — not production multi-admin auth.
        </p>
        <form
          className="mt-4 flex flex-wrap gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            setToken(token.trim())
            setSaved(true)
          }}
        >
          <input
            type="password"
            value={token}
            onChange={(e) => {
              setLocal(e.target.value)
              setSaved(false)
            }}
            placeholder="Paste API token"
            className="min-w-[16rem] flex-1 rounded border border-[var(--border)] bg-[var(--bg)] px-3 py-2 text-sm"
            autoComplete="off"
          />
          <button
            type="submit"
            className="rounded bg-[var(--accent)] px-4 py-2 text-sm font-medium text-white hover:bg-[var(--accent-dim)]"
          >
            Save token
          </button>
        </form>
        {saved ? (
          <p className="mt-2 text-xs text-[var(--success)]">Token saved in this browser (localStorage).</p>
        ) : null}
      </section>
    </div>
  )
}
