import type { CityMap, Incident, NetworkFrame, VehicleFrame } from '../lib/types'

/**
 * The map renderer.
 *
 * # Why Canvas2D and not Three.js
 *
 * The brief asked for Three.js "only where it adds value", so: it does not add
 * value here, and this is the reasoning.
 *
 * What this view has to do is show tens of thousands of point entities and a
 * road network of up to 40,000 links, in a way that stays legible while the
 * operator pans and zooms across an 8 km city. That is a 2D information
 * display. A perspective camera would put the far side of the city at a
 * different scale from the near side, which is precisely wrong for comparing
 * congestion across districts, and every extruded building would be occlusion
 * that hides the thing the page exists to show. WebGL would win on raw fill
 * rate at very high entity counts -- but the server already caps the stream at
 * a few thousand culled vehicles, because a 1600-pixel canvas cannot resolve
 * more, and at that count Canvas2D is comfortably inside frame budget.
 *
 * So the trade is: a 3D view would cost a renderer, a camera controller, a
 * picking implementation and a shader pipeline, and would buy a worse
 * information display. It is on the "what not to build" list on purpose.
 *
 * # How it draws
 *
 * Two layers. The road network is expensive to draw and changes at 2 Hz, so it
 * is rendered to an offscreen canvas and only rebuilt when the camera moves or
 * a new congestion frame arrives. Vehicles are cheap and change at 8 Hz, so
 * they are drawn on top every animation frame, with positions interpolated
 * between server frames so the motion is smooth at 60 fps rather than stepping
 * eight times a second.
 */

export interface Camera {
  /** Centre of the view, in world millimetres. */
  cx: number
  cy: number
  /** Pixels per world millimetre. */
  scale: number
}

export interface RenderOptions {
  showDistricts: boolean
  showTransit: boolean
  showInfra: boolean
  showRegions: boolean
  congestionOverlay: boolean
  labelDistricts: boolean
}

export interface HitResult {
  kind: 'vehicle' | 'edge' | 'poi'
  id: number
  label: string
}

// Road colours by class. Bright enough to read the street grid against the
// near-black ground without competing with the vehicle layer for attention:
// the network is context, the traffic on it is the subject.
const ROAD_BASE = ['#1b2431', '#25303f', '#33414f', '#465a6e']
const ROAD_WIDTH = [1.0, 1.5, 2.5, 3.8]

// Congestion ramp: free-flowing teal through amber to a saturated red.
//
// Chosen so the two ends stay distinguishable to the ~8% of men with a
// red-green deficiency, which a naive green-to-red ramp is not: lightness falls
// monotonically along the ramp, so the ordering survives both colour-vision
// deficiency and being printed in grey.
const RAMP: [number, number, number][] = [
  [240, 74, 94], // 0%   stopped
  [244, 108, 63], // 20%
  [242, 173, 60], // 40%
  [212, 205, 90], // 60%
  [122, 199, 140], // 80%
  [82, 150, 160], // 100% free flow -- deliberately muted
]

function rampColor(pct: number): string {
  const t = Math.max(0, Math.min(1, pct / 100)) * (RAMP.length - 1)
  const i = Math.min(RAMP.length - 2, Math.floor(t))
  const f = t - i
  const a = RAMP[i]
  const b = RAMP[i + 1]
  const r = Math.round(a[0] + (b[0] - a[0]) * f)
  const g = Math.round(a[1] + (b[1] - a[1]) * f)
  const bl = Math.round(a[2] + (b[2] - a[2]) * f)
  return `rgb(${r},${g},${bl})`
}

const REGION_HUES = [188, 268, 42, 340, 150, 20, 300, 90, 220, 0, 120, 260]

const VEHICLE_COLOR = ['#7cc6ff', '#b79bff', '#f2a2ff', '#ff6b81', '#ff9f45', '#67e8f9']
const VEHICLE_SIZE = [1.9, 2.9, 3.2, 3.0, 3.0, 2.6]

export class CityRenderer {
  private ctx: CanvasRenderingContext2D
  private road: HTMLCanvasElement
  private roadCtx: CanvasRenderingContext2D
  private dpr = 1
  private w = 0
  private h = 0
  private roadDirty = true
  private fitted = false

  private map: CityMap | null = null
  private net: NetworkFrame | null = null
  private incidents: Incident[] = []

  // Interpolation state, keyed by vehicle id.
  private prevX = new Map<number, number>()
  private prevY = new Map<number, number>()
  private curX = new Map<number, number>()
  private curY = new Map<number, number>()
  private frameAt = 0
  private framePeriod = 125

  private lastVehicles: VehicleFrame | null = null
  /** World-space positions of the last frame, for hit testing. */
  private hitX: Float64Array = new Float64Array(0)
  private hitY: Float64Array = new Float64Array(0)

  cam: Camera = { cx: 0, cy: 0, scale: 1 }
  opts: RenderOptions = {
    showDistricts: true,
    showTransit: false,
    showInfra: true,
    showRegions: false,
    congestionOverlay: true,
    labelDistricts: true,
  }

  constructor(private canvas: HTMLCanvasElement) {
    const c = canvas.getContext('2d', { alpha: false })
    if (!c) throw new Error('canvas 2d context unavailable')
    this.ctx = c
    this.road = document.createElement('canvas')
    const rc = this.road.getContext('2d', { alpha: true })
    if (!rc) throw new Error('offscreen canvas unavailable')
    this.roadCtx = rc
  }

  setMap(m: CityMap) {
    this.map = m
    this.fitted = false
    this.roadDirty = true
    this.prevX.clear()
    this.prevY.clear()
    this.curX.clear()
    this.curY.clear()
  }

  setNetwork(n: NetworkFrame) {
    this.net = n
    this.roadDirty = true
  }

  setIncidents(i: Incident[] | null | undefined) {
    this.incidents = i ?? []
  }

  setOptions(o: Partial<RenderOptions>) {
    this.opts = { ...this.opts, ...o }
    this.roadDirty = true
  }

  resize(w: number, h: number, dpr: number) {
    this.w = w
    this.h = h
    this.dpr = dpr
    this.canvas.width = Math.max(1, Math.floor(w * dpr))
    this.canvas.height = Math.max(1, Math.floor(h * dpr))
    this.canvas.style.width = `${w}px`
    this.canvas.style.height = `${h}px`
    this.road.width = this.canvas.width
    this.road.height = this.canvas.height
    this.roadDirty = true
    // The first layout pass often reports a zero-sized element, so a fit()
    // issued when the map arrives can silently do nothing and leave the camera
    // at its default scale of one pixel per millimetre -- which renders a city
    // eight kilometres across as a single road somewhere off screen. Re-fitting
    // on the first resize that has real dimensions removes that whole class of
    // "blank canvas on load" bug.
    if (!this.fitted && this.map && this.w > 0 && this.h > 0) this.fit()
  }

  /** Fits the whole city in view with a small margin. */
  fit() {
    if (!this.map || this.w <= 0 || this.h <= 0) return
    const pad = 0.94
    const s = Math.min(this.w / this.map.width, this.h / this.map.height) * pad
    this.cam = { cx: this.map.width / 2, cy: this.map.height / 2, scale: s }
    this.fitted = true
    this.roadDirty = true
  }

  panBy(dxPx: number, dyPx: number) {
    this.cam.cx -= dxPx / this.cam.scale
    this.cam.cy -= dyPx / this.cam.scale
    this.clampCamera()
    this.roadDirty = true
  }

  zoomAt(px: number, py: number, factor: number) {
    const before = this.toWorld(px, py)
    const min = this.map ? Math.min(this.w / this.map.width, this.h / this.map.height) * 0.4 : 1e-6
    this.cam.scale = Math.max(min, Math.min(this.cam.scale * factor, 0.02))
    const after = this.toWorld(px, py)
    this.cam.cx += before.x - after.x
    this.cam.cy += before.y - after.y
    this.clampCamera()
    this.roadDirty = true
  }

  private clampCamera() {
    if (!this.map) return
    // Allow a screen of overscroll so the city edge can be inspected, but not
    // so much that the operator can lose the city entirely.
    const marginX = this.w / this.cam.scale / 2
    const marginY = this.h / this.cam.scale / 2
    this.cam.cx = Math.max(-marginX, Math.min(this.map.width + marginX, this.cam.cx))
    this.cam.cy = Math.max(-marginY, Math.min(this.map.height + marginY, this.cam.cy))
  }

  toScreen(x: number, y: number): { x: number; y: number } {
    return {
      x: (x - this.cam.cx) * this.cam.scale + this.w / 2,
      y: (y - this.cam.cy) * this.cam.scale + this.h / 2,
    }
  }

  toWorld(px: number, py: number): { x: number; y: number } {
    return {
      x: (px - this.w / 2) / this.cam.scale + this.cam.cx,
      y: (py - this.h / 2) / this.cam.scale + this.cam.cy,
    }
  }

  /** The world rectangle currently visible, for the server-side viewport cull. */
  viewport(): { x0: number; y0: number; x1: number; y1: number } {
    const a = this.toWorld(0, 0)
    const b = this.toWorld(this.w, this.h)
    // A margin of one screen means a pan does not reveal an empty band before
    // the next frame arrives.
    const mx = (b.x - a.x) * 0.25
    const my = (b.y - a.y) * 0.25
    return { x0: a.x - mx, y0: a.y - my, x1: b.x + mx, y1: b.y + my }
  }

  pushVehicles(f: VehicleFrame) {
    // Roll current positions into previous, then decode the new frame into
    // current. Interpolation between the two runs on the animation clock.
    const t = performance.now()
    this.framePeriod = Math.max(60, Math.min(400, t - this.frameAt || 125))
    this.frameAt = t

    const tmpX = this.prevX
    const tmpY = this.prevY
    this.prevX = this.curX
    this.prevY = this.curY
    this.curX = tmpX
    this.curY = tmpY
    this.curX.clear()
    this.curY.clear()

    const spanX = f.x1 - f.x0
    const spanY = f.y1 - f.y0
    if (this.hitX.length < f.sent) {
      this.hitX = new Float64Array(Math.max(1024, f.sent * 2))
      this.hitY = new Float64Array(Math.max(1024, f.sent * 2))
    }
    for (let i = 0; i < f.sent; i++) {
      const wx = f.x0 + (f.nx[i] / 65535) * spanX
      const wy = f.y0 + (f.ny[i] / 65535) * spanY
      this.curX.set(f.ids[i], wx)
      this.curY.set(f.ids[i], wy)
      this.hitX[i] = wx
      this.hitY[i] = wy
    }
    this.lastVehicles = f

    // Bound the interpolation maps. Vehicle ids are recycled by the engine, so
    // this grows to roughly the peak concurrent fleet and then stops -- but a
    // long session on a shrinking network would otherwise retain every id ever
    // seen.
    if (this.prevX.size > 40000) {
      this.prevX.clear()
      this.prevY.clear()
    }
  }

  draw(now: number) {
    const ctx = this.ctx
    ctx.save()
    ctx.scale(this.dpr, this.dpr)
    ctx.fillStyle = '#080b10'
    ctx.fillRect(0, 0, this.w, this.h)

    if (!this.map) {
      ctx.restore()
      return
    }
    if (this.roadDirty) {
      this.drawRoadLayer()
      this.roadDirty = false
    }
    ctx.drawImage(this.road, 0, 0, this.w, this.h)
    this.drawVehicles(ctx, now)
    this.drawIncidents(ctx, now)
    ctx.restore()
  }

  // ------------------------------------------------------------- layers ---

  private drawRoadLayer() {
    const m = this.map!
    const ctx = this.roadCtx
    ctx.save()
    ctx.setTransform(1, 0, 0, 1, 0, 0)
    ctx.clearRect(0, 0, this.road.width, this.road.height)
    ctx.scale(this.dpr, this.dpr)

    const vp = this.viewport()
    if (this.opts.showDistricts) this.drawDistricts(ctx, vp)

    const scale = this.cam.scale
    // Level of detail.
    //
    // The first version dropped local streets whenever the view was zoomed out
    // past a threshold, on the theory that sub-pixel lines are wasted work.
    // Looking at it settled the question: most vehicles are on local streets,
    // so hiding those streets produced a screen full of cars driving across
    // open ground. The network is the frame of reference for everything else on
    // the canvas, and an incomplete frame of reference is worse than a slightly
    // slower one.
    //
    // So the whole network is drawn, and the threshold only bites on maps large
    // enough for it to matter -- above 30,000 edges, viewed from far enough out
    // that a local street is well under a pixel wide anyway.
    const minClass = m.edgeFrom.length > 30000 && scale < 0.00004 ? 1 : 0
    const net = this.net
    const overlay = this.opts.congestionOverlay && net != null

    // Two passes: casing then fill, so junctions read as continuous roads
    // rather than as a pile of separate line segments.
    for (let pass = 0; pass < 2; pass++) {
      for (let e = 0; e < m.edgeFrom.length; e++) {
        const cls = m.edgeClass[e]
        if (cls < minClass) continue
        const a = m.edgeFrom[e]
        const b = m.edgeTo[e]
        // Draw each undirected link once: the reverse edge has From > To for
        // exactly one of the pair.
        if (a > b) continue
        const x1 = m.nodeX[a]
        const y1 = m.nodeY[a]
        const x2 = m.nodeX[b]
        const y2 = m.nodeY[b]
        if (
          (x1 < vp.x0 && x2 < vp.x0) || (x1 > vp.x1 && x2 > vp.x1) ||
          (y1 < vp.y0 && y2 < vp.y0) || (y1 > vp.y1 && y2 > vp.y1)
        ) {
          continue
        }
        const p1 = this.toScreen(x1, y1)
        const p2 = this.toScreen(x2, y2)
        const w = Math.max(0.6, ROAD_WIDTH[cls] * Math.min(2.4, scale * 22000))

        if (pass === 0) {
          // Casing. A touch lighter than the ground so junctions read as
          // continuous roads instead of a heap of separate segments.
          ctx.strokeStyle = '#0d1219'
          ctx.lineWidth = w + 1.8
        } else if (overlay) {
          const v = net!.edgeSpeedPct[e]
          if (v === 254) {
            ctx.strokeStyle = '#b91c1c' // closed
          } else if (v === 255) {
            ctx.strokeStyle = '#c2740c' // lanes blocked
          } else if (v >= 92) {
            // Free-flowing links keep the structural colour. Painting the whole
            // network with the ramp would make a healthy city look like an
            // alarm, and would leave no visual headroom for the links that are
            // actually in trouble.
            ctx.strokeStyle = ROAD_BASE[cls]
          } else {
            ctx.strokeStyle = rampColor(v)
          }
          ctx.lineWidth = w
        } else {
          ctx.strokeStyle = ROAD_BASE[cls]
          ctx.lineWidth = w
        }
        ctx.lineCap = 'round'
        ctx.beginPath()
        ctx.moveTo(p1.x, p1.y)
        ctx.lineTo(p2.x, p2.y)
        ctx.stroke()
      }
    }

    if (this.opts.showTransit) this.drawTransit(ctx)
    if (this.opts.showInfra) this.drawInfrastructure(ctx, vp)
    if (this.opts.showDistricts && this.opts.labelDistricts) this.labelDistricts(ctx)
    ctx.restore()
  }

  private drawDistricts(ctx: CanvasRenderingContext2D, _vp: { x0: number; y0: number }) {
    const m = this.map!
    for (const d of m.districts) {
      const a = this.toScreen(d.minX, d.minY)
      const b = this.toScreen(d.maxX, d.maxY)
      if (this.opts.showRegions) {
        const hue = REGION_HUES[d.region % REGION_HUES.length]
        ctx.fillStyle = `hsla(${hue}, 62%, 52%, 0.075)`
        ctx.fillRect(a.x, a.y, b.x - a.x, b.y - a.y)
      }
      ctx.strokeStyle = 'rgba(120,150,190,0.13)'
      ctx.lineWidth = 1
      ctx.setLineDash([3, 5])
      ctx.strokeRect(a.x, a.y, b.x - a.x, b.y - a.y)
      ctx.setLineDash([])
    }
  }

  private labelDistricts(ctx: CanvasRenderingContext2D) {
    const m = this.map!
    ctx.font = '500 10px ui-monospace, SFMono-Regular, Menlo, monospace'
    ctx.textAlign = 'left'
    for (const d of m.districts) {
      const p = this.toScreen(d.minX, d.minY)
      if (p.x < -80 || p.y < -20 || p.x > this.w + 80 || p.y > this.h + 20) continue
      ctx.fillStyle = 'rgba(150,180,215,0.42)'
      const label = this.opts.showRegions ? `${d.name.toUpperCase()}  ·  W${d.region}` : d.name.toUpperCase()
      ctx.fillText(label, p.x + 8, p.y + 16)
    }
  }

  private drawTransit(ctx: CanvasRenderingContext2D) {
    const m = this.map!
    for (const r of m.routes) {
      ctx.strokeStyle = r.mode === 1 ? 'rgba(240,171,252,0.55)' : 'rgba(167,139,250,0.30)'
      ctx.lineWidth = r.mode === 1 ? 2 : 1.2
      ctx.setLineDash(r.mode === 1 ? [] : [6, 4])
      ctx.beginPath()
      r.stops.forEach((n, i) => {
        const p = this.toScreen(m.nodeX[n], m.nodeY[n])
        if (i === 0) ctx.moveTo(p.x, p.y)
        else ctx.lineTo(p.x, p.y)
      })
      ctx.stroke()
      ctx.setLineDash([])
    }
  }

  private drawInfrastructure(ctx: CanvasRenderingContext2D, vp: { x0: number; y0: number; x1: number; y1: number }) {
    const m = this.map!
    for (const p of m.pois) {
      if (p.x < vp.x0 || p.x > vp.x1 || p.y < vp.y0 || p.y > vp.y1) continue
      const s = this.toScreen(p.x, p.y)
      switch (p.kind) {
        case 'hospital':
          ctx.fillStyle = '#f87186'
          ctx.fillRect(s.x - 3.5, s.y - 1, 7, 2)
          ctx.fillRect(s.x - 1, s.y - 3.5, 2, 7)
          break
        case 'substation':
          ctx.strokeStyle = '#fbbf24'
          ctx.lineWidth = 1.2
          ctx.beginPath()
          ctx.moveTo(s.x + 1.5, s.y - 4)
          ctx.lineTo(s.x - 1.5, s.y)
          ctx.lineTo(s.x + 1, s.y)
          ctx.lineTo(s.x - 1.5, s.y + 4)
          ctx.stroke()
          break
        case 'depot':
          ctx.strokeStyle = 'rgba(103,232,249,0.7)'
          ctx.lineWidth = 1
          ctx.strokeRect(s.x - 2.5, s.y - 2.5, 5, 5)
          break
        case 'school':
          if (this.cam.scale < 0.0002) break
          ctx.fillStyle = 'rgba(148,163,184,0.5)'
          ctx.fillRect(s.x - 1.5, s.y - 1.5, 3, 3)
          break
      }
    }
  }

  private drawVehicles(ctx: CanvasRenderingContext2D, now: number) {
    const f = this.lastVehicles
    if (!f) return
    // Interpolate toward the newest frame. Clamped at 1 so a stall shows
    // vehicles stopped where they were rather than sliding off the map.
    const t = Math.max(0, Math.min(1, (now - this.frameAt) / this.framePeriod))
    const scale = this.cam.scale
    const sizeMul = Math.min(2.1, Math.max(0.55, scale * 26000))

    ctx.globalAlpha = 1
    for (let i = 0; i < f.sent; i++) {
      const id = f.ids[i]
      const cx = this.curX.get(id)!
      const cy = this.curY.get(id)!
      const px = this.prevX.get(id)
      const py = this.prevY.get(id)
      const wx = px === undefined ? cx : px + (cx - px) * t
      const wy = py === undefined ? cy : py + (cy - py) * t
      const s = this.toScreen(wx, wy)
      if (s.x < -8 || s.y < -8 || s.x > this.w + 8 || s.y > this.h + 8) continue

      const kind = f.kind[i]
      const r = VEHICLE_SIZE[kind] * sizeMul
      if (kind >= 3) {
        // Emergency vehicles get a pulsing halo. They are the entities an
        // operator is actually tracking, and there are never many.
        const pulse = 0.5 + 0.5 * Math.sin(now / 170 + id)
        ctx.fillStyle = `rgba(255,90,120,${0.16 + 0.2 * pulse})`
        ctx.beginPath()
        ctx.arc(s.x, s.y, r + 5 + 3 * pulse, 0, Math.PI * 2)
        ctx.fill()
      }
      ctx.fillStyle = VEHICLE_COLOR[kind]
      // Stopped traffic is drawn dimmer, which turns a jam into a visibly
      // duller, denser band rather than an indistinguishable clump of dots.
      ctx.globalAlpha = f.speedKph[i] < 4 ? 0.5 : 1
      ctx.beginPath()
      ctx.arc(s.x, s.y, r, 0, Math.PI * 2)
      ctx.fill()
    }
    ctx.globalAlpha = 1
  }

  private drawIncidents(ctx: CanvasRenderingContext2D, now: number) {
    for (const inc of this.incidents) {
      if (inc.resolved || (inc.x === 0 && inc.y === 0)) continue
      const s = this.toScreen(inc.x, inc.y)
      if (s.x < -40 || s.y < -40 || s.x > this.w + 40 || s.y > this.h + 40) continue
      const phase = (now / 1100) % 1
      const radius = 7 + phase * 26
      ctx.strokeStyle = `rgba(248,113,113,${0.75 * (1 - phase)})`
      ctx.lineWidth = 2
      ctx.beginPath()
      ctx.arc(s.x, s.y, radius, 0, Math.PI * 2)
      ctx.stroke()

      ctx.fillStyle = '#f87171'
      ctx.beginPath()
      ctx.arc(s.x, s.y, 3.2, 0, Math.PI * 2)
      ctx.fill()

      if (this.cam.scale > 0.00006) {
        ctx.font = '600 10px ui-monospace, SFMono-Regular, Menlo, monospace'
        ctx.fillStyle = 'rgba(254,202,202,0.92)'
        ctx.textAlign = 'left'
        const label = inc.awaitingUnits > 0 ? `${inc.kind} · awaiting ${inc.awaitingUnits}` : inc.kind
        ctx.fillText(label, s.x + 10, s.y - 8)
      }
    }
  }

  // -------------------------------------------------------- hit testing ---

  /** Finds the nearest entity to a screen point, within a pixel radius. */
  pick(px: number, py: number): HitResult | null {
    const m = this.map
    if (!m) return null
    const world = this.toWorld(px, py)
    const tolWorld = 9 / this.cam.scale

    const f = this.lastVehicles
    if (f) {
      let best = -1
      let bestD = tolWorld * tolWorld
      for (let i = 0; i < f.sent; i++) {
        const dx = this.hitX[i] - world.x
        const dy = this.hitY[i] - world.y
        const d = dx * dx + dy * dy
        if (d < bestD) {
          bestD = d
          best = i
        }
      }
      if (best >= 0) {
        return { kind: 'vehicle', id: f.ids[best], label: `vehicle ${f.ids[best]}` }
      }
    }

    // Infrastructure next: it is sparse and an operator clicking a hospital
    // icon means the hospital, not the road under it.
    let bestPoi = -1
    let bestPoiD = (14 / this.cam.scale) ** 2
    for (let i = 0; i < m.pois.length; i++) {
      const p = m.pois[i]
      if (p.kind === 'school') continue
      const dx = p.x - world.x
      const dy = p.y - world.y
      const d = dx * dx + dy * dy
      if (d < bestPoiD) {
        bestPoiD = d
        bestPoi = i
      }
    }
    if (bestPoi >= 0) {
      const p = m.pois[bestPoi]
      return { kind: 'poi', id: bestPoi, label: p.name }
    }

    // Finally the road under the cursor: nearest point on segment.
    let bestEdge = -1
    let bestEdgeD = tolWorld * tolWorld
    for (let e = 0; e < m.edgeFrom.length; e++) {
      const a = m.edgeFrom[e]
      const b = m.edgeTo[e]
      if (a > b) continue
      const x1 = m.nodeX[a]
      const y1 = m.nodeY[a]
      const x2 = m.nodeX[b]
      const y2 = m.nodeY[b]
      if (
        world.x < Math.min(x1, x2) - tolWorld || world.x > Math.max(x1, x2) + tolWorld ||
        world.y < Math.min(y1, y2) - tolWorld || world.y > Math.max(y1, y2) + tolWorld
      ) {
        continue
      }
      const vx = x2 - x1
      const vy = y2 - y1
      const len2 = vx * vx + vy * vy || 1
      let t = ((world.x - x1) * vx + (world.y - y1) * vy) / len2
      t = Math.max(0, Math.min(1, t))
      const dx = x1 + t * vx - world.x
      const dy = y1 + t * vy - world.y
      const d = dx * dx + dy * dy
      if (d < bestEdgeD) {
        bestEdgeD = d
        bestEdge = e
      }
    }
    if (bestEdge >= 0) return { kind: 'edge', id: bestEdge, label: `road ${bestEdge}` }
    return null
  }

  markDirty() {
    this.roadDirty = true
  }
}
