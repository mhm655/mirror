import { useCallback, useEffect, useRef, useState } from 'react'
import { api, bootstrapDevKey, getKey, setKey } from './lib/api'
import { SimStream } from './lib/stream'
import type {
  CityMap, DistrictStat, EventView, Incident, Metrics, Series, Snapshot,
} from './lib/types'
import { MapView, type MapHandle } from './components/MapView'
import {
  AgentPanel, ChaosPanel, CheckpointPanel, ComparePanel, EventsPanel,
  InjectPanel, InspectorPanel, MetricsPanel, OverviewPanel, PolicyPanel,
} from './components/panels'
import { fmtClock, fmtInt } from './components/common'
import type { HitResult } from './render/renderer'

const TABS = [
  'overview', 'metrics', 'events', 'inspect', 'inject',
  'policy', 'compare', 'assistant', 'chaos', 'recovery',
] as const
type Tab = (typeof TABS)[number]

const SPEEDS = [1, 4, 16, 64, 256, 0]

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [manualKey, setManualKey] = useState('')

  const [sims, setSims] = useState<Snapshot[]>([])
  const [simId, setSimId] = useState<string | null>(null)
  const [snap, setSnap] = useState<Snapshot | null>(null)
  const [metrics, setMetrics] = useState<Metrics | null>(null)
  const [series, setSeries] = useState<Series | null>(null)
  const [districts, setDistricts] = useState<DistrictStat[]>([])
  const [events, setEvents] = useState<EventView[]>([])
  const [incidents, setIncidents] = useState<Incident[]>([])
  const [map, setMap] = useState<CityMap | null>(null)
  const [connected, setConnected] = useState(false)
  const [tab, setTab] = useState<Tab>('overview')
  const [hit, setHit] = useState<HitResult | null>(null)
  const [banner, setBanner] = useState<string | null>(null)
  const [vehShown, setVehShown] = useState(0)
  const [vehTotal, setVehTotal] = useState(0)

  const streamRef = useRef<SimStream | null>(null)
  const mapHandle = useRef<MapHandle | null>(null)
  const mapForSim = useRef<string | null>(null)

  // -------------------------------------------------------------- auth ---

  useEffect(() => {
    ;(async () => {
      if (getKey()) {
        try {
          await api.listSims()
          setAuthed(true)
          return
        } catch {
          setKey(null)
        }
      }
      setAuthed(await bootstrapDevKey())
    })()
  }, [])

  // ------------------------------------------------------- sim listing ---

  const refreshSims = useCallback(async () => {
    try {
      const r = await api.listSims()
      setSims(r.simulations)
      setSimId((cur) => cur ?? r.simulations[0]?.id ?? null)
    } catch (e) {
      setBanner((e as Error).message)
    }
  }, [])

  useEffect(() => {
    if (!authed) return
    refreshSims()
    const t = window.setInterval(refreshSims, 3000)
    return () => window.clearInterval(t)
  }, [authed, refreshSims])

  // ------------------------------------------------------------ stream ---

  useEffect(() => {
    if (!authed || !simId) return
    setEvents([])
    setHit(null)
    const s = new SimStream(simId, {
      onStatus: (up) => setConnected(up),
      onSnapshot: (sn, m, se, ds) => {
        setSnap(sn)
        setMetrics(m)
        setSeries(se)
        setDistricts(ds)
      },
      onEvents: (evs) => setEvents((prev) => [...prev, ...evs].slice(-600)),
      onVehicles: (f) => {
        mapHandle.current?.pushVehicles(f)
        setVehShown(f.sent)
        setVehTotal(f.total)
      },
      onNetwork: (f) => mapHandle.current?.setNetwork(f),
    })
    streamRef.current = s
    return () => {
      s.close()
      streamRef.current = null
    }
  }, [authed, simId])

  // The static map is fetched once per simulation. Forks share their parent's
  // map, so switching between scenarios of the same city costs nothing.
  useEffect(() => {
    if (!authed || !simId) return
    const parent = sims.find((s) => s.id === simId)
    const key = parent?.mapHash ?? simId
    if (mapForSim.current === key) return
    let cancelled = false
    api
      .map(simId, parent?.mapHash)
      .then((m) => {
        if (cancelled) return
        mapForSim.current = key
        setMap(m)
      })
      .catch((e) => setBanner((e as Error).message))
    return () => {
      cancelled = true
    }
  }, [authed, simId, sims])

  useEffect(() => {
    if (!simId) return
    const load = () =>
      api
        .incidents(simId)
        .then((r) => {
          setIncidents(r.incidents)
          mapHandle.current?.setIncidents(r.incidents)
        })
        .catch(() => undefined)
    load()
    const t = window.setInterval(load, 2000)
    return () => window.clearInterval(t)
  }, [simId])

  // ---------------------------------------------------------- controls ---

  const control = async (action: string, extra: Record<string, unknown> = {}) => {
    if (!simId) return
    try {
      await api.control(simId, { action, ...extra } as never)
      refreshSims()
    } catch (e) {
      setBanner((e as Error).message)
    }
  }

  const fork = async () => {
    if (!simId || !snap) return
    const name = window.prompt('Name for the new scenario', `Fork of ${snap.name}`)
    if (name === null) return
    try {
      const child = await api.fork(simId, { name, play: true, speed: snap.speed })
      await refreshSims()
      setSimId(child.id)
      setTab('policy')
    } catch (e) {
      setBanner((e as Error).message)
    }
  }

  const newSim = async () => {
    const preset = window.prompt('City size: small, medium, large or huge', 'medium')
    if (!preset) return
    const popRaw = window.prompt('Population', '60000')
    if (popRaw === null) return
    try {
      const s = await api.createSim({
        name: `${preset} city`,
        preset,
        population: Number(popRaw) || 40000,
        startHour: 7,
      })
      await api.control(s.id, { action: 'play' })
      await refreshSims()
      setSimId(s.id)
    } catch (e) {
      setBanner((e as Error).message)
    }
  }

  const removeSim = async (id: string) => {
    if (!window.confirm('Delete this scenario? Its state and checkpoints are discarded.')) return
    try {
      await api.deleteSim(id)
      if (simId === id) setSimId(null)
      await refreshSims()
    } catch (e) {
      setBanner((e as Error).message)
    }
  }

  // Keyboard: space toggles the transport, which is what anyone who has used a
  // timeline expects it to do.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT')) return
      if (e.code === 'Space') {
        e.preventDefault()
        control(snap?.state === 'running' ? 'pause' : 'play')
      }
      if (e.key === 'f') mapHandle.current?.fit()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [snap?.state, simId])

  const onViewport = useCallback(
    (x0: number, y0: number, x1: number, y1: number, maxVehicles: number) => {
      streamRef.current?.setViewport(x0, y0, x1, y1, maxVehicles)
    },
    [],
  )

  const onReady = useCallback((h: MapHandle) => {
    mapHandle.current = h
  }, [])

  // ---------------------------------------------------------- rendering ---

  if (authed === null) {
    return (
      <div className="gate">
        <div className="card">
          <h1>MIRROR</h1>
          <p>Connecting…</p>
        </div>
      </div>
    )
  }

  if (!authed) {
    return (
      <div className="gate">
        <div className="card">
          <h1>MIRROR</h1>
          <p style={{ marginTop: 14 }}>
            This server is running in production mode, so it will not hand out a credential. Paste
            an API key configured through <code>MIRROR_API_KEYS</code>.
          </p>
          <div className="field" style={{ marginTop: 14 }}>
            <label>api key</label>
            <input type="text" value={manualKey} onChange={(e) => setManualKey(e.target.value)} placeholder="mrr_…" />
          </div>
          <button
            className="btn primary"
            style={{ width: '100%' }}
            onClick={async () => {
              setKey(manualKey.trim())
              try {
                await api.listSims()
                setAuthed(true)
              } catch (e) {
                setBanner((e as Error).message)
                setKey(null)
              }
            }}
          >
            Connect
          </button>
          {banner && <div className="err" style={{ marginTop: 12 }}>{banner}</div>}
        </div>
      </div>
    )
  }

  const roots = sims.filter((s) => !s.parentId)
  const childrenOf = (id: string) => sims.filter((s) => s.parentId === id)

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="dot" />
          MIRROR <small>city digital twin</small>
        </div>

        <div className="clock">{snap ? fmtClock(snap.clockHour, snap.clockMin) : '--:--'}</div>

        <div className="transport">
          <button
            className={`btn${snap?.state === 'running' ? ' active' : ''}`}
            onClick={() => control(snap?.state === 'running' ? 'pause' : 'play')}
            title="Space"
          >
            {snap?.state === 'running' ? '❚❚ pause' : '▶ play'}
          </button>
          <div className="speeds">
            {SPEEDS.map((sp) => (
              <button
                key={sp}
                className={`btn${snap?.speed === sp ? ' active' : ''}`}
                onClick={() => control('speed', { speed: sp })}
                title={sp === 0 ? 'as fast as the CPU allows' : `${sp}x simulated time`}
              >
                {sp === 0 ? 'max' : `${sp}×`}
              </button>
            ))}
          </div>
        </div>

        <div className="spacer" />

        <div className="stat">
          <span className="k">tick rate</span>
          <span className="v">{fmtInt(snap?.ticksPerSecond)}/s</span>
        </div>
        <div className="stat">
          <span className="k">tick</span>
          <span className="v">{snap ? `${snap.perf.tickMillis.toFixed(2)} ms` : '—'}</span>
        </div>
        <div className="stat">
          <span className="k">workers</span>
          <span className="v">{fmtInt(snap?.regions)}</span>
        </div>
        <div className="stat">
          <span className="k">vehicles</span>
          <span className="v">{fmtInt(snap?.live.activeVehicles)}</span>
        </div>
        <div className="stat" style={{ minWidth: 138 }}>
          <span className="k">state digest</span>
          <span className="v" title="FNV-1a over the canonical state encoding. Identical inputs give an identical digest.">
            {snap?.digest ?? '—'}
          </span>
        </div>
      </header>

      <aside className="rail">
        <div className="rail-head">
          <span>scenarios</span>
          <span>{sims.length}</span>
        </div>
        {roots.map((s) => (
          <div key={s.id}>
            <ScenarioRow s={s} sel={s.id === simId} onSelect={() => setSimId(s.id)} onDelete={() => removeSim(s.id)} />
            {childrenOf(s.id).map((c) => (
              <ScenarioRow key={c.id} s={c} child sel={c.id === simId} onSelect={() => setSimId(c.id)} onDelete={() => removeSim(c.id)} />
            ))}
          </div>
        ))}
        <div className="rail-actions">
          <button className="btn primary" onClick={fork} disabled={!simId}>
            Fork this scenario
          </button>
          <button className="btn" onClick={newSim}>
            New city
          </button>
        </div>
        <div className="spacer" />
        {snap && (
          <div className="rail-actions" style={{ borderTop: '1px solid var(--line)' }}>
            <div className="kv" style={{ fontSize: 11 }}>
              <div className="k">preset</div>
              <div className="v">{snap.mapPreset}</div>
              <div className="k">seed</div>
              <div className="v">{snap.seed}</div>
              <div className="k">map</div>
              <div className="v" style={{ fontSize: 10 }}>{snap.mapHash.slice(0, 10)}</div>
              <div className="k">residents</div>
              <div className="v">{fmtInt(snap.population)}</div>
            </div>
          </div>
        )}
      </aside>

      <MapView
        map={map}
        connected={connected}
        onViewport={onViewport}
        onPick={(h) => {
          setHit(h)
          if (h) setTab('inspect')
        }}
        onReady={onReady}
        hudActive={vehShown}
        hudTotal={vehTotal}
        hudSpeed={snap?.live.avgSpeedKph ?? 0}
        hudCongested={snap?.live.congestedPct ?? 0}
      />

      <section className="panel">
        <nav className="tabs">
          {TABS.map((t) => (
            <button key={t} className={`tab${tab === t ? ' sel' : ''}`} onClick={() => setTab(t)}>
              {t}
            </button>
          ))}
        </nav>
        <div className="panel-body">
          {banner && (
            <div className="err" onClick={() => setBanner(null)}>
              {banner}
            </div>
          )}
          {snap?.lastError && <div className="warn">{snap.lastError}</div>}
          {!snap && <div className="empty">Waiting for the first frame…</div>}

          {snap && metrics && series && tab === 'overview' && (
            <OverviewPanel snap={snap} metrics={metrics} series={series} districts={districts} />
          )}
          {snap && metrics && tab === 'metrics' && <MetricsPanel snap={snap} metrics={metrics} />}
          {snap && tab === 'events' && <EventsPanel events={events} startHour={snap.clockHour} />}
          {simId && tab === 'inspect' && <InspectorPanel simId={simId} hit={hit} incidents={incidents} />}
          {simId && tab === 'inject' && (
            <InjectPanel
              simId={simId}
              selectedEdge={hit?.kind === 'edge' ? hit.id : null}
              onDone={refreshSims}
            />
          )}
          {snap && tab === 'policy' && <PolicyPanel snap={snap} onChanged={refreshSims} />}
          {simId && tab === 'compare' && <ComparePanel sims={sims} current={simId} />}
          {simId && tab === 'assistant' && <AgentPanel simId={simId} />}
          {simId && tab === 'chaos' && <ChaosPanel simId={simId} />}
          {simId && tab === 'recovery' && <CheckpointPanel simId={simId} />}
        </div>
      </section>
    </div>
  )
}

function ScenarioRow(props: {
  s: Snapshot
  sel: boolean
  child?: boolean
  onSelect(): void
  onDelete(): void
}) {
  const { s } = props
  return (
    <div
      className={`scn${props.sel ? ' sel' : ''}${props.child ? ' child' : ''}`}
      onClick={props.onSelect}
    >
      <div className="row1">
        <span className="nm grow">{s.name}</span>
        {props.child && <span className="pill fork">fork</span>}
        <span className={`pill ${s.state === 'running' ? 'run' : 'pause'}`}>{s.state}</span>
      </div>
      <div className="row2">
        <span>{fmtClock(s.clockHour, s.clockMin)}</span>
        <span>{fmtInt(s.live.activeVehicles)} veh</span>
        <span>{s.live.avgSpeedKph} km/h</span>
        <span className="grow" />
        {props.child && (
          <button
            className="btn small"
            title="delete scenario"
            onClick={(e) => {
              e.stopPropagation()
              props.onDelete()
            }}
          >
            ×
          </button>
        )}
      </div>
    </div>
  )
}
