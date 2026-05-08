export interface SessionInfo {
  authenticated: boolean
  first_run: boolean
}

export interface WatchedAnimal {
  name: string
  threshold: number
}

export interface CorrectionRule {
  detected: string
  correction: string
  regex?: boolean
}

export interface SpeciesCount {
  name: string
  count: number
}

export interface StatsTotals {
  pictures: number
  sightings_today: number
  sightings_7d: number
  species_30d: number
  disk_bytes: number
}

export interface YearTrend {
  species: string[]
  weeks: string[]
  series: number[][]
}

export interface StatsBundle {
  totals: StatsTotals
  top_7d: SpeciesCount[]
  year_trend: YearTrend
  hour_of_day: number[]
  generated_at: string
}

export interface AIQualityConfig {
  enabled?: boolean
  url?: string
  model?: string
  bearer_token?: string
  discard_threshold?: number
  normalize_width?: number
  max_candidates?: number
}

export interface EBirdConfig {
  enabled?: boolean
  api_key?: string
  region?: string
  lat?: number
  lng?: number
  dist_km?: number
  back_days?: number
}

export interface Config {
  rtsp_url: string
  http_addr: string
  detection_cooldown_s: number
  session_timeout_s: number
  auto_capture_enabled: boolean
  watched_animals: WatchedAnimal[]
  classifier_corrections: CorrectionRule[]
  ai_quality: AIQualityConfig
  ebird: EBirdConfig
  bird_confidence_threshold?: number
  bird_max_crops?: number
}

export interface BBox {
  x1: number
  y1: number
  x2: number
  y2: number
}

export interface SpeciesGuess {
  name: string
  confidence: number
}

export interface DetectionRecord {
  name: string
  confidence: number
  box: BBox
}

export interface BirdCrop {
  filename: string
  species?: SpeciesGuess[]
  ai_score?: number
  yolo_conf?: number
  box?: BBox
  user_species?: string
}

export interface Picture {
  name: string
  size: number
  mod_time: string
  species?: string
  manual: boolean
  detections?: DetectionRecord[]
  bird_species?: SpeciesGuess[]
  has_crop?: boolean
  user_species?: string
  user_notes?: string
  analyzed_at?: string
  reclassified_at?: string
  ai_quality_score?: number
  ai_quality_at?: string
  ai_quality_error?: string
  bird_crops?: BirdCrop[]
  hearted?: boolean
}

export interface BirdInfoImage {
  name?: string
  source: string
}

export interface BirdInfo {
  common_name: string
  scientific_name?: string
  description?: string
  conservation_status?: string
  family?: string
  genus?: string
  sound?: string
  male_image?: string
  female_image?: string
  other_images?: BirdInfoImage[]
  source?: string
}

export interface PictureMetadata {
  detections?: DetectionRecord[]
  bird_species?: SpeciesGuess[]
  bird_crop?: string
  bird_crops?: BirdCrop[]
  user_species?: string
  user_notes?: string
  analyzed_at?: string
  reclassified_at?: string
  ai_quality_score?: number
  ai_quality_at?: string
  ai_quality_error?: string
  ai_quality_raw?: string
}

export interface Detection {
  class_id: number
  name: string
  confidence: number
  box?: BBox
  species?: SpeciesGuess[]
}

export interface Status {
  rtsp_connected: boolean
  animals_present: Detection[]
  last_capture_at: string
  detector_ready: boolean
}

export interface DetectionLogEntry {
  id: number
  timestamp: string
  classes: string[]
  top_class: string
  top_confidence: number
  picture?: string
}

async function req<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, { credentials: 'same-origin', ...init })
  if (!r.ok) throw new Error((await r.text()) || r.statusText)
  const ct = r.headers.get('content-type') || ''
  if (ct.includes('application/json')) return r.json() as Promise<T>
  return undefined as unknown as T
}

export const api = {
  session: () => req<SessionInfo>('/api/session'),

  login: (password: string) =>
    req<void>('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    }),

  firstRunSetup: (password: string) =>
    req<void>('/api/first-run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    }),

  logout: () => req<void>('/api/logout', { method: 'POST' }),

  getConfig: () => req<Config>('/api/config'),

  saveConfig: (c: Partial<Config>) =>
    req<Config>('/api/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(c),
    }),

  applyCorrections: () =>
    req<{ processed: number; modified: number; first_error?: string }>(
      '/api/apply-corrections',
      { method: 'POST' },
    ),

  testAIQuality: (cfg: { url?: string; model?: string; bearer_token?: string }) =>
    req<{
      ok: boolean
      http_status?: number
      latency_ms?: number
      score?: number
      raw_response?: string
      error?: string
    }>('/api/ai-quality/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    }),

  testEBird: (cfg: {
    api_key?: string
    region?: string
    lat?: number
    lng?: number
    dist_km?: number
    back_days?: number
  }) =>
    req<{
      ok: boolean
      count?: number
      sample?: string[]
      latency_ms?: number
      error?: string
    }>('/api/ebird/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg),
    }),

  changePassword: (old_password: string, new_password: string) =>
    req<void>('/api/change-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ old_password, new_password }),
    }),

  testConnection: (rtsp_url: string) =>
    req<{ ok: boolean; error?: string }>('/api/test-rtsp', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rtsp_url }),
    }),

  capture: () => req<{ name: string }>('/api/capture', { method: 'POST' }),

  getStats: () => req<StatsBundle>('/api/stats'),

  pictures: () => req<Picture[]>('/api/pictures'),

  deletePicture: (name: string) =>
    req<void>(`/api/pictures/${encodeURIComponent(name)}`, { method: 'DELETE' }),

  heartPicture: (name: string, hearted?: boolean) =>
    req<{ hearted: boolean }>(`/api/pictures/${encodeURIComponent(name)}/heart`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(hearted === undefined ? {} : { hearted }),
    }),

  reclassifyPicture: (name: string, opts?: { destructive?: boolean }) =>
    req<{
      detections: Detection[]
      bird_species?: SpeciesGuess[]
      reclassified_at?: string
      has_crop?: boolean
      crop_count?: number
      quality_enabled?: boolean
      quality_score?: number
      quality_threshold?: number
      quality_error?: string
      non_bird?: boolean
    }>(
      `/api/pictures/${encodeURIComponent(name)}/reclassify${
        opts?.destructive ? '?destructive=true' : ''
      }`,
      { method: 'POST' },
    ),

  scorePictureQuality: (name: string, deleteBelowThreshold = false) =>
    req<{
      enabled: boolean
      score?: number
      threshold?: number
      deleted?: boolean
      delete_error?: string
      error?: string
    }>(
      `/api/pictures/${encodeURIComponent(name)}/quality${
        deleteBelowThreshold ? '?delete_below_threshold=true' : ''
      }`,
      { method: 'POST' },
    ),

  pictureMetadata: (name: string) =>
    req<PictureMetadata>(`/api/pictures/${encodeURIComponent(name)}/metadata`),

  savePictureMetadata: (
    name: string,
    patch: {
      user_species?: string
      user_notes?: string
      bird_species?: SpeciesGuess[]
      detections?: DetectionRecord[]
      crop_user_species?: Record<string, string>
    },
  ) =>
    req<PictureMetadata>(`/api/pictures/${encodeURIComponent(name)}/metadata`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch),
    }),

  status: () => req<Status>('/api/status'),

  classes: () => req<string[]>('/api/classes'),

  detectDebug: (threshold = 0.1, top = 20) =>
    req<Detection[]>(`/api/detect-debug?threshold=${threshold}&top=${top}`),

  birdInfo: (name: string) =>
    req<BirdInfo>(`/api/bird-info?name=${encodeURIComponent(name)}`),

  detections: (params: { limit?: number; before?: number } = {}) => {
    const q = new URLSearchParams()
    if (params.limit) q.set('limit', String(params.limit))
    if (params.before) q.set('before', String(params.before))
    const qs = q.toString()
    return req<DetectionLogEntry[]>(`/api/detections${qs ? '?' + qs : ''}`)
  },
}
