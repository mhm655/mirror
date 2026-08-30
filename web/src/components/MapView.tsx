import { useEffect, useRef, useState } from 'react'
import { CityRenderer, type HitResult, type RenderOptions } from '../render/renderer'
import type { CityMap, Incident, NetworkFrame, VehicleFrame } from '../lib/types'
import { fmtInt } from './common'

export interface MapHandle {
  pushVehicles(f: VehicleFrame): void
  setNetwork(f: NetworkFrame): void
  setIncidents(i: Incident[]): void
  fit(): void
}

export function MapView(props: {
  map: CityMap | null
  connected: boolean
  onViewport(x0: number, y0: number, x1: number, y1: number, maxVehicles: number): void
  onPick(hit: HitResult | null): void
  onReady(h: MapHandle): void
  hudActive: number
  hudTotal: number
  hudSpeed: number
  hudCongested: number
}) {
  const wrapRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const rendRef = useRef<CityRenderer | null>(null)
  const [dragging, setDragging] = useState(false)
  const [opts, setOpts] = useState<RenderOptions>({
    showDistricts: true,
    showTransit: false,
    showInfra: true,
    showRegions: false,
    congestionOverlay: true,
    labelDistricts: true,
  })

  // Set up the renderer once; the animation loop and observers live with it.
  useEffect(() => {
    const canvas = canvasRef.current
    const wrap = wrapRef.current
    if (!canvas || !wrap) return
    const r = new CityRenderer(canvas)
    rendRef.current = r

    const resize = () => {
      const rect = wrap.getBoundingClientRect()
      r.resize(rect.width, rect.height, window.devicePixelRatio || 1)
    }
    resize()
    const ro = new ResizeObserver(resize)
    ro.observe(wrap)

    let raf = 0
    const loop = (t: number) => {
      r.draw(t)
      raf = requestAnimationFrame(loop)
    }
    raf = requestAnimationFrame(loop)

    props.onReady({
      pushVehicles: (f) => r.pushVehicles(f),
      setNetwork: (f) => r.setNetwork(f),
      setIncidents: (i) => r.setIncidents(i),
      fit: () => r.fit(),
    })

    return () => {
      cancelAnimationFrame(raf)
      ro.disconnect()
      rendRef.current = null
    }
    // Mount-only: the handle is stable and re-running this would tear down the
    // canvas on every parent render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const r = rendRef.current
    if (!r || !props.map) return
    r.setMap(props.map)
    r.fit()
    pushViewport()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.map])

  useEffect(() => {
    rendRef.current?.setOptions(opts)
  }, [opts])

  // The server culls to the viewport, so it needs to know what the viewport is.
  // Sent on a timer rather than on every pan frame: at 60 fps a drag would
  // otherwise push 60 control messages a second for a value the server samples
  // eight times a second.
  const pushViewport = () => {
    const r = rendRef.current
    if (!r) return
    const vp = r.viewport()
    const px = (wrapRef.current?.clientWidth ?? 1000) * (wrapRef.current?.clientHeight ?? 700)
    // Roughly one vehicle per 260 square pixels keeps dots distinguishable.
    const maxVehicles = Math.max(800, Math.min(9000, Math.floor(px / 260)))
    props.onViewport(vp.x0, vp.y0, vp.x1, vp.y1, maxVehicles)
  }

  useEffect(() => {
    const t = window.setInterval(pushViewport, 400)
    return () => window.clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // ------------------------------------------------------------- input ---

  const drag = useRef<{ x: number; y: number; moved: number } | null>(null)

  const onPointerDown = (e: React.PointerEvent) => {
    ;(e.target as Element).setPointerCapture(e.pointerId)
    drag.current = { x: e.clientX, y: e.clientY, moved: 0 }
    setDragging(true)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const d = drag.current
    if (!d) return
    const dx = e.clientX - d.x
    const dy = e.clientY - d.y
    d.x = e.clientX
    d.y = e.clientY
    d.moved += Math.abs(dx) + Math.abs(dy)
    rendRef.current?.panBy(dx, dy)
  }
  const onPointerUp = (e: React.PointerEvent) => {
    const d = drag.current
    drag.current = null
    setDragging(false)
    // A click is a drag that went nowhere. Without this, every pan would also
    // select whatever happened to be under the cursor when the mouse came up.
    if (d && d.moved < 4) {
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
      const hit = rendRef.current?.pick(e.clientX - rect.left, e.clientY - rect.top) ?? null
      props.onPick(hit)
    }
    pushViewport()
  }
  const onWheel = (e: React.WheelEvent) => {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
    const factor = Math.exp(-e.deltaY * 0.0016)
    rendRef.current?.zoomAt(e.clientX - rect.left, e.clientY - rect.top, factor)
    pushViewport()
  }

  const toggle = (k: keyof RenderOptions) => setOpts((o) => ({ ...o, [k]: !o[k] }))

  return (
    <div className="mapwrap" ref={wrapRef}>
      <canvas
        ref={canvasRef}
        className={`city${dragging ? ' drag' : ''}`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onWheel={onWheel}
      />

      <div className="map-overlay map-hud">
        <div>
          <b>{fmtInt(props.hudActive)}</b> shown&nbsp; / &nbsp;{fmtInt(props.hudTotal)} on network
        </div>
        <div>
          mean <b>{props.hudSpeed}</b> km/h&nbsp; · &nbsp;<b>{props.hudCongested}%</b> links congested
        </div>
      </div>

      <div className="map-overlay map-tools">
        <button className={`btn small${opts.congestionOverlay ? ' active' : ''}`} onClick={() => toggle('congestionOverlay')}>
          congestion
        </button>
        <button className={`btn small${opts.showDistricts ? ' active' : ''}`} onClick={() => toggle('showDistricts')}>
          districts
        </button>
        <button className={`btn small${opts.showRegions ? ' active' : ''}`} onClick={() => toggle('showRegions')}>
          workers
        </button>
        <button className={`btn small${opts.showTransit ? ' active' : ''}`} onClick={() => toggle('showTransit')}>
          transit
        </button>
        <button className={`btn small${opts.showInfra ? ' active' : ''}`} onClick={() => toggle('showInfra')}>
          infrastructure
        </button>
        <button className="btn small" onClick={() => { rendRef.current?.fit(); pushViewport() }}>
          fit
        </button>
      </div>

      <div className="map-overlay map-legend">
        <div className="legend-ramp" />
        <div className="legend-row" style={{ justifyContent: 'space-between', width: 132 }}>
          <span>stopped</span>
          <span>free flow</span>
        </div>
        <div className="legend-row">
          <i className="swatch" style={{ background: '#7cc6ff' }} /> car
          <i className="swatch" style={{ background: '#b79bff' }} /> bus
          <i className="swatch" style={{ background: '#f2a2ff' }} /> metro
          <i className="swatch" style={{ background: '#ff6b81' }} /> emergency
        </div>
      </div>

      <div className={`conn${props.connected ? ' up' : ''}`}>
        <i className="led" />
        {props.connected ? 'live' : 'reconnecting'}
      </div>
    </div>
  )
}
