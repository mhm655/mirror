import { getKey } from './api'
import type {
  DistrictStat, EventView, Metrics, NetworkFrame, Series, Snapshot, VehicleFrame,
} from './types'

const MAGIC = 0x4d57
const VERSION = 3
const FRAME_VEHICLES = 1
const FRAME_NETWORK = 2

export interface StreamHandlers {
  onSnapshot?(s: Snapshot, m: Metrics, series: Series, districts: DistrictStat[]): void
  onEvents?(evs: EventView[], missed: number): void
  onVehicles?(f: VehicleFrame): void
  onNetwork?(f: NetworkFrame): void
  onStatus?(connected: boolean, detail?: string): void
}

/**
 * The live connection.
 *
 * Buffers are reused across frames. At 8 frames per second with up to 6,000
 * vehicles, allocating fresh typed arrays each time would hand the garbage
 * collector 200 MB a minute and show up as periodic jank in the animation --
 * which, on a page whose entire job is to look smooth, is the one thing that
 * cannot be allowed to happen.
 */
export class SimStream {
  private ws: WebSocket | null = null
  private closed = false
  private retry = 0
  private timer: number | undefined

  private vf: VehicleFrame = {
    tick: 0, total: 0, sent: 0, x0: 0, y0: 0, x1: 0, y1: 0,
    ids: new Int32Array(0), kind: new Uint8Array(0), status: new Uint8Array(0),
    speedKph: new Uint8Array(0), nx: new Uint16Array(0), ny: new Uint16Array(0),
  }
  private nf: NetworkFrame = {
    tick: 0, edgeSpeedPct: new Uint8Array(0),
    signalPhase: new Uint8Array(0), signalPowered: new Uint8Array(0),
  }

  constructor(
    private sim: string,
    private handlers: StreamHandlers,
  ) {
    this.connect()
  }

  private connect() {
    if (this.closed) return
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const key = getKey() ?? ''
    // The browser WebSocket API cannot set headers, so the credential travels
    // as a query parameter. The server accepts it on this route only.
    const url = `${proto}//${location.host}/ws?sim=${encodeURIComponent(this.sim)}&key=${encodeURIComponent(key)}`
    const ws = new WebSocket(url, 'mirror.v1')
    ws.binaryType = 'arraybuffer'
    this.ws = ws

    ws.onopen = () => {
      this.retry = 0
      this.handlers.onStatus?.(true)
      this.send({ type: 'subscribe', sim: this.sim })
    }
    ws.onclose = () => {
      this.handlers.onStatus?.(false, 'disconnected')
      this.scheduleReconnect()
    }
    ws.onerror = () => {
      this.handlers.onStatus?.(false, 'connection error')
    }
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') this.onText(ev.data)
      else this.onBinary(ev.data as ArrayBuffer)
    }
  }

  private scheduleReconnect() {
    if (this.closed) return
    // Exponential backoff with a ceiling. A console left open on a laptop lid
    // that has been shut for an hour should not come back and hammer the
    // server, and it should not give up either.
    this.retry = Math.min(this.retry + 1, 6)
    const delay = Math.min(500 * 2 ** this.retry, 15000)
    window.clearTimeout(this.timer)
    this.timer = window.setTimeout(() => this.connect(), delay)
  }

  private onText(data: string) {
    let msg: {
      type: string
      snapshot?: Snapshot
      metrics?: Metrics
      series?: Series
      districts?: DistrictStat[]
      events?: EventView[]
      missed?: number
    }
    try {
      msg = JSON.parse(data)
    } catch {
      return
    }
    if (msg.snapshot && msg.metrics && msg.series) {
      this.handlers.onSnapshot?.(msg.snapshot, msg.metrics, msg.series, msg.districts ?? [])
    }
    if (msg.events?.length) this.handlers.onEvents?.(msg.events, msg.missed ?? 0)
  }

  private onBinary(buf: ArrayBuffer) {
    const dv = new DataView(buf)
    if (buf.byteLength < 8) return
    if (dv.getUint16(0, true) !== MAGIC || dv.getUint16(2, true) !== VERSION) return
    switch (dv.getUint8(4)) {
      case FRAME_VEHICLES:
        this.decodeVehicles(dv, buf)
        break
      case FRAME_NETWORK:
        this.decodeNetwork(dv, buf)
        break
    }
  }

  private decodeVehicles(dv: DataView, buf: ArrayBuffer) {
    const HDR = 40
    const REC = 10
    if (buf.byteLength < HDR) return
    const f = this.vf
    f.tick = Number(dv.getBigUint64(8, true))
    f.x0 = dv.getInt32(16, true)
    f.y0 = dv.getInt32(20, true)
    f.x1 = dv.getInt32(24, true)
    f.y1 = dv.getInt32(28, true)
    f.total = dv.getUint32(32, true)
    const sent = dv.getUint32(36, true)
    const avail = Math.floor((buf.byteLength - HDR) / REC)
    f.sent = Math.min(sent, avail)

    if (f.ids.length < f.sent) {
      // Grow geometrically so a slowly rising vehicle count does not
      // reallocate every single frame.
      const cap = Math.max(1024, 1 << Math.ceil(Math.log2(f.sent + 1)))
      f.ids = new Int32Array(cap)
      f.kind = new Uint8Array(cap)
      f.status = new Uint8Array(cap)
      f.speedKph = new Uint8Array(cap)
      f.nx = new Uint16Array(cap)
      f.ny = new Uint16Array(cap)
    }
    let o = HDR
    for (let i = 0; i < f.sent; i++, o += REC) {
      f.ids[i] = dv.getUint32(o, true)
      const ks = dv.getUint8(o + 4)
      f.kind[i] = ks & 0x0f
      f.status[i] = ks >> 4
      f.speedKph[i] = dv.getUint8(o + 5)
      f.nx[i] = dv.getUint16(o + 6, true)
      f.ny[i] = dv.getUint16(o + 8, true)
    }
    this.handlers.onVehicles?.(f)
  }

  private decodeNetwork(dv: DataView, buf: ArrayBuffer) {
    const HDR = 24
    if (buf.byteLength < HDR) return
    const nEdges = dv.getUint32(16, true)
    const nSignals = dv.getUint32(20, true)
    const packedLen = Math.ceil(nSignals / 4)
    if (buf.byteLength < HDR + nEdges + packedLen) return

    const f = this.nf
    f.tick = Number(dv.getBigUint64(8, true))
    // The congestion array is copied rather than aliased: the renderer holds
    // it across frames to draw the road layer only when it changes, and an
    // aliased view into a recycled socket buffer would tear.
    if (f.edgeSpeedPct.length !== nEdges) f.edgeSpeedPct = new Uint8Array(nEdges)
    f.edgeSpeedPct.set(new Uint8Array(buf, HDR, nEdges))

    if (f.signalPhase.length !== nSignals) {
      f.signalPhase = new Uint8Array(nSignals)
      f.signalPowered = new Uint8Array(nSignals)
    }
    const packed = new Uint8Array(buf, HDR + nEdges, packedLen)
    for (let i = 0; i < nSignals; i++) {
      const bits = (packed[i >> 2] >> ((i % 4) * 2)) & 0x3
      f.signalPhase[i] = bits & 1
      f.signalPowered[i] = (bits >> 1) & 1
    }
    this.handlers.onNetwork?.(f)
  }

  send(msg: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg))
  }

  setViewport(x0: number, y0: number, x1: number, y1: number, maxVehicles: number) {
    this.send({ type: 'viewport', x0, y0, x1, y1, maxVehicles })
  }

  subscribe(sim: string) {
    this.sim = sim
    this.send({ type: 'subscribe', sim })
  }

  close() {
    this.closed = true
    window.clearTimeout(this.timer)
    this.ws?.close()
  }
}
