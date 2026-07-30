/** REST client for Breakwater :8443 /api/v1.
 *
 * M2 auth: dev-only local API token (Authorization: Bearer).
 * Store in localStorage key `bw_api_token` for the shell; M6 replaces with sessions.
 */

const TOKEN_KEY = 'bw_api_token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string): void {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const res = await fetch(path, { ...init, headers })
  if (!res.ok) {
    let msg = res.statusText
    try {
      const body = (await res.json()) as { error?: string }
      if (body.error) msg = body.error
    } catch {
      /* ignore */
    }
    throw new ApiError(res.status, msg)
  }
  return res.json() as Promise<T>
}

export type Machine = {
  id: string
  hostname: string
  os: string
  status: string
  last_seen: string | null
  agent_version: string
}

export type InventoryItem = {
  kind: string
  external_id: string
  name: string
  details?: Record<string, unknown>
  rct_capable: boolean
}

export type MachineDetail = Machine & {
  inventory: InventoryItem[]
}

export type Job = {
  id: string
  machine_id: string
  type: string
  state: string
  started_at: string | null
  finished_at: string | null
  bytes_read: number
  bytes_stored: number
  error_message?: string
  created_at: string
}

export type Snapshot = {
  id: string
  machine_id: string
  kind: string
  source: string
  bytes_read: number
  bytes_stored: number
  job_id?: string
  created_at: string
}

export type AuditEvent = {
  id: string
  ts: string
  actor: string
  actor_type: string
  action: string
  target: string
  detail: string
}

export type Summary = {
  machines_total: number
  machines_online: number
  machines_offline: number
  jobs_last_24h: number
  jobs_success_24h: number
  jobs_failed_24h: number
  jobs_running: number
  snapshots_total: number
  capacity_bytes: number | null
  capacity_note: string
}

export type JobEvent = {
  job_id: string
  machine_id: string
  type: string
  state: string
  bytes_read?: number
  bytes_stored?: number
  error_message?: string
}

export const api = {
  summary: () => request<Summary>('/api/v1/summary'),
  machines: () => request<{ machines: Machine[] }>('/api/v1/machines'),
  machine: (id: string) => request<MachineDetail>(`/api/v1/machines/${encodeURIComponent(id)}`),
  jobs: (q?: { machine_id?: string; state?: string; limit?: number }) => {
    const p = new URLSearchParams()
    if (q?.machine_id) p.set('machine_id', q.machine_id)
    if (q?.state) p.set('state', q.state)
    if (q?.limit) p.set('limit', String(q.limit))
    const qs = p.toString()
    return request<{ jobs: Job[] }>(`/api/v1/jobs${qs ? `?${qs}` : ''}`)
  },
  snapshots: (q?: { machine_id?: string; limit?: number }) => {
    const p = new URLSearchParams()
    if (q?.machine_id) p.set('machine_id', q.machine_id)
    if (q?.limit) p.set('limit', String(q.limit))
    const qs = p.toString()
    return request<{ snapshots: Snapshot[] }>(`/api/v1/snapshots${qs ? `?${qs}` : ''}`)
  },
  audit: (limit = 50) =>
    request<{ events: AuditEvent[]; chain_ok: boolean; chain_err?: string }>(
      `/api/v1/audit?limit=${limit}`,
    ),
}

/** Open SSE stream for live job events. Uses ?token= because EventSource cannot set headers. */
export function openJobEvents(
  onEvent: (ev: JobEvent) => void,
  onError?: (err: Event) => void,
): () => void {
  const token = getToken()
  const url = `/api/v1/events?token=${encodeURIComponent(token)}`
  const es = new EventSource(url)
  es.addEventListener('job', (e) => {
    try {
      const data = JSON.parse((e as MessageEvent).data) as JobEvent
      onEvent(data)
    } catch {
      /* ignore malformed */
    }
  })
  es.onerror = (e) => {
    onError?.(e)
  }
  return () => es.close()
}
