import type {
  AgentResponse, ChaosResult, CityMap, Comparison, EventView,
  Incident, Metrics, Series, Snapshot,
} from './types'

/**
 * REST client.
 *
 * The credential lives in localStorage and is attached as a bearer header. In
 * development the server hands one out from /api/v1/auth/dev-session; in
 * production that endpoint 404s and the key has to be pasted in, which is the
 * behaviour you want -- a console that silently works without credentials in
 * production is a console with no credentials in production.
 */

const KEY_STORAGE = 'mirror.apiKey'

let apiKey: string | null = null
try {
  apiKey = localStorage.getItem(KEY_STORAGE)
} catch {
  // Private browsing, or storage disabled. The session still works; it just
  // has to re-bootstrap its key on every reload.
  apiKey = null
}

export function getKey(): string | null {
  return apiKey
}

export function setKey(k: string | null) {
  apiKey = k
  try {
    if (k) localStorage.setItem(KEY_STORAGE, k)
    else localStorage.removeItem(KEY_STORAGE)
  } catch {
    /* ignore */
  }
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (apiKey) headers.set('Authorization', `Bearer ${apiKey}`)
  if (init?.body) headers.set('Content-Type', 'application/json')
  const res = await fetch(path, { ...init, headers })
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

/** Bootstraps a development credential. Returns false in production mode. */
export async function bootstrapDevKey(): Promise<boolean> {
  try {
    const res = await fetch('/api/v1/auth/dev-session', { method: 'POST' })
    if (!res.ok) return false
    const body = (await res.json()) as { key: string }
    setKey(body.key)
    return true
  } catch {
    return false
  }
}

export const api = {
  listSims: () => req<{ simulations: Snapshot[] }>('/api/v1/simulations'),

  createSim: (body: {
    name?: string
    preset?: string
    seed?: number
    population?: number
    startHour?: number
    regions?: number
    workers?: number
  }) => req<Snapshot>('/api/v1/simulations', { method: 'POST', body: JSON.stringify(body) }),

  getSim: (id: string) => req<Snapshot>(`/api/v1/simulations/${id}`),

  deleteSim: (id: string) => req<void>(`/api/v1/simulations/${id}`, { method: 'DELETE' }),

  metrics: (id: string) => req<Metrics>(`/api/v1/simulations/${id}/metrics`),
  series: (id: string) => req<Series>(`/api/v1/simulations/${id}/series`),
  // The server always sends an array, but a client that trusts a remote to
  // never send null is a client that crashes the first time it does.
  incidents: async (id: string) => {
    const r = await req<{ incidents: Incident[] | null }>(`/api/v1/simulations/${id}/incidents`)
    return { incidents: r.incidents ?? [] }
  },

  events: async (id: string, from = 0, limit = 200) => {
    const r = await req<{ events: EventView[] | null; nextSeq: number; missed: number }>(
      `/api/v1/simulations/${id}/events?from=${from}&limit=${limit}`,
    )
    return { ...r, events: r.events ?? [] }
  },

  commands: (id: string) =>
    req<{ commands: { seq: number; tick: number; kind: string; text: string }[] }>(
      `/api/v1/simulations/${id}/commands`,
    ),

  checkpoints: (id: string) =>
    req<{ checkpoints: { tick: number; digest: string; uncompressedBytes: number; created: string }[] }>(
      `/api/v1/simulations/${id}/checkpoints`,
    ),

  entity: (id: string, kind: string, entityId: number) =>
    req<Record<string, unknown>>(`/api/v1/simulations/${id}/entity?kind=${kind}&id=${entityId}`),

  control: (id: string, body: { action: string; speed?: number; untilTick?: number }) =>
    req<{ status: string }>(`/api/v1/simulations/${id}/control`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  inject: (id: string, body: Record<string, unknown>) =>
    req<{ seq: number; tick: number; kind: string }>(`/api/v1/simulations/${id}/events`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  setPolicy: (id: string, body: Record<string, unknown>) =>
    req<{ applied: number }>(`/api/v1/simulations/${id}/policy`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  fork: (id: string, body: Record<string, unknown>) =>
    req<Snapshot>(`/api/v1/simulations/${id}/fork`, { method: 'POST', body: JSON.stringify(body) }),

  restore: (id: string, tick = 0) =>
    req<{ restoredFromTick: number; ticksReplayed: number; recoveryMillis: number }>(
      `/api/v1/simulations/${id}/restore`,
      { method: 'POST', body: JSON.stringify({ tick }) },
    ),

  compare: (ids: string[]) =>
    req<Comparison>('/api/v1/compare', { method: 'POST', body: JSON.stringify({ ids }) }),

  chaos: (body: { sim: string; experiment: string; param?: number }) =>
    req<ChaosResult>('/api/v1/chaos', { method: 'POST', body: JSON.stringify(body) }),

  agentTools: () =>
    req<{ tools: { name: string; description: string; tier: string }[] }>('/api/v1/agent/tools'),

  agentChat: (body: { sim: string; message: string; approveMutations: boolean }) =>
    req<AgentResponse>('/api/v1/agent/chat', { method: 'POST', body: JSON.stringify(body) }),

  audit: () =>
    req<{ entries: Record<string, unknown>[] }>('/api/v1/audit?limit=100'),

  /**
   * Fetches the static map and converts the columnar JSON into typed arrays.
   *
   * The server sends parallel arrays rather than an array of objects precisely
   * so this step is a copy rather than a parse of tens of thousands of small
   * objects. On the large preset that is the difference between a 200 ms and a
   * 2 s load.
   *
   * `version` is the map hash. Putting it in the URL makes the request
   * content-addressed, so an HTTP cache is safe to trust: a different city can
   * never be served from a cache entry belonging to an older one, which is
   * exactly the failure a plain per-simulation URL invites once ids are reused
   * across restarts.
   */
  async map(id: string, version?: string): Promise<CityMap> {
    const raw = await req<{
      name: string
      hash: string
      width: number
      height: number
      nodeX: number[]
      nodeY: number[]
      nodeSignal: number[]
      edgeFrom: number[]
      edgeTo: number[]
      edgeClass: number[]
      edgeDistrict: number[]
      districts: CityMap['districts']
      pois: CityMap['pois']
      signalNodes: number[]
      routes: CityMap['routes']
    }>(`/api/v1/simulations/${id}/map${version ? `?v=${encodeURIComponent(version)}` : ''}`)
    return {
      name: raw.name,
      hash: raw.hash,
      width: raw.width,
      height: raw.height,
      nodeX: Int32Array.from(raw.nodeX ?? []),
      nodeY: Int32Array.from(raw.nodeY ?? []),
      nodeSignal: Int32Array.from(raw.nodeSignal ?? []),
      edgeFrom: Int32Array.from(raw.edgeFrom ?? []),
      edgeTo: Int32Array.from(raw.edgeTo ?? []),
      edgeClass: Uint8Array.from(raw.edgeClass ?? []),
      edgeDistrict: Int32Array.from(raw.edgeDistrict ?? []),
      districts: raw.districts ?? [],
      pois: raw.pois ?? [],
      signalNodes: Int32Array.from(raw.signalNodes ?? []),
      routes: raw.routes ?? [],
    }
  },
}
