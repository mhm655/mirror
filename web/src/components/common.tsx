import { useEffect, useRef } from 'react'

export function fmtInt(n: number | undefined): string {
  if (n === undefined || !isFinite(n)) return '—'
  return Math.round(n).toLocaleString('en-GB')
}

export function fmtNum(n: number | undefined, dp = 1): string {
  if (n === undefined || !isFinite(n)) return '—'
  return n.toLocaleString('en-GB', { minimumFractionDigits: dp, maximumFractionDigits: dp })
}

/** Durations are shown as m:ss because an operator thinks in minutes. */
export function fmtDur(sec: number | undefined): string {
  if (sec === undefined || !isFinite(sec) || sec <= 0) return '—'
  const s = Math.round(sec)
  if (s < 60) return `${s}s`
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`
}

export function fmtClock(h: number, m: number): string {
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`
}

export function Tile(props: { k: string; v: string; s?: string; tone?: 'ok' | 'warn' | 'bad' }) {
  const color =
    props.tone === 'bad' ? 'var(--red)' : props.tone === 'warn' ? 'var(--amber)' : undefined
  return (
    <div className="tile">
      <div className="k">{props.k}</div>
      <div className="v" style={color ? { color } : undefined}>
        {props.v}
      </div>
      {props.s && <div className="s">{props.s}</div>}
    </div>
  )
}

export function KV(props: { k: string; v: React.ReactNode }) {
  return (
    <>
      <div className="k">{props.k}</div>
      <div className="v">{props.v}</div>
    </>
  )
}

export function Bar(props: { pct: number; warnAt?: number; badAt?: number }) {
  const pct = Math.max(0, Math.min(100, props.pct))
  const cls = pct >= (props.badAt ?? 90) ? 'bad' : pct >= (props.warnAt ?? 70) ? 'warn' : ''
  return (
    <div className="bar">
      <i className={cls} style={{ width: `${pct}%` }} />
    </div>
  )
}

/**
 * A sparkline.
 *
 * Hand-drawn on a canvas rather than pulled from a chart library. Six of these
 * update twice a second against a 1,440-point series; a general-purpose charting
 * library would rebuild an SVG scene graph each time, and the whole feature is
 * forty lines of canvas calls. The library would also bring its own opinions
 * about colour and typography into a page that has very specific ones.
 */
export function Spark(props: {
  data: number[]
  color?: string
  height?: number
  max?: number
  fill?: boolean
}) {
  const ref = useRef<HTMLCanvasElement>(null)
  const { data, color = 'var(--accent)', height = 54, max, fill = true } = props

  useEffect(() => {
    const cv = ref.current
    if (!cv) return
    const dpr = window.devicePixelRatio || 1
    const w = cv.clientWidth
    const h = height
    cv.width = Math.max(1, Math.floor(w * dpr))
    cv.height = Math.max(1, Math.floor(h * dpr))
    const ctx = cv.getContext('2d')
    if (!ctx) return
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, w, h)

    if (data.length < 2) {
      ctx.fillStyle = 'rgba(120,140,170,0.35)'
      ctx.font = '10px ui-monospace, monospace'
      ctx.fillText('collecting…', 4, h / 2)
      return
    }
    const resolved = getComputedStyle(document.documentElement)
    const stroke = color.startsWith('var(')
      ? resolved.getPropertyValue(color.slice(4, -1)).trim() || '#42bec2'
      : color

    const hi = max ?? Math.max(1, ...data)
    const n = data.length
    const x = (i: number) => (i / (n - 1)) * w
    const y = (v: number) => h - 3 - (Math.max(0, v) / hi) * (h - 7)

    // Baseline grid: two faint rules, enough to read a level against without
    // turning a 54-pixel-tall chart into a lattice.
    ctx.strokeStyle = 'rgba(120,140,170,0.10)'
    ctx.lineWidth = 1
    for (const f of [0.5, 1]) {
      const yy = Math.round(y(hi * f)) + 0.5
      ctx.beginPath()
      ctx.moveTo(0, yy)
      ctx.lineTo(w, yy)
      ctx.stroke()
    }

    if (fill) {
      const grad = ctx.createLinearGradient(0, 0, 0, h)
      grad.addColorStop(0, stroke + '38')
      grad.addColorStop(1, stroke + '00')
      ctx.fillStyle = grad
      ctx.beginPath()
      ctx.moveTo(0, h)
      for (let i = 0; i < n; i++) ctx.lineTo(x(i), y(data[i]))
      ctx.lineTo(w, h)
      ctx.closePath()
      ctx.fill()
    }

    ctx.strokeStyle = stroke
    ctx.lineWidth = 1.4
    ctx.lineJoin = 'round'
    ctx.beginPath()
    for (let i = 0; i < n; i++) {
      const px = x(i)
      const py = y(data[i])
      if (i === 0) ctx.moveTo(px, py)
      else ctx.lineTo(px, py)
    }
    ctx.stroke()

    // Mark the latest sample, because the whole point is "where are we now".
    ctx.fillStyle = stroke
    ctx.beginPath()
    ctx.arc(x(n - 1), y(data[n - 1]), 2.1, 0, Math.PI * 2)
    ctx.fill()
  }, [data, color, height, max, fill])

  return <canvas ref={ref} className="chart" style={{ height }} />
}

export function ChartBlock(props: {
  label: string
  value: string
  data: number[]
  color?: string
  max?: number
}) {
  return (
    <div style={{ marginBottom: 12 }}>
      <div className="chart-label">
        <span>{props.label}</span>
        <span>{props.value}</span>
      </div>
      <Spark data={props.data} color={props.color} max={props.max} />
    </div>
  )
}
