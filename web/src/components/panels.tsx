import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import type {
  AgentResponse, ChaosResult, Comparison, DistrictStat, EventView,
  Incident, Metrics, Series, Snapshot,
} from '../lib/types'
import { Bar, ChartBlock, KV, Tile, fmtDur, fmtInt, fmtNum } from './common'
import type { HitResult } from '../render/renderer'

/* ------------------------------------------------------------- overview --- */

export function OverviewPanel(props: {
  snap: Snapshot
  metrics: Metrics
  series: Series
  districts: DistrictStat[]
}) {
  const { snap, metrics, series } = props
  const live = snap.live
  const powered =
    live.substationsTotal > 0
      ? Math.round((live.substationsOnline / live.substationsTotal) * 100)
      : 100

  return (
    <>
      <div className="section">
        <h3>Network</h3>
        <div className="tiles">
          <Tile k="vehicles" v={fmtInt(live.activeVehicles)} s={`${fmtInt(live.cars)} car · ${fmtInt(live.buses + live.metros)} transit`} />
          <Tile
            k="mean speed"
            v={`${live.avgSpeedKph}`}
            s="km/h"
            tone={live.avgSpeedKph < 18 ? 'bad' : live.avgSpeedKph < 28 ? 'warn' : undefined}
          />
          <Tile
            k="congested links"
            v={`${live.congestedPct}%`}
            tone={live.congestedPct > 30 ? 'bad' : live.congestedPct > 15 ? 'warn' : undefined}
          />
          <Tile
            k="open incidents"
            v={fmtInt(live.openIncidents)}
            tone={live.openIncidents > 4 ? 'bad' : live.openIncidents > 0 ? 'warn' : undefined}
          />
        </div>
      </div>

      <div className="section">
        <h3>Population</h3>
        <div className="kv">
          <KV k="Residents" v={fmtInt(snap.population)} />
          <KV k="Travelling" v={fmtInt(live.agentsTravelling)} />
          <KV k="At home" v={fmtInt(live.agentsAtHome)} />
          <KV k="At work" v={fmtInt(live.agentsAtWork)} />
          <KV
            k="Stranded"
            v={<span style={{ color: live.agentsStranded > 0 ? 'var(--amber)' : undefined }}>{fmtInt(live.agentsStranded)}</span>}
          />
          <KV k="Trips completed" v={fmtInt(metrics.tripsCompleted)} />
        </div>
      </div>

      <div className="section">
        <h3>Infrastructure</h3>
        <div className="kv">
          <KV k="Substations online" v={`${live.substationsOnline} / ${live.substationsTotal}`} />
        </div>
        <div style={{ margin: '4px 0 9px' }}>
          <Bar pct={powered} warnAt={101} badAt={101} />
        </div>
        <div className="kv">
          <KV k="Signals dark" v={<span style={{ color: live.signalsDark > 0 ? 'var(--amber)' : undefined }}>{fmtInt(live.signalsDark)}</span>} />
          <KV k="Hospital occupancy" v={`${live.hospitalUtilPct}%`} />
        </div>
        <div style={{ marginTop: 4 }}>
          <Bar pct={live.hospitalUtilPct} warnAt={85} badAt={95} />
        </div>
        <div className="kv" style={{ marginTop: 9 }}>
          <KV k="Weather" v={`${live.weather}, ${live.tempC}°C`} />
        </div>
      </div>

      <div className="section">
        <h3>Last 24 simulated hours</h3>
        <ChartBlock label="active vehicles" value={fmtInt(live.activeVehicles)} data={series.activeVehicles} />
        <ChartBlock label="mean speed km/h" value={`${live.avgSpeedKph}`} data={series.avgSpeedKph} color="var(--accent-2)" max={90} />
        <ChartBlock label="congested links %" value={`${live.congestedPct}%`} data={series.congestionPct.map((v) => v / 10)} color="var(--amber)" max={100} />
        <ChartBlock label="hospital occupancy %" value={`${live.hospitalUtilPct}%`} data={series.hospitalPct.map((v) => v / 10)} color="var(--red)" max={100} />
      </div>

      <div className="section">
        <h3>Districts</h3>
        <table className="cmp">
          <thead>
            <tr>
              <th>district</th>
              <th>veh</th>
              <th>km/h</th>
              <th>cong</th>
              <th>pwr</th>
            </tr>
          </thead>
          <tbody>
            {[...props.districts]
              .sort((a, b) => b.congestedPct - a.congestedPct)
              .map((d) => (
                <tr key={d.id}>
                  <td>{d.name}</td>
                  <td>{fmtInt(d.vehicles)}</td>
                  <td>{d.avgSpeedKph}</td>
                  <td style={{ color: d.congestedPct > 30 ? 'var(--red)' : d.congestedPct > 15 ? 'var(--amber)' : undefined }}>
                    {d.congestedPct}%
                  </td>
                  <td style={{ color: d.poweredPct < 100 ? 'var(--red)' : undefined }}>{d.poweredPct}%</td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
    </>
  )
}

/* ------------------------------------------------------------- metrics --- */

export function MetricsPanel(props: { snap: Snapshot; metrics: Metrics }) {
  const m = props.metrics
  const perf = props.snap.perf
  return (
    <>
      <div className="section">
        <h3>Travel time</h3>
        <div className="tiles">
          <Tile k="mean" v={fmtDur(m.travelMeanSec)} />
          <Tile k="p50" v={fmtDur(m.travelP50Sec)} />
          <Tile k="p95" v={fmtDur(m.travelP95Sec)} />
          <Tile k="p99" v={fmtDur(m.travelP99Sec)} />
        </div>
        <div className="kv" style={{ marginTop: 9 }}>
          <KV k="Mean delay vs free flow" v={fmtDur(m.delayMeanSec)} />
          <KV k="P95 delay" v={fmtDur(m.delayP95Sec)} />
          <KV k="Trips started" v={fmtInt(m.tripsStarted)} />
          <KV k="Trips completed" v={fmtInt(m.tripsCompleted)} />
          <KV k="Trips abandoned" v={fmtInt(m.tripsAbandoned)} />
        </div>
        {m.tripsCompleted < 200 && (
          <div className="warn" style={{ marginTop: 9 }}>
            Only {fmtInt(m.tripsCompleted)} trips have completed. Percentiles over a sample this
            small move around a lot; let the run go further before drawing conclusions.
          </div>
        )}
      </div>

      <div className="section">
        <h3>Emergency response</h3>
        <div className="kv">
          <KV k="Dispatched" v={fmtInt(m.emergencyDispatched)} />
          <KV k="On scene" v={fmtInt(m.emergencyArrived)} />
          <KV k="Mean response" v={fmtDur(m.responseMeanSec)} />
          <KV k="P50 response" v={fmtDur(m.responseP50Sec)} />
          <KV k="P95 response" v={fmtDur(m.responseP95Sec)} />
          <KV k="Incidents opened" v={fmtInt(m.incidentsOpened)} />
          <KV k="Incidents resolved" v={fmtInt(m.incidentsResolved)} />
          <KV k="Casualties" v={fmtInt(m.casualties)} />
        </div>
      </div>

      <div className="section">
        <h3>Health system</h3>
        <div className="kv">
          <KV k="Admissions" v={fmtInt(m.hospitalAdmissions)} />
          <KV k="Diversions" v={fmtInt(m.hospitalDiversions)} />
          <KV k="Rejections" v={<span style={{ color: m.hospitalRejections > 0 ? 'var(--red)' : undefined }}>{fmtInt(m.hospitalRejections)}</span>} />
          <KV k="Peak occupancy" v={`${m.peakHospitalUtilPct}%`} />
        </div>
      </div>

      <div className="section">
        <h3>Transit</h3>
        <div className="kv">
          <KV k="Boardings" v={fmtInt(m.transitBoardings)} />
          <KV k="Left behind" v={<span style={{ color: m.transitDenied > 0 ? 'var(--amber)' : undefined }}>{fmtInt(m.transitDenied)}</span>} />
        </div>
      </div>

      <div className="section">
        <h3>Energy and environment</h3>
        <div className="kv">
          <KV k="Vehicle distance" v={`${fmtNum(m.vehicleKm, 0)} km`} />
          <KV k="Time stopped" v={`${fmtNum(m.stoppedVehicleHours, 1)} veh-h`} />
          <KV k="Fuel" v={`${fmtNum(m.fuelLitres, 0)} L`} />
          <KV k="CO₂" v={`${fmtNum(m.co2Kg, 0)} kg`} />
          <KV k="Substation trips" v={fmtInt(m.substationTrips)} />
          <KV k="Dropped calls" v={fmtInt(m.commsDropped)} />
        </div>
      </div>

      <div className="section">
        <h3>Engine</h3>
        <div className="kv">
          <KV k="Tick" v={fmtInt(props.snap.tick)} />
          <KV k="Ticks per second" v={fmtInt(props.snap.ticksPerSecond)} />
          <KV k="Tick duration" v={`${fmtNum(perf.tickMillis, 2)} ms`} />
          <KV k="Parallel phase" v={`${fmtNum(perf.phaseAMillis, 2)} ms`} />
          <KV k="Serial commit" v={`${fmtNum(perf.phaseBMillis, 2)} ms`} />
          <KV k="Serial fraction" v={`${perf.serialPercent}%`} />
          <KV k="Region workers" v={fmtInt(props.snap.regions)} />
          <KV k="Intents last tick" v={fmtInt(perf.intents)} />
          <KV k="Edge crossings" v={fmtInt(perf.crossings)} />
          <KV k="Route queries" v={fmtInt(perf.routeQueries)} />
          <KV k="Reroutes" v={fmtInt(m.reroutes)} />
          <KV k="Routing failures" v={fmtInt(m.routeFailures)} />
          <KV k="Events dropped" v={fmtInt(perf.eventsDropped)} />
          <KV k="Checkpoints" v={fmtInt(props.snap.checkpoints)} />
          <KV k="State digest" v={<span title="FNV-1a over the canonical state encoding">{props.snap.digest}</span>} />
        </div>
      </div>
    </>
  )
}

/* -------------------------------------------------------------- events --- */

export function EventsPanel(props: { events: EventView[]; startHour: number }) {
  const [filter, setFilter] = useState<'all' | 'notice' | 'warning' | 'critical'>('notice')
  const rank = { info: 0, notice: 1, warning: 2, critical: 3 }
  const min = filter === 'all' ? 0 : rank[filter]
  const shown = props.events.filter((e) => rank[e.severity] >= min).slice(-260).reverse()

  const clock = (tick: number) => {
    const tod = (Math.floor(tick / 10) + props.startHour * 3600) % 86400
    const h = Math.floor(tod / 3600)
    const m = Math.floor((tod % 3600) / 60)
    const s = tod % 60
    return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  }

  return (
    <>
      <div className="row wrap" style={{ marginBottom: 10 }}>
        {(['all', 'notice', 'warning', 'critical'] as const).map((f) => (
          <button key={f} className={`btn small${filter === f ? ' active' : ''}`} onClick={() => setFilter(f)}>
            {f}
          </button>
        ))}
      </div>
      {shown.length === 0 ? (
        <div className="empty">
          Nothing at this severity yet.
          <br />
          Inject an event to see the city react.
        </div>
      ) : (
        <div className="feed">
          {shown.map((e) => (
            <div key={e.seq} className={`ev ${e.severity}`}>
              <div className="t">{clock(e.tick)}</div>
              <div className="m">{e.text}</div>
            </div>
          ))}
        </div>
      )}
    </>
  )
}

/* ------------------------------------------------------------ inspector --- */

export function InspectorPanel(props: { simId: string; hit: HitResult | null; incidents: Incident[] }) {
  const [data, setData] = useState<Record<string, unknown> | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [live, setLive] = useState(true)

  useEffect(() => {
    if (!props.hit) {
      setData(null)
      return
    }
    let cancelled = false
    const kindMap: Record<string, string> = { vehicle: 'vehicle', edge: 'edge', poi: 'poi' }
    const load = async () => {
      try {
        // A POI hit carries the index into the map's landmark list, which is not
        // an entity id; the click handler resolves those to a typed lookup
        // before we get here for hospitals and substations, and anything else
        // falls back to showing the road beneath.
        const kind = kindMap[props.hit!.kind] ?? 'edge'
        if (kind === 'poi') {
          setData(null)
          return
        }
        const d = await api.entity(props.simId, kind, props.hit!.id)
        if (!cancelled) {
          setData(d)
          setErr(null)
        }
      } catch (e) {
        if (!cancelled) setErr((e as Error).message)
      }
    }
    load()
    const t = live ? window.setInterval(load, 700) : undefined
    return () => {
      cancelled = true
      if (t) window.clearInterval(t)
    }
  }, [props.hit, props.simId, live])

  const open = props.incidents.filter((i) => !i.resolved)

  return (
    <>
      <div className="section">
        <h3>
          Selection
          <button
            className={`btn small${live ? ' active' : ''}`}
            style={{ float: 'right', marginTop: -3 }}
            onClick={() => setLive((v) => !v)}
          >
            {live ? 'live' : 'frozen'}
          </button>
        </h3>
        {!props.hit && (
          <div className="empty">
            Click a vehicle, a road or a piece of infrastructure on the map.
            <br />
            Scroll to zoom, drag to pan.
          </div>
        )}
        {err && <div className="err">{err}</div>}
        {data && (
          <div className="kv">
            {Object.entries(data)
              .filter(([k]) => !['x', 'y', 'x1', 'y1', 'x2', 'y2', 'homeX', 'homeY', 'workX', 'workY'].includes(k))
              .map(([k, v]) => (
                <KV key={k} k={humanise(k)} v={renderVal(v)} />
              ))}
          </div>
        )}
      </div>

      <div className="section">
        <h3>Open incidents ({open.length})</h3>
        {open.length === 0 ? (
          <div className="empty">No open incidents.</div>
        ) : (
          <div className="feed">
            {open.map((i) => (
              <div key={i.id} className={`ev ${i.casualties > 0 ? 'critical' : 'warning'}`}>
                <div className="t">#{i.id}</div>
                <div className="m">
                  <b>{i.kind}</b> in {i.district}
                  <br />
                  severity {i.severity}/1000
                  {i.casualties > 0 && ` · ${i.casualties} casualties`}
                  <br />
                  {i.responseSec > 0
                    ? `first unit on scene after ${fmtDur(i.responseSec)}`
                    : i.awaitingUnits > 0
                      ? `awaiting ${i.awaitingUnits} units`
                      : 'units en route'}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  )
}

function humanise(k: string): string {
  return k
    .replace(/([A-Z])/g, ' $1')
    .replace(/^./, (c) => c.toUpperCase())
    .replace(/ Pct$/, ' %')
    .replace(/ Kph$/, ' km/h')
    .replace(/ Sec$/, ' (s)')
    .replace(/ K W$/, ' kW')
}

function renderVal(v: unknown): React.ReactNode {
  if (typeof v === 'boolean') return v ? 'yes' : 'no'
  if (typeof v === 'number') return Number.isInteger(v) ? fmtInt(v) : fmtNum(v, 2)
  if (Array.isArray(v)) return v.length ? v.join(', ') : '—'
  return String(v)
}

/* -------------------------------------------------------------- inject --- */

const EVENT_FORMS: {
  kind: string
  label: string
  hint: string
  fields: { key: 'a' | 'b' | 'c' | 'd'; label: string; def: number }[]
}[] = [
  {
    kind: 'accident',
    label: 'Traffic collision',
    hint: 'Blocks lanes in proportion to severity, calls out emergency units, and sends casualties to hospital.',
    fields: [
      { key: 'a', label: 'road id', def: 0 },
      { key: 'b', label: 'severity 0-1000', def: 850 },
      { key: 'c', label: 'casualties', def: 2 },
    ],
  },
  {
    kind: 'close_road',
    label: 'Close a road',
    hint: 'Makes the link impassable; informed drivers reroute around it as they discover it.',
    fields: [
      { key: 'a', label: 'road id', def: 0 },
      { key: 'b', label: 'duration (ticks)', def: 12000 },
    ],
  },
  {
    kind: 'power_failure',
    label: 'Substation failure',
    hint: 'Signals go dark, streets unlit, hospitals to generators, cell sites to battery, load sheds onto neighbours.',
    fields: [
      { key: 'a', label: 'substation id', def: 0 },
      { key: 'b', label: 'duration (ticks)', def: 12000 },
    ],
  },
  {
    kind: 'weather',
    label: 'Weather',
    hint: '0 clear · 1 rain · 2 storm · 3 snow · 4 heatwave · 5 fog. Affects speed, crash rate and electrical demand.',
    fields: [
      { key: 'a', label: 'condition 0-5', def: 2 },
      { key: 'b', label: 'temperature °C', def: 9 },
      { key: 'c', label: 'wind km/h', def: 55 },
      { key: 'd', label: 'duration (ticks)', def: 18000 },
    ],
  },
  {
    kind: 'hospital_surge',
    label: 'Mass casualty',
    hint: 'Presents patients directly at a hospital; watch diversions and rejections.',
    fields: [
      { key: 'a', label: 'hospital id', def: 0 },
      { key: 'b', label: 'patients', def: 40 },
    ],
  },
  {
    kind: 'flood',
    label: 'Flood a district',
    hint: 'Closes low-lying streets first; motorways are engineered with drainage and survive longer.',
    fields: [
      { key: 'a', label: 'district id', def: 0 },
      { key: 'b', label: 'severity 0-1000', def: 500 },
      { key: 'c', label: 'duration (ticks)', def: 30000 },
    ],
  },
  {
    kind: 'transit_failure',
    label: 'Suspend a transit route',
    hint: 'No further departures until the outage ends; passengers accumulate at stops.',
    fields: [
      { key: 'a', label: 'route id', def: 0 },
      { key: 'b', label: 'duration (ticks)', def: 18000 },
    ],
  },
  {
    kind: 'spawn_traffic',
    label: 'Extra demand',
    hint: 'Departs residents who are still at home. Never fabricates driverless vehicles.',
    fields: [
      { key: 'a', label: 'trips', def: 1500 },
      { key: 'b', label: 'district id (-1 = any)', def: -1 },
    ],
  },
]

export function InjectPanel(props: { simId: string; selectedEdge: number | null; onDone(): void }) {
  const [kind, setKind] = useState(EVENT_FORMS[0].kind)
  const form = EVENT_FORMS.find((f) => f.kind === kind)!
  const [vals, setVals] = useState<Record<string, number>>({})
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  useEffect(() => {
    const next: Record<string, number> = {}
    for (const f of form.fields) next[f.key] = f.def
    // Clicking a road then opening this panel pre-fills the target, which is
    // the difference between "inject an accident" being a two-click operation
    // and a copy-the-id-out-of-the-inspector operation.
    if (props.selectedEdge != null && (kind === 'accident' || kind === 'close_road')) {
      next.a = props.selectedEdge
    }
    setVals(next)
  }, [kind, props.selectedEdge, form])

  const submit = async () => {
    try {
      const body: Record<string, unknown> = { kind }
      for (const f of form.fields) body[f.key] = vals[f.key] ?? f.def
      const r = await api.inject(props.simId, body)
      setMsg({ ok: true, text: `Accepted as ${r.kind} at tick ${r.tick}. It is in the command log and will replay.` })
      props.onDone()
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    }
  }

  return (
    <>
      <div className="section">
        <h3>Inject an event</h3>
        <div className="field">
          <label>event</label>
          <select value={kind} onChange={(e) => setKind(e.target.value)}>
            {EVENT_FORMS.map((f) => (
              <option key={f.kind} value={f.kind}>
                {f.label}
              </option>
            ))}
          </select>
        </div>
        <p className="dim" style={{ fontSize: 11.5, lineHeight: 1.55, marginTop: -2 }}>
          {form.hint}
        </p>
        {form.fields.map((f) => (
          <div className="field" key={f.key}>
            <label>{f.label}</label>
            <input
              type="number"
              value={vals[f.key] ?? f.def}
              onChange={(e) => setVals((v) => ({ ...v, [f.key]: Number(e.target.value) }))}
            />
          </div>
        ))}
        <button className="btn primary" onClick={submit} style={{ width: '100%' }}>
          Inject into the live simulation
        </button>
        {msg && <div className={msg.ok ? 'ok' : 'err'} style={{ marginTop: 9 }}>{msg.text}</div>}
        <div className="warn" style={{ marginTop: 9 }}>
          This changes the simulation everyone is watching. To explore a change without
          consequences, fork a scenario instead.
        </div>
      </div>
    </>
  )
}

/* ------------------------------------------------------------- compare --- */

export function ComparePanel(props: { sims: Snapshot[]; current: string }) {
  const [selected, setSelected] = useState<string[]>([props.current])
  const [cmp, setCmp] = useState<Comparison | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const toggle = (id: string) =>
    setSelected((s) => (s.includes(id) ? s.filter((x) => x !== id) : [...s, id]))

  const run = async () => {
    setBusy(true)
    setErr(null)
    try {
      setCmp(await api.compare(selected))
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div className="section">
        <h3>Scenarios to compare</h3>
        <p className="dim" style={{ fontSize: 11.5, lineHeight: 1.55 }}>
          The first one selected is the baseline; the rest are reported as changes against it.
        </p>
        {props.sims.map((s) => (
          <label className="check" key={s.id}>
            <input type="checkbox" checked={selected.includes(s.id)} onChange={() => toggle(s.id)} />
            <span className="grow">{s.name}</span>
            <span className="mono faint">{Math.round(s.tick / 600)}m</span>
          </label>
        ))}
        <button className="btn primary" onClick={run} disabled={busy || selected.length < 2} style={{ width: '100%', marginTop: 6 }}>
          {busy ? 'comparing…' : `Compare ${selected.length} scenarios`}
        </button>
        {err && <div className="err" style={{ marginTop: 9 }}>{err}</div>}
      </div>

      {cmp && (
        <div className="section">
          <h3>Result</h3>
          {cmp.warnings.map((w, i) => (
            <div className="warn" key={i}>
              {w}
            </div>
          ))}
          <table className="cmp">
            <thead>
              <tr>
                <th>metric</th>
                {cmp.scenarios.map((s) => (
                  <th key={s.id} title={s.name}>
                    {shortLabel(s.name)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {cmp.rows.map((r) => (
                <tr key={r.key}>
                  <td title={r.unit ? `in ${r.unit}` : undefined}>{r.label}</td>
                  {cmp.scenarios.map((s, i) => {
                    const v = r.values[s.id]
                    const d = r.deltaPct[s.id] ?? 0
                    const flat = Math.abs(d) < 0.5
                    const good = d < 0 === r.lowerIsBetter
                    return (
                      <td key={s.id}>
                        {fmtNum(v, v >= 100 ? 0 : 1)}
                        {i > 0 && (
                          <div className={`delta ${flat ? 'flat' : good ? 'good' : 'bad'}`} style={{ fontSize: 10 }}>
                            {flat ? '—' : `${d > 0 ? '+' : ''}${d.toFixed(1)}%`}
                          </div>
                        )}
                      </td>
                    )
                  })}
                </tr>
              ))}
            </tbody>
          </table>
          <p className="faint" style={{ fontSize: 11, lineHeight: 1.55, marginTop: 10 }}>
            Single runs on one seed. A difference of a few percent is inside the noise of the
            underlying traffic model; re-run the same comparison on several seeds before treating
            one as real.
          </p>
        </div>
      )}
    </>
  )
}

function shortLabel(n: string): string {
  const i = n.indexOf(' - ')
  const s = i > 0 ? n.slice(0, i) : n
  return s.length > 12 ? s.slice(0, 12) + '…' : s
}

/* --------------------------------------------------------------- agent --- */

export function AgentPanel(props: { simId: string }) {
  const [input, setInput] = useState('')
  const [approve, setApprove] = useState(false)
  const [busy, setBusy] = useState(false)
  const [log, setLog] = useState<{ role: 'user' | 'bot'; text: string; resp?: AgentResponse }[]>([])
  const [tools, setTools] = useState<{ name: string; tier: string; description: string }[]>([])
  const [showTools, setShowTools] = useState(false)
  const endRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    api.agentTools().then((t) => setTools(t.tools)).catch(() => setTools([]))
  }, [])

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [log, busy])

  const ask = async (text: string) => {
    if (!text.trim() || busy) return
    setLog((l) => [...l, { role: 'user', text }])
    setInput('')
    setBusy(true)
    try {
      const resp = await api.agentChat({ sim: props.simId, message: text, approveMutations: approve })
      setLog((l) => [...l, { role: 'bot', text: resp.reply, resp }])
    } catch (e) {
      setLog((l) => [...l, { role: 'bot', text: `Request failed: ${(e as Error).message}` }])
    } finally {
      setBusy(false)
    }
  }

  const suggestions = [
    'What is the state of the city right now?',
    'Would adaptive traffic signals help? Run it and show me.',
    'What if we added 60% more buses instead?',
    'Which hospitals are under pressure?',
    'Show me the power network.',
  ]

  return (
    <>
      <div className="section">
        <h3>
          Operations assistant
          <button className="btn small" style={{ float: 'right', marginTop: -3 }} onClick={() => setShowTools((v) => !v)}>
            {tools.length} tools
          </button>
        </h3>
        {showTools && (
          <div style={{ marginBottom: 10 }}>
            {tools.map((t) => (
              <div className="step" key={t.name} title={t.description}>
                <span className={`tier ${t.tier}`}>{t.tier}</span>
                <span className="name">{t.name}</span>
              </div>
            ))}
            <p className="faint" style={{ fontSize: 11, lineHeight: 1.5, marginTop: 6 }}>
              Sandboxed tools fork the simulation and cannot affect what you are watching.
              Mutating tools are only offered when you grant authority below, and only if your
              own role permits them.
            </p>
          </div>
        )}

        <div className="chat">
          {log.length === 0 && (
            <div className="empty">
              Ask about the city, or ask a "what if" question — the assistant will fork
              scenarios, run them and compare the results.
            </div>
          )}
          {log.map((m, i) => (
            <div className={`msg ${m.role}`} key={i}>
              {m.role === 'bot' ? <pre>{m.text}</pre> : m.text}
              {m.resp && m.resp.steps.length > 0 && (
                <div className="steps">
                  {m.resp.steps.map((s, j) => (
                    <div className="step" key={j}>
                      <span className={`tier ${s.tier}`}>{s.tier}</span>
                      <span className="name">{s.tool}</span>
                      <span>{s.millis} ms</span>
                      {s.error && <span className="err">{s.error}</span>}
                    </div>
                  ))}
                </div>
              )}
              {m.resp?.notice && (
                <div className="faint" style={{ fontSize: 10.5, marginTop: 6, lineHeight: 1.5 }}>
                  {m.resp.notice}
                </div>
              )}
            </div>
          ))}
          {busy && <div className="msg bot dim">Working — running tools against the simulation…</div>}
          <div ref={endRef} />
        </div>
      </div>

      <div className="section">
        <div className="row wrap" style={{ marginBottom: 8 }}>
          {suggestions.map((s) => (
            <button key={s} className="btn small" onClick={() => ask(s)} disabled={busy}>
              {s.length > 34 ? s.slice(0, 34) + '…' : s}
            </button>
          ))}
        </div>
        <div className="field">
          <textarea
            value={input}
            placeholder="Ask about the city, or pose a what-if…"
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) ask(input)
            }}
          />
        </div>
        <label className="check">
          <input type="checkbox" checked={approve} onChange={(e) => setApprove(e.target.checked)} />
          Allow it to change this live simulation
        </label>
        <button className="btn primary" onClick={() => ask(input)} disabled={busy || !input.trim()} style={{ width: '100%' }}>
          Send &nbsp;<span className="faint">⌘↵</span>
        </button>
      </div>
    </>
  )
}

/* --------------------------------------------------------------- chaos --- */

const EXPERIMENTS = [
  {
    id: 'checkpoint_recovery',
    name: 'Checkpoint recovery',
    blurb:
      'Discards the running state, restores the newest checkpoint and replays the command log forward. Reports whether the reconstructed state matches the original bit for bit.',
  },
  {
    id: 'corrupt_checkpoint',
    name: 'Corrupt a checkpoint',
    blurb:
      'Flips bytes inside the newest checkpoint and attempts to load it. A correct system refuses it; the result says which layer caught it.',
  },
  {
    id: 'determinism_probe',
    name: 'Determinism probe',
    blurb:
      'Forks the simulation, advances both arms with no intervention and compares digests. This is the CI determinism test, run against the live system.',
  },
  {
    id: 'event_storm',
    name: 'Event storm',
    blurb:
      'Floods the network with incidents. Observability should degrade — dropped events — while the tick rate holds, because effects are not the system of record.',
  },
  {
    id: 'region_overload',
    name: 'Region overload',
    blurb:
      'Concentrates demand into one district to expose partition imbalance in the phase timings.',
  },
]

export function ChaosPanel(props: { simId: string }) {
  const [running, setRunning] = useState<string | null>(null)
  const [results, setResults] = useState<ChaosResult[]>([])
  const [err, setErr] = useState<string | null>(null)

  const run = async (id: string) => {
    setRunning(id)
    setErr(null)
    try {
      const r = await api.chaos({ sim: props.simId, experiment: id })
      setResults((rs) => [r, ...rs].slice(0, 8))
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setRunning(null)
    }
  }

  return (
    <>
      <div className="section">
        <h3>Chaos lab</h3>
        <p className="dim" style={{ fontSize: 11.5, lineHeight: 1.55 }}>
          Controlled failure injection. Each experiment measures, breaks something, recovers and
          measures again — and reports honestly when recovery was not exact.
        </p>
        {EXPERIMENTS.map((x) => (
          <div key={x.id} style={{ marginBottom: 10 }}>
            <button className="btn" style={{ width: '100%', textAlign: 'left' }} disabled={running !== null} onClick={() => run(x.id)}>
              {running === x.id ? `running ${x.name}…` : x.name}
            </button>
            <p className="faint" style={{ fontSize: 11, lineHeight: 1.5, margin: '4px 0 0' }}>
              {x.blurb}
            </p>
          </div>
        ))}
        {err && <div className="err">{err}</div>}
      </div>

      {results.length > 0 && (
        <div className="section">
          <h3>Results</h3>
          {results.map((r, i) => (
            <div key={i} className={r.stateDiverged ? 'err' : 'ok'} style={{ marginBottom: 9 }}>
              <b>{r.experiment}</b>
              <div style={{ marginTop: 4, fontFamily: 'var(--mono)', fontSize: 10.5, lineHeight: 1.6 }}>
                tick {fmtInt(r.tickBefore)} → {fmtInt(r.tickAfter)}
                <br />
                digest {r.digestBefore} → {r.digestAfter}
                {r.recoveredFromTick !== undefined && r.recoveredFromTick > 0 && (
                  <>
                    <br />
                    recovered from tick {fmtInt(r.recoveredFromTick)}, replayed{' '}
                    {fmtInt(r.ticksReplayed ?? 0)} ticks in {fmtInt(r.recoveryMillis ?? 0)} ms
                  </>
                )}
                <br />
                throughput {fmtInt(r.ticksPerSecBefore)} → {fmtInt(r.ticksPerSecAfter)} ticks/s
              </div>
              {r.notes.map((n, j) => (
                <div key={j} style={{ marginTop: 5, fontSize: 11.5, lineHeight: 1.5 }}>
                  {n}
                </div>
              ))}
            </div>
          ))}
        </div>
      )}
    </>
  )
}

/* -------------------------------------------------------------- policy --- */

export function PolicyPanel(props: { snap: Snapshot; onChanged(): void }) {
  const p = props.snap.policy
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)

  const apply = async (patch: Record<string, unknown>) => {
    setBusy(true)
    try {
      await api.setPolicy(props.snap.id, patch)
      setMsg('Applied. It is recorded in the command log.')
      props.onChanged()
    } catch (e) {
      setMsg((e as Error).message)
    } finally {
      setBusy(false)
      window.setTimeout(() => setMsg(null), 3200)
    }
  }

  const slider = (
    label: string,
    key: string,
    value: number,
    min: number,
    max: number,
    suffix = '%',
  ) => (
    <div className="field" key={key}>
      <label>
        {label} — <span className="mono">{value}{suffix}</span>
      </label>
      <input
        type="range"
        min={min}
        max={max}
        value={value}
        disabled={busy}
        onChange={(e) => apply({ [key]: Number(e.target.value) })}
      />
    </div>
  )

  return (
    <div className="section">
      <h3>Live policy</h3>
      <p className="dim" style={{ fontSize: 11.5, lineHeight: 1.55 }}>
        These change the running simulation immediately. Every change is a command in the log, so
        the run stays replayable.
      </p>
      <label className="check">
        <input
          type="checkbox"
          checked={p.adaptiveSignals}
          disabled={busy}
          onChange={(e) => apply({ adaptiveSignals: e.target.checked })}
        />
        Queue-actuated traffic signals
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={p.emergencyPreemption}
          disabled={busy}
          onChange={(e) => apply({ emergencyPreemption: e.target.checked })}
        />
        Emergency vehicle signal preemption
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={p.congestionCharge}
          disabled={busy}
          onChange={(e) => apply({ congestionCharge: e.target.checked })}
        />
        Central district congestion charge
      </label>
      {slider('Transit fleet', 'transitVehiclesPct', p.transitVehiclesPct, 50, 400)}
      {slider('Drivers with live traffic info', 'rerouteAwarenessPct', p.rerouteAwarenessPct, 0, 100)}
      {slider('Speed limits', 'speedLimitPct', p.speedLimitPct, 50, 130)}
      {msg && <div className="ok">{msg}</div>}
    </div>
  )
}

/* --------------------------------------------------------- checkpoints --- */

export function CheckpointPanel(props: { simId: string }) {
  const [rows, setRows] = useState<{ tick: number; digest: string; uncompressedBytes: number; created: string }[]>([])
  const [cmds, setCmds] = useState<{ seq: number; tick: number; kind: string; text: string }[]>([])
  const [msg, setMsg] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      const [c, k] = await Promise.all([api.checkpoints(props.simId), api.commands(props.simId)])
      setRows(c.checkpoints ?? [])
      setCmds(k.commands ?? [])
    } catch {
      /* the panel is informational; a transient failure just shows stale data */
    }
  }
  useEffect(() => {
    load()
    const t = window.setInterval(load, 4000)
    return () => window.clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.simId])

  const restore = async (tick: number) => {
    setBusy(true)
    try {
      const r = await api.restore(props.simId, tick)
      setMsg(`Restored tick ${fmtInt(r.restoredFromTick)} and replayed ${fmtInt(r.ticksReplayed)} ticks in ${r.recoveryMillis} ms.`)
    } catch (e) {
      setMsg((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const totalBytes = useMemo(() => rows.reduce((a, r) => a + r.uncompressedBytes, 0), [rows])

  return (
    <>
      <div className="section">
        <h3>Checkpoints ({rows.length})</h3>
        {msg && <div className="ok">{msg}</div>}
        {rows.length === 0 ? (
          <div className="empty">No checkpoints yet. One is written every 30 simulated minutes.</div>
        ) : (
          <table className="cmp">
            <thead>
              <tr>
                <th>tick</th>
                <th>digest</th>
                <th>size</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {[...rows].reverse().map((r) => (
                <tr key={r.tick}>
                  <td>{fmtInt(r.tick)}</td>
                  <td style={{ fontSize: 10 }}>{r.digest.slice(0, 10)}</td>
                  <td>{Math.round(r.uncompressedBytes / 1024)} KB</td>
                  <td>
                    <button className="btn small" disabled={busy} onClick={() => restore(r.tick)}>
                      restore
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {rows.length > 0 && (
          <p className="faint" style={{ fontSize: 11, marginTop: 8, lineHeight: 1.5 }}>
            {Math.round(totalBytes / 1024)} KB of state across {rows.length} checkpoints. Restoring
            rolls back to the checkpoint and replays every command recorded since, which is the
            same code path crash recovery uses.
          </p>
        )}
      </div>

      <div className="section">
        <h3>Command log ({cmds.length})</h3>
        <p className="dim" style={{ fontSize: 11.5, lineHeight: 1.55 }}>
          The authoritative record. Initial parameters plus these commands reproduce this run
          exactly; everything else in the event feed is derived and is regenerated on replay.
        </p>
        <div className="feed">
          {cmds.slice(-60).reverse().map((c) => (
            <div className="ev notice" key={c.seq}>
              <div className="t">{fmtInt(c.tick)}</div>
              <div className="m">{c.text}</div>
            </div>
          ))}
        </div>
      </div>
    </>
  )
}
