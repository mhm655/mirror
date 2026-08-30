// Shapes mirroring the Go API. Hand-written rather than generated: the surface
// is small, and a generator would be one more build step to keep working for a
// payoff measured in a few dozen lines.

export interface PolicyView {
  name: string
  adaptiveSignals: boolean
  adaptiveMaxExtendSec: number
  emergencyPreemption: boolean
  transitVehiclesPct: number
  rerouteAwarenessPct: number
  speedLimitPct: number
  congestionCharge: boolean
  ambulanceSurgePct: number
}

export interface LiveView {
  activeVehicles: number
  cars: number
  buses: number
  metros: number
  emergency: number
  agentsTravelling: number
  agentsAtHome: number
  agentsAtWork: number
  agentsStranded: number
  openIncidents: number
  substationsOnline: number
  substationsTotal: number
  signalsDark: number
  hospitalUtilPct: number
  avgSpeedKph: number
  congestedPct: number
  weather: string
  tempC: number
}

export interface PerfView {
  tickMillis: number
  phaseAMillis: number
  phaseBMillis: number
  serialPercent: number
  intents: number
  crossings: number
  routeQueries: number
  eventsDropped: number
}

export interface Snapshot {
  id: string
  name: string
  parentId?: string
  branchTick?: number
  state: 'paused' | 'running' | 'completed' | 'failed'
  speed: number
  tick: number
  clockHour: number
  clockMin: number
  simSeconds: number
  ticksPerSecond: number
  population: number
  mapPreset: string
  mapHash: string
  seed: number
  digest: string
  regions: number
  workers: number
  checkpoints: number
  lastError?: string
  policy: PolicyView
  live: LiveView
  perf: PerfView
}

export interface Metrics {
  tripsStarted: number
  tripsCompleted: number
  tripsAbandoned: number
  travelMeanSec: number
  travelP50Sec: number
  travelP95Sec: number
  travelP99Sec: number
  delayMeanSec: number
  delayP95Sec: number
  emergencyDispatched: number
  emergencyArrived: number
  responseMeanSec: number
  responseP50Sec: number
  responseP95Sec: number
  incidentsOpened: number
  incidentsResolved: number
  casualties: number
  hospitalAdmissions: number
  hospitalRejections: number
  hospitalDiversions: number
  peakHospitalUtilPct: number
  vehicleKm: number
  fuelLitres: number
  co2Kg: number
  stoppedVehicleHours: number
  transitBoardings: number
  transitDenied: number
  outageUnitTicks: number
  substationTrips: number
  commsDropped: number
  reroutes: number
  routeFailures: number
}

export interface Series {
  activeVehicles: number[]
  avgSpeedKph: number[]
  congestionPct: number[]
  hospitalPct: number[]
  poweredPct: number[]
  openIncidents: number[]
}

export interface DistrictStat {
  id: number
  name: string
  vehicles: number
  avgSpeedKph: number
  congestedPct: number
  openIncidents: number
  poweredPct: number
  region: number
}

export type Severity = 'info' | 'notice' | 'warning' | 'critical'

export interface EventView {
  seq: number
  tick: number
  kind: string
  severity: Severity
  region: number
  text: string
  a: number
  b: number
}

export interface DistrictShape {
  id: number
  name: string
  minX: number
  minY: number
  maxX: number
  maxY: number
  region: number
}

export interface PoiView {
  kind: string
  name: string
  x: number
  y: number
  id: number
  capacity: number
}

export interface RouteView {
  id: number
  name: string
  mode: number
  stops: number[]
}

/** The static world, fetched once per simulation and held in typed arrays. */
export interface CityMap {
  name: string
  hash: string
  width: number
  height: number
  nodeX: Int32Array
  nodeY: Int32Array
  nodeSignal: Int32Array
  edgeFrom: Int32Array
  edgeTo: Int32Array
  edgeClass: Uint8Array
  edgeDistrict: Int32Array
  districts: DistrictShape[]
  pois: PoiView[]
  signalNodes: Int32Array
  routes: RouteView[]
}

export interface Incident {
  id: number
  kind: string
  edge: number
  district: string
  startTick: number
  severity: number
  casualties: number
  responseSec: number
  resolved: boolean
  awaitingUnits: number
  x: number
  y: number
}

export interface ComparisonRow {
  key: string
  label: string
  unit: string
  lowerIsBetter: boolean
  values: Record<string, number>
  deltaPct: Record<string, number>
}

export interface ScenarioBrief {
  id: string
  name: string
  tick: number
  parentId?: string
  branchTick?: number
  policy: PolicyView
  metrics: Metrics
}

export interface Comparison {
  baselineId: string
  scenarios: ScenarioBrief[]
  rows: ComparisonRow[]
  warnings: string[]
}

export interface AgentStep {
  tool: string
  tier: string
  input: unknown
  output?: unknown
  error?: string
  millis: number
}

export interface AgentResponse {
  reply: string
  steps: AgentStep[]
  model: string
  planner: 'llm' | 'builtin'
  turns: number
  truncated?: boolean
  notice?: string
}

export interface ChaosResult {
  experiment: string
  sim: string
  description: string
  digestBefore: string
  digestAfter: string
  tickBefore: number
  tickAfter: number
  recoveredFromTick?: number
  ticksReplayed?: number
  recoveryMillis?: number
  stateDiverged?: boolean
  eventsDropped: number
  ticksPerSecBefore: number
  ticksPerSecAfter: number
  notes: string[]
}

/** Decoded live vehicle frame, held as flat arrays for the renderer. */
export interface VehicleFrame {
  tick: number
  total: number
  sent: number
  /** Viewport the server culled against, in world millimetres. */
  x0: number
  y0: number
  x1: number
  y1: number
  ids: Int32Array
  kind: Uint8Array
  status: Uint8Array
  speedKph: Uint8Array
  /** Normalised 0..65535 within the viewport. */
  nx: Uint16Array
  ny: Uint16Array
}

/** Decoded network frame: one byte per edge, two bits per signal. */
export interface NetworkFrame {
  tick: number
  /** 0..200 = percent of free-flow speed, 254 = closed, 255 = lanes blocked. */
  edgeSpeedPct: Uint8Array
  signalPhase: Uint8Array
  signalPowered: Uint8Array
}

export const VEHICLE_KIND = ['car', 'bus', 'metro', 'ambulance', 'fire', 'police'] as const
