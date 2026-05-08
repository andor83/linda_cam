<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import {
  api,
  type BBox,
  type BirdInfo,
  type BirdInfoImage,
  type Picture,
  type PictureMetadata,
} from '../api'
import { captureSignal } from '../bus'

const pictures = ref<Picture[]>([])
const viewing = ref<Picture | null>(null)
const loading = ref(false)
const err = ref('')
const reclassifying = ref<Set<string>>(new Set())
const search = ref('')

// Info modal state. infoFor is the picture currently open in the modal;
// infoMd holds the freshest metadata (refetched on open so we get any edits
// from another tab/session). The form* refs are local edit copies so the
// user can tweak everything and either Save or Close to discard.
const infoFor = ref<Picture | null>(null)
const infoMd = ref<PictureMetadata | null>(null)
const infoLoading = ref(false)
const infoSaving = ref(false)
const infoErr = ref('')
const formSpecies = ref('')
const formNotes = ref('')
// Per-crop user-confirmed species, keyed by crop index. Initialized
// from infoFor.bird_crops[i].user_species on modal open. Saved back
// to the row via metadataPatch.crop_user_species.
const formCropSpecies = ref<Map<number, string>>(new Map())
const infoTab = ref<'analysis' | 'debug'>('analysis')

function prettyJSON(raw: string | undefined | null): string {
  if (!raw) return '(empty)'
  const trimmed = raw.trim()
  if (!trimmed) return '(empty)'
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2)
  } catch {
    // Not valid JSON — show as-is so the user sees what the model
    // actually returned (markdown fences, prose, etc).
    return raw
  }
}

function debugDump(): string {
  if (!infoFor.value) return ''
  const md = infoMd.value ?? {}
  return JSON.stringify(
    {
      name: infoFor.value.name,
      hearted: infoFor.value.hearted ?? false,
      analyzed_at: md.analyzed_at ?? null,
      reclassified_at: md.reclassified_at ?? null,
      ai_quality_score: md.ai_quality_score ?? null,
      ai_quality_at: md.ai_quality_at ?? null,
      ai_quality_error: md.ai_quality_error ?? null,
      bird_species: md.bird_species ?? [],
      detections: md.detections ?? [],
      user_species: md.user_species ?? '',
      user_notes: md.user_notes ?? '',
    },
    null,
    2,
  )
}
// Read-only views of the modal's species + detection data, sourced from
// infoMd when fresh, otherwise from the picture record. Used by the info
// modal display sections AND the bbox overlay over the modal image.
const modalSpecies = computed<{ name: string; confidence: number }[]>(() => {
  // Multi-crop sighting: surface the selected crop's species.
  const p = infoFor.value
  if (p && cropCount(p) > 0) {
    const c = p.bird_crops![currentCropIdx(p)]
    if (c.species && c.species.length > 0) return c.species
  }
  return infoMd.value?.bird_species ?? infoFor.value?.bird_species ?? []
})
const modalDetections = computed<{ name: string; confidence: number; box: BBox }[]>(() => {
  // Multi-crop bird sighting: surface only the currently-selected
  // crop's bbox + species so the overlay rectangle on the modal
  // image highlights which bird the species panel below describes.
  // Cycling crops via the header arrows updates this directly.
  const p = infoFor.value
  if (p && cropCount(p) > 0 && p.bird_crops) {
    const c = p.bird_crops[currentCropIdx(p)]
    if (c.box) {
      const top = c.species?.[0]
      return [
        {
          name: top?.name ?? 'Bird',
          confidence: top?.confidence ?? 0,
          box: c.box,
        },
      ]
    }
  }
  return (infoMd.value?.detections ?? infoFor.value?.detections ?? []) as {
    name: string
    confidence: number
    box: BBox
  }[]
})

// Fullscreen "picture-in-picture" view: the main image fills the viewport
// and the bird-classifier crop overlays in the bottom-right corner.
const pip = ref(false)

// Shared (per-component-instance) cache for Ornithophile lookups so the
// hover popover and modal section don't duplicate fetches. `null` is a
// negative cache entry so a 404 doesn't keep trying.
const birdInfoCache = ref<Map<string, BirdInfo | null>>(new Map())
const birdInfoInflight = new Map<string, Promise<BirdInfo | null>>()

async function fetchBirdInfo(name: string): Promise<BirdInfo | null> {
  const key = name.toLowerCase().trim()
  if (!key) return null
  if (birdInfoCache.value.has(key)) return birdInfoCache.value.get(key) ?? null
  let p = birdInfoInflight.get(key)
  if (!p) {
    p = api.birdInfo(name).then(
      (v) => {
        birdInfoCache.value.set(key, v)
        birdInfoInflight.delete(key)
        return v
      },
      () => {
        birdInfoCache.value.set(key, null)
        birdInfoInflight.delete(key)
        return null
      },
    )
    birdInfoInflight.set(key, p)
  }
  return p
}

// Pick the best species name to look up for a given picture. Prefers the
// user-curated override, falls back to the top classifier guess, returns
// null when no fine-grained name is available (e.g. plain "Bird").
function lookupSpeciesFor(p: Picture | null | undefined): string | null {
  if (!p) return null
  if (p.user_species && p.user_species.trim()) return p.user_species.trim()
  // Multi-crop: per-crop human override wins; otherwise prefer the
  // highest-confidence classifier guess across all crops.
  if (p.bird_crops && p.bird_crops.length > 0) {
    for (const c of p.bird_crops) {
      const cu = c.user_species?.trim()
      if (cu) return cu
    }
    let bestName: string | null = null
    let bestConf = 0
    for (const c of p.bird_crops) {
      if (c.species && c.species.length > 0) {
        const top = c.species[0]
        if (top.confidence > bestConf && top.name && top.name.trim()) {
          bestConf = top.confidence
          bestName = top.name.trim()
        }
      }
    }
    if (bestName) return bestName
  }
  const top = p.bird_species?.[0]?.name
  return top && top.trim() ? top : null
}

// birdsForRow returns one display entry per detected bird in the
// thumbnail row. Each entry carries the raw species name (for the
// Ornithophile lookup) and the formatted display string (name +
// confidence). User-confirmed species takes precedence and produces
// a single entry without a confidence percentage. Multi-crop bird
// sightings produce one entry per crop; legacy single-crop sightings
// produce one entry from BirdSpecies.
function birdsForRow(
  p: Picture,
): { name: string; display: string }[] {
  const u = p.user_species?.trim()
  if (u) return [{ name: u, display: u }]
  if (p.bird_crops && p.bird_crops.length > 0) {
    const out: { name: string; display: string }[] = []
    for (const c of p.bird_crops) {
      // Per-crop human override wins over the classifier guess.
      const cu = c.user_species?.trim()
      if (cu) {
        out.push({ name: cu, display: cu })
        continue
      }
      if (c.species && c.species.length > 0) {
        const top = c.species[0]
        if (!top.name || !top.name.trim()) continue
        out.push({
          name: top.name,
          display: `${top.name} ${(top.confidence * 100).toFixed(0)}%`,
        })
      }
    }
    if (out.length > 0) return out
  }
  if (p.bird_species && p.bird_species.length > 0) {
    const top = p.bird_species[0]
    if (top.name && top.name.trim()) {
      return [
        {
          name: top.name,
          display: `${top.name} ${(top.confidence * 100).toFixed(0)}%`,
        },
      ]
    }
  }
  return []
}

// Hover popover state. hoverFor pins the popover to a single picture so
// repeated mouseenters on the same row don't restart the fetch.
const hoverFor = ref<string | null>(null)
const hoverPos = ref<{ top: number; left: number } | null>(null)
const hoverInfo = ref<BirdInfo | null>(null)
const hoverLoading = ref(false)
let hoverHideTimer: number | undefined

// onBirdNameHover positions the Ornithophile popover next to the
// hovered element and fetches info for `name`. `key` uniquely
// identifies the hovered element (e.g. "<picture>:<crop_idx>") so the
// popover doesn't restart when the cursor jitters within one entry.
function onBirdNameHover(key: string, name: string, ev: MouseEvent) {
  if (!name) return
  if (hoverHideTimer) {
    window.clearTimeout(hoverHideTimer)
    hoverHideTimer = undefined
  }
  if (hoverFor.value === key) return
  const target = ev.currentTarget as HTMLElement | null
  if (!target) return
  const rect = target.getBoundingClientRect()
  const POP_W = 360
  let left = rect.right + 8
  if (left + POP_W > window.innerWidth - 8) {
    left = Math.max(8, rect.left - POP_W - 8)
  }
  let top = rect.top
  if (top + 320 > window.innerHeight - 8) {
    top = Math.max(8, window.innerHeight - 328)
  }
  hoverPos.value = { top, left }
  hoverFor.value = key
  hoverInfo.value = null
  hoverLoading.value = true
  fetchBirdInfo(name).then((info) => {
    if (hoverFor.value !== key) return
    hoverInfo.value = info
    hoverLoading.value = false
  })
}

// onSpeciesEnter is the legacy single-name entry point used by the
// modal title hover. Keeps the existing call sites working.
function onSpeciesEnter(p: Picture, ev: MouseEvent) {
  const name = lookupSpeciesFor(p)
  if (!name) return
  onBirdNameHover(p.name, name, ev)
}

function onSpeciesLeave() {
  hoverHideTimer = window.setTimeout(() => {
    hoverFor.value = null
    hoverInfo.value = null
    hoverLoading.value = false
  }, 150)
}

function onPopoverEnter() {
  if (hoverHideTimer) {
    window.clearTimeout(hoverHideTimer)
    hoverHideTimer = undefined
  }
}

// Element ref + ResizeObserver used to keep the popover within the viewport
// even after the lazily-loaded image grows it past our initial size guess.
let popoverObserver: ResizeObserver | null = null

function bindPopoverEl(el: unknown) {
  if (popoverObserver) {
    popoverObserver.disconnect()
    popoverObserver = null
  }
  if (el instanceof HTMLElement) {
    popoverObserver = new ResizeObserver(() => clampPopoverPosition(el))
    popoverObserver.observe(el)
    clampPopoverPosition(el)
  }
}

// Clamp the popover so its bottom edge never extends below the viewport;
// if it would, slide it up so it sits at the bottom (with an 8 px margin).
// Top edge gets the same treatment in case the trigger sits very low and
// the popover is taller than the screen.
function clampPopoverPosition(el: HTMLElement) {
  const pos = hoverPos.value
  if (!pos) return
  const margin = 8
  const rect = el.getBoundingClientRect()
  let top = pos.top
  const maxTop = window.innerHeight - rect.height - margin
  if (top > maxTop) top = Math.max(margin, maxTop)
  if (top < margin) top = margin
  if (top !== pos.top) {
    hoverPos.value = { ...pos, top }
  }
}

// Modal bird-info state. Loaded after the metadata fetch in openInfo.
const modalBirdInfo = ref<BirdInfo | null>(null)
const modalBirdLoading = ref(false)
const modalBirdImageIdx = ref(0)
const modalBirdImages = computed<BirdInfoImage[]>(() => {
  const i = modalBirdInfo.value
  if (!i) return []
  const out: BirdInfoImage[] = []
  if (i.male_image) out.push({ name: 'Male', source: i.male_image })
  if (i.female_image) out.push({ name: 'Female', source: i.female_image })
  for (const img of i.other_images ?? []) out.push(img)
  return out
})

function modalBirdPrev() {
  if (modalBirdImages.value.length === 0) return
  modalBirdImageIdx.value =
    (modalBirdImageIdx.value - 1 + modalBirdImages.value.length) % modalBirdImages.value.length
}
function modalBirdNext() {
  if (modalBirdImages.value.length === 0) return
  modalBirdImageIdx.value = (modalBirdImageIdx.value + 1) % modalBirdImages.value.length
}

// Bulk-reclassify state. Walks every picture that hasn't been manually
// reclassified (reclassified_at is set only by the user's per-picture
// reclassify button) and re-runs the detector + classifier pipeline on
// each, persisting metadata in the process. Manual reclassifications
// are kept untouched so the user's curation isn't overwritten.
const classifyOpen = ref(false)
const classifyState = ref<'idle' | 'running' | 'done' | 'cancelled'>('idle')
const classifyTotal = ref(0)
const classifyDone = ref(0)
const classifyHits = ref(0)
const classifyEmpty = ref(0)
const classifySkipped = ref(0)
const classifyErrors = ref(0)
const classifyCurrentNonBird = ref(false)
const classifyCurrent = ref<Picture | null>(null)
const classifyCropIdx = ref(-1) // current crop being shown on the right pane (-1 = no crop yet)
const classifyCropTotal = ref(0)
let classifyAbort = false

// Date-range filter on the bulk reclassify dialog. Empty string =
// open-ended on that side. Uses YYYY-MM-DD strings from the native
// <input type="date"> picker.
const classifyFromDate = ref('')
const classifyToDate = ref('')

// Bulk reclassify is "start over from scratch": every picture in the
// date range gets re-run through the bird pipeline regardless of
// whether it's been processed before. Date range is optional; empty
// inputs include all pictures.
const classifyTargets = computed<Picture[]>(() => {
  const from = classifyFromDate.value
  const to = classifyToDate.value
  if (!from && !to) return pictures.value
  // Build inclusive bounds. mod_time arrives as ISO; compare its
  // YYYY-MM-DD prefix lexicographically (matches the date-picker
  // value format and avoids timezone gymnastics).
  return pictures.value.filter((p) => {
    const day = (p.mod_time ?? '').slice(0, 10)
    if (!day) return false
    if (from && day < from) return false
    if (to && day > to) return false
    return true
  })
})

// MIN_CROP_VIEW_MS pacing each crop preview so a human can see what
// the pipeline picked. Configurable here; bumped slightly above the
// user's stated minimum.
const MIN_CROP_VIEW_MS = 280

function sleep(ms: number) {
  return new Promise<void>((r) => window.setTimeout(r, ms))
}
const classifyPercent = computed(() => {
  if (classifyTotal.value === 0) return 0
  return Math.round((classifyDone.value / classifyTotal.value) * 100)
})

function openClassifyAll() {
  classifyOpen.value = true
  classifyState.value = 'idle'
  classifyTotal.value = 0
  classifyDone.value = 0
  classifyHits.value = 0
  classifyEmpty.value = 0
  classifySkipped.value = 0
  classifyErrors.value = 0
  classifyCurrent.value = null
  classifyCurrentNonBird.value = false
  classifyCropIdx.value = -1
  classifyCropTotal.value = 0
}

async function startClassifyAll() {
  classifyAbort = false
  classifyState.value = 'running'
  // Snapshot now — captureSignal could mutate pictures.value during the loop.
  const targets = [...classifyTargets.value]
  classifyTotal.value = targets.length
  classifyDone.value = 0
  classifyHits.value = 0
  classifyEmpty.value = 0
  classifySkipped.value = 0
  classifyErrors.value = 0
  for (const p of targets) {
    if (classifyAbort) break
    classifyCurrent.value = p
    classifyCurrentNonBird.value = false
    classifyCropIdx.value = -1
    classifyCropTotal.value = 0
    try {
      const r = await api.reclassifyPicture(p.name, { destructive: true })
      const dets = r.detections ?? []
      const cropCount = r.crop_count ?? 0
      classifyCropTotal.value = cropCount
      classifyCurrentNonBird.value = !!r.non_bird
      if (r.non_bird) classifySkipped.value++
      else if (cropCount > 0) classifyHits.value++
      else classifyEmpty.value++
      // Update local picture entry with whatever the server now has,
      // including the fresh bird_crops list from a refetched metadata.
      const idx = pictures.value.findIndex((x) => x.name === p.name)
      if (idx >= 0) {
        const recs = dets
          .filter((d) => d.box)
          .map((d) => ({ name: d.name, confidence: d.confidence, box: d.box! }))
        let freshCrops: typeof pictures.value[number]['bird_crops']
        try {
          const md = await api.pictureMetadata(p.name)
          freshCrops = md.bird_crops
        } catch {
          /* leave undefined; the next page reload picks it up */
        }
        pictures.value[idx] = {
          ...pictures.value[idx],
          detections: recs,
          bird_species: r.bird_species,
          bird_crops: freshCrops,
          analyzed_at: new Date().toISOString(),
          reclassified_at: r.reclassified_at,
          has_crop: !!r.has_crop,
        }
      }
      // Walk through each saved crop on the right pane so the user
      // can see which crops the pipeline kept. Minimum dwell time
      // per crop (MIN_CROP_VIEW_MS) so it's visible to a human.
      for (let i = 0; i < cropCount; i++) {
        if (classifyAbort) break
        classifyCropIdx.value = i
        await sleep(MIN_CROP_VIEW_MS)
      }
      // For pictures with no crops, still show the "empty" state for
      // a moment so the loop has a visible cadence rather than blowing
      // through them invisibly.
      if (cropCount === 0) {
        await sleep(MIN_CROP_VIEW_MS)
      }
    } catch {
      classifyErrors.value++
      await sleep(MIN_CROP_VIEW_MS)
    }
    classifyDone.value++
  }
  classifyState.value = classifyAbort ? 'cancelled' : 'done'
  classifyCurrent.value = null
  classifyCropIdx.value = -1
}

function cancelClassify() {
  classifyAbort = true
}

function closeClassify() {
  if (classifyState.value === 'running') {
    classifyAbort = true
  }
  classifyOpen.value = false
}

// AI image-quality batch scan. Same shape as the classify-unclassified
// modal: a target list, a per-iteration "show source + result, brief
// pause, advance" loop, and final tallies.
const qualityOpen = ref(false)
const qualityState = ref<'idle' | 'running' | 'done' | 'cancelled'>('idle')
const qualityTotal = ref(0)
const qualityDone = ref(0)
const qualityScored = ref(0)
const qualityDeleted = ref(0)
const qualityErrors = ref(0)
const qualityCurrent = ref<Picture | null>(null)
const qualityCurrentScore = ref<number | null>(null)
const qualityCurrentThreshold = ref<number | null>(null)
const qualityCurrentDeleted = ref(false)
const qualityCurrentError = ref('')
const qualityAutoDelete = ref(false)
const qualityIncludeAnalyzed = ref(false)
let qualityAbort = false

// Only pictures with a saved bird-classifier crop are eligible for the
// AI quality scan — that's the close-up the AI actually evaluates, and it
// also doubles as the "is this a bird?" filter.
const qualityTargets = computed<Picture[]>(() => {
  const birds = pictures.value.filter((p) => p.has_crop)
  if (qualityIncludeAnalyzed.value) return birds
  return birds.filter((p) => typeof p.ai_quality_score !== 'number')
})
const qualityPercent = computed(() => {
  if (qualityTotal.value === 0) return 0
  return Math.round((qualityDone.value / qualityTotal.value) * 100)
})

function openQualityScan() {
  qualityOpen.value = true
  qualityState.value = 'idle'
  qualityTotal.value = 0
  qualityDone.value = 0
  qualityScored.value = 0
  qualityDeleted.value = 0
  qualityErrors.value = 0
  qualityCurrent.value = null
  qualityCurrentScore.value = null
  qualityCurrentThreshold.value = null
  qualityCurrentDeleted.value = false
  qualityCurrentError.value = ''
}

async function startQualityScan() {
  qualityAbort = false
  qualityState.value = 'running'
  // Snapshot now — captureSignal could mutate pictures.value during the loop.
  const targets = [...qualityTargets.value]
  qualityTotal.value = targets.length
  qualityDone.value = 0
  qualityScored.value = 0
  qualityDeleted.value = 0
  qualityErrors.value = 0
  for (const p of targets) {
    if (qualityAbort) break
    qualityCurrent.value = p
    qualityCurrentScore.value = null
    qualityCurrentThreshold.value = null
    qualityCurrentDeleted.value = false
    qualityCurrentError.value = ''
    try {
      const r = await api.scorePictureQuality(p.name, qualityAutoDelete.value)
      if (!r.enabled) {
        // Misconfigured — surface and bail out of the whole loop.
        qualityCurrentError.value = r.error ?? 'AI quality scoring is disabled'
        qualityErrors.value++
        qualityDone.value++
        qualityState.value = 'cancelled'
        return
      }
      if (r.error) {
        qualityErrors.value++
        qualityCurrentError.value = r.error
      } else {
        qualityScored.value++
        if (typeof r.score === 'number') qualityCurrentScore.value = r.score
        if (typeof r.threshold === 'number') qualityCurrentThreshold.value = r.threshold
        if (r.deleted) {
          qualityDeleted.value++
          qualityCurrentDeleted.value = true
          // Drop from the local list so it disappears from the gallery.
          const idx = pictures.value.findIndex((x) => x.name === p.name)
          if (idx >= 0) pictures.value.splice(idx, 1)
          if (viewing.value?.name === p.name) viewing.value = null
        } else {
          // Patch score onto the in-memory picture so the badge updates.
          const idx = pictures.value.findIndex((x) => x.name === p.name)
          if (idx >= 0 && typeof r.score === 'number') {
            pictures.value[idx] = {
              ...pictures.value[idx],
              ai_quality_score: r.score,
              ai_quality_at: new Date().toISOString(),
            }
          }
        }
      }
    } catch (e: any) {
      qualityErrors.value++
      qualityCurrentError.value = e?.message ?? 'request failed'
    }
    qualityDone.value++
    // Brief pause so the user can read the result before we advance.
    await new Promise((res) => setTimeout(res, 800))
  }
  if (qualityState.value === 'running') {
    qualityState.value = qualityAbort ? 'cancelled' : 'done'
  }
}

function cancelQualityScan() {
  qualityAbort = true
}

function closeQualityScan() {
  if (qualityState.value === 'running') {
    qualityAbort = true
  }
  qualityOpen.value = false
}

function pictureHasInfo(p: Picture): boolean {
  return (
    (p.detections && p.detections.length > 0) ||
    (p.bird_species && p.bird_species.length > 0) ||
    (p.bird_crops && p.bird_crops.length > 0) ||
    !!p.user_species ||
    !!p.user_notes ||
    !!p.has_crop
  )
}

// Substring match against filename (which carries date + YOLO species),
// the localized timestamp string, the YOLO species token, and every
// candidate fine-grained bird species. Case-insensitive; empty query
// returns everything.
const filteredPictures = computed<Picture[]>(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return pictures.value
  return pictures.value.filter((p) => {
    if (p.name.toLowerCase().includes(q)) return true
    if (p.species && p.species.toLowerCase().includes(q)) return true
    if (formatTs(p).toLowerCase().includes(q)) return true
    if (p.bird_species?.some((s) => s.name.toLowerCase().includes(q))) return true
    if (
      p.bird_crops?.some((c) =>
        c.species?.some((s) => s.name.toLowerCase().includes(q)),
      )
    ) {
      return true
    }
    if (p.user_species && p.user_species.toLowerCase().includes(q)) return true
    return false
  })
})

// Reference to the scrolling <aside> so we can preserve position on refresh.
const thumbScroll = ref<HTMLElement | null>(null)

async function load() {
  if (loading.value) return
  loading.value = true
  err.value = ''
  // Snapshot scroll before the list reactively changes so we can restore it.
  const scroller = thumbScroll.value
  const prevScrollTop = scroller?.scrollTop ?? 0
  const prevScrollHeight = scroller?.scrollHeight ?? 0
  const hadList = pictures.value.length > 0
  try {
    const next = await api.pictures()
    pictures.value = next
    // If whatever we were viewing has been deleted elsewhere, clear it.
    if (viewing.value && !next.some((p) => p.name === viewing.value!.name)) {
      viewing.value = null
    }
    // First successful load with no selection yet → preselect the newest.
    if (!hadList && viewing.value == null && next.length > 0) {
      viewing.value = next[0]
    }
  } catch (e: any) {
    err.value = e?.message ?? 'load failed'
  } finally {
    loading.value = false
    // After Vue flushes the DOM update, adjust scrollTop so that the content
    // the user was looking at stays fixed in the viewport.
    await nextTick()
    const el = thumbScroll.value
    if (el) {
      const delta = el.scrollHeight - prevScrollHeight
      // Only shift when the user was already scrolled into the list; if they
      // were at the very top, let the new (newer) items come into view.
      if (delta > 0 && prevScrollTop > 0) {
        el.scrollTop = prevScrollTop + delta
      } else {
        el.scrollTop = prevScrollTop
      }
    }
  }
}

async function remove(p: Picture) {
  if (!confirm(`Delete ${p.name}?`)) return
  try {
    await api.deletePicture(p.name)
  } catch (e: any) {
    err.value = e?.message ?? 'delete failed'
    return
  }
  if (viewing.value?.name === p.name) viewing.value = null
  await load()
}

async function toggleHeart(p: Picture, e: Event) {
  e.stopPropagation()
  try {
    const r = await api.heartPicture(p.name)
    p.hearted = r.hearted
    if (viewing.value?.name === p.name) {
      viewing.value = { ...viewing.value, hearted: r.hearted }
    }
  } catch (err: any) {
    console.error('heart toggle failed', err)
  }
}

function formatTs(p: Picture): string {
  return new Date(p.mod_time).toLocaleString()
}

// Per-picture currently-selected crop index for multi-crop bird
// sightings. Single-crop sightings stay at 0 implicitly.
const cropIndex = ref<Map<string, number>>(new Map())

function cropCount(p: Picture | null | undefined): number {
  if (!p || !p.bird_crops) return 0
  return p.bird_crops.length
}

function currentCropIdx(p: Picture | null | undefined): number {
  if (!p) return 0
  const n = cropCount(p)
  if (n <= 1) return 0
  const v = cropIndex.value.get(p.name) ?? 0
  if (v < 0) return 0
  if (v >= n) return n - 1
  return v
}

function cycleCrop(p: Picture, delta: number, e?: Event) {
  e?.stopPropagation()
  const n = cropCount(p)
  if (n <= 1) return
  const cur = currentCropIdx(p)
  let next = (cur + delta) % n
  if (next < 0) next += n
  cropIndex.value.set(p.name, next)
  // Vue won't pick up Map mutations without a reassignment.
  cropIndex.value = new Map(cropIndex.value)
}

function cropImageURL(p: Picture): string {
  if (cropCount(p) > 0) {
    return `/api/pictures/${encodeURIComponent(p.name)}/crops/${currentCropIdx(p)}`
  }
  return p.has_crop
    ? `/api/pictures/${encodeURIComponent(p.name)}/crop`
    : `/api/pictures/${encodeURIComponent(p.name)}?thumb=1`
}

// basicClass returns the high-level animal class shown on the green
// row badge — "bird", "fox", "deer", etc. The species filename token
// produced by the new bird pipeline contains the fine-grained species
// ("chipping-sparrow", "northern-cardinal") which is already shown on
// the species lines above; this helper collapses any hyphenated token
// back to plain "bird" so the badge reads as "what animal" rather
// than "which species". Multi-crop sightings always badge as bird.
function basicClass(p: Picture): string | null {
  if (p.bird_crops && p.bird_crops.length > 0) return 'bird'
  if (!p.species) return null
  if (p.species.includes('-')) return 'bird'
  return p.species
}

function aiQualityScore(p: Picture | null | undefined): number | null {
  if (!p) return null
  // Multi-crop sighting: show the selected crop's score.
  if (cropCount(p) > 0) {
    const c = p.bird_crops![currentCropIdx(p)]
    if (typeof c.ai_score === 'number') return c.ai_score
  }
  if (typeof p.ai_quality_score !== 'number') return null
  return p.ai_quality_score
}

// aiQualityColor returns a CSS color for a 0–100 score: red below 40,
// amber 40–70, green 70+. Used for both the modal bar and the row badge.
function aiQualityColor(score: number): string {
  if (score < 40) return '#c75454'
  if (score < 70) return '#c79c54'
  return '#54c77f'
}

function topSpecies(p: Picture): string | null {
  // User-confirmed species takes precedence over the classifier guess and
  // is shown without a confidence number — it's an assertion, not a guess.
  const u = p.user_species?.trim()
  if (u) return u
  // Multi-crop sighting: show the currently-selected crop's species.
  if (cropCount(p) > 0) {
    const c = p.bird_crops![currentCropIdx(p)]
    if (c.species && c.species.length > 0) {
      const top = c.species[0]
      return `${top.name} ${(top.confidence * 100).toFixed(0)}%`
    }
  }
  if (!p.bird_species || p.bird_species.length === 0) return null
  const top = p.bird_species[0]
  return `${top.name} ${(top.confidence * 100).toFixed(0)}%`
}

async function openInfo(p: Picture, ev: Event) {
  ev.stopPropagation()
  infoFor.value = p
  infoMd.value = null
  infoErr.value = ''
  infoLoading.value = true
  infoTab.value = 'analysis'
  formSpecies.value = p.user_species ?? ''
  formNotes.value = p.user_notes ?? ''
  // Seed per-crop overrides from the live picture record so cycling
  // works before the metadata round-trip resolves.
  formCropSpecies.value = new Map<number, string>(
    (p.bird_crops ?? []).map((c, i) => [i, c.user_species ?? '']),
  )
  try {
    const md = await api.pictureMetadata(p.name)
    infoMd.value = md
    if (md.user_species != null) formSpecies.value = md.user_species
    if (md.user_notes != null) formNotes.value = md.user_notes
    // Refresh per-crop overrides from authoritative server state.
    if (md.bird_crops) {
      formCropSpecies.value = new Map<number, string>(
        md.bird_crops.map((c, i) => [i, c.user_species ?? '']),
      )
    }
  } catch (e: any) {
    infoErr.value = e?.message ?? 'load failed'
  } finally {
    infoLoading.value = false
  }
  // Reset + lazy-load Ornithophile data for the new modal. Use the
  // user-typed override (if any) before falling back to whatever species
  // the metadata sidecar holds.
  modalBirdInfo.value = null
  modalBirdImageIdx.value = 0
  const fromForm: Picture = {
    ...p,
    user_species: formSpecies.value || p.user_species,
    bird_species: infoMd.value?.bird_species ?? p.bird_species,
  }
  const speciesName = lookupSpeciesFor(fromForm)
  if (speciesName) {
    modalBirdLoading.value = true
    fetchBirdInfo(speciesName).then((info) => {
      modalBirdInfo.value = info
      modalBirdLoading.value = false
    })
  }
}

function openPip() { pip.value = true }
function closePip() { pip.value = false }

// Two-way binding for the per-crop "Confirmed species" input. The
// active row in formCropSpecies tracks `currentCropIdx(infoFor)` so
// cycling crops in the modal swaps the field's value to the new
// crop's override, and typing writes back to that crop only.
const currentCropFormSpecies = computed<string>({
  get() {
    const p = infoFor.value
    if (!p) return ''
    const idx = currentCropIdx(p)
    return formCropSpecies.value.get(idx) ?? ''
  },
  set(v: string) {
    const p = infoFor.value
    if (!p) return
    const idx = currentCropIdx(p)
    const m = new Map(formCropSpecies.value)
    m.set(idx, v)
    formCropSpecies.value = m
  },
})

// currentModalSpeciesName picks the species name driving the modal's
// Ornithophile section: user override > selected crop's top species
// (multi-crop) > legacy bird_species[0]. Reactive — changes when the
// user cycles crops in the header so the right info loads.
const currentModalSpeciesName = computed<string | null>(() => {
  const p = infoFor.value
  if (!p) return null
  // Multi-crop: per-crop human override (form has unsaved precedence
  // over saved row state) > classifier guess for that crop.
  if (cropCount(p) > 0 && p.bird_crops) {
    const idx = currentCropIdx(p)
    const formCu = formCropSpecies.value.get(idx)?.trim()
    if (formCu) return formCu
    const c = p.bird_crops[idx]
    const cu = c.user_species?.trim()
    if (cu) return cu
    const n = c.species?.[0]?.name?.trim()
    if (n) return n
  }
  // Legacy / single-crop path.
  const u = (formSpecies.value || p.user_species || '').trim()
  if (u) return u
  const md = infoMd.value?.bird_species ?? p.bird_species
  const top = md?.[0]?.name?.trim()
  return top || null
})

// Refetch Ornithophile when the modal's effective species changes —
// e.g. cycling the crop selector to a different bird in a multi-crop
// sighting, or typing a user-confirmed species in the form.
watch(currentModalSpeciesName, (name, prev) => {
  if (!infoFor.value) return
  if (!name) {
    modalBirdInfo.value = null
    modalBirdLoading.value = false
    return
  }
  if (name === prev) return
  modalBirdInfo.value = null
  modalBirdImageIdx.value = 0
  modalBirdLoading.value = true
  fetchBirdInfo(name).then((info) => {
    if (currentModalSpeciesName.value !== name) return
    modalBirdInfo.value = info
    modalBirdLoading.value = false
  })
})

function closeInfo() {
  infoFor.value = null
  infoMd.value = null
  infoErr.value = ''
  modalBirdInfo.value = null
  modalBirdLoading.value = false
  modalBirdImageIdx.value = 0
}

async function saveInfo() {
  if (!infoFor.value) return
  infoSaving.value = true
  infoErr.value = ''
  try {
    const p = infoFor.value
    const patch: Parameters<typeof api.savePictureMetadata>[1] = {
      user_notes: formNotes.value,
    }
    if (cropCount(p) > 0) {
      // Multi-crop sighting: send the per-crop user-species map.
      // Picture-level user_species stays untouched on multi-crop.
      const cropMap: Record<string, string> = {}
      for (const [idx, val] of formCropSpecies.value.entries()) {
        cropMap[String(idx)] = val
      }
      patch.crop_user_species = cropMap
    } else {
      // Legacy / single-crop path.
      patch.user_species = formSpecies.value
    }
    const md = await api.savePictureMetadata(p.name, patch)
    infoMd.value = md
    // Patch the picture in the list so the row reflects the edit
    // without a full refresh.
    const idx = pictures.value.findIndex((x) => x.name === p.name)
    if (idx >= 0) {
      pictures.value[idx] = {
        ...pictures.value[idx],
        user_species: md.user_species,
        user_notes: md.user_notes,
        bird_crops: md.bird_crops ?? pictures.value[idx].bird_crops,
      }
      if (viewing.value?.name === p.name) {
        viewing.value = pictures.value[idx]
      }
      // Also reflect the saved crop user-species values back into
      // the form's local state, so subsequent edits start from the
      // server's view.
      if (md.bird_crops) {
        formCropSpecies.value = new Map<number, string>(
          md.bird_crops.map((c, i) => [i, c.user_species ?? '']),
        )
      }
    }
  } catch (e: any) {
    infoErr.value = e?.message ?? 'save failed'
  } finally {
    infoSaving.value = false
  }
}

async function reclassify(p: Picture, ev: Event) {
  ev.stopPropagation()
  if (reclassifying.value.has(p.name)) return
  reclassifying.value = new Set([...reclassifying.value, p.name])
  err.value = ''
  try {
    const r = await api.reclassifyPicture(p.name)
    // Patch the picture in place so the new species shows up without a full
    // refresh (which would flicker the scroll position).
    const idx = pictures.value.findIndex((x) => x.name === p.name)
    if (idx >= 0) {
      pictures.value[idx] = {
        ...pictures.value[idx],
        bird_species: r.bird_species,
        reclassified_at: r.reclassified_at,
        ai_quality_score:
          typeof r.quality_score === 'number'
            ? r.quality_score
            : pictures.value[idx].ai_quality_score,
        ai_quality_at:
          typeof r.quality_score === 'number'
            ? new Date().toISOString()
            : pictures.value[idx].ai_quality_at,
      }
      if (viewing.value?.name === p.name) {
        viewing.value = pictures.value[idx]
      }
    }
    // Decide whether to prompt the user to delete. Two independent
    // signals; whichever is more decisive forms the prompt message:
    //   1. No bird detection at all on reanalysis (e.g. a leaf).
    //   2. AI quality score below the user's discard threshold.
    // Non-bird sightings (fox/deer/cat/dog/person) explicitly skip
    // both prompts — the backend marks the response with `non_bird`
    // and we keep these for security / interest, no AI scoring, no
    // delete prompt. Either prompt fires at most once per reclassify;
    // "no bird" takes precedence because it's the more decisive
    // failure for a presumed-bird picture.
    if (r.quality_error) {
      console.warn('AI quality scoring failed:', r.quality_error)
    }
    let promptMsg: string | null = null
    if (!r.non_bird) {
      // The new bird pipeline reports detected birds via `crop_count`
      // (each surviving crop is one bird). Legacy responses fall back
      // to `detections` containing a "Bird" entry. Either signal
      // counts as "bird present."
      const dets = r.detections ?? []
      const hasBird =
        (r.crop_count ?? 0) > 0 ||
        !!r.has_crop ||
        dets.some((d) => {
          const norm = (d.name ?? '').toLowerCase().replace(/[^a-z0-9]/g, '')
          return norm === 'bird'
        })
      if (!hasBird) {
        promptMsg =
          `Reanalysis didn't detect a bird in this image — ` +
          `the original detection may have been a false positive.\n\n` +
          `Delete from gallery?`
      } else if (
        r.quality_enabled &&
        typeof r.quality_score === 'number' &&
        typeof r.quality_threshold === 'number' &&
        r.quality_score < r.quality_threshold
      ) {
        promptMsg =
          `AI rated this picture ${r.quality_score} / 100 ` +
          `(below your discard threshold of ${r.quality_threshold}).\n\n` +
          `Delete from gallery?`
      }
    }
    if (promptMsg && confirm(promptMsg)) {
      try {
        await api.deletePicture(p.name)
        if (viewing.value?.name === p.name) viewing.value = null
        await load()
      } catch (e: any) {
        err.value = e?.message ?? 'delete failed'
      }
    }
  } catch (e: any) {
    err.value = e?.message ?? 'reclassify failed'
  } finally {
    const next = new Set(reclassifying.value)
    next.delete(p.name)
    reclassifying.value = next
  }
}

onMounted(() => {
  load()
  window.addEventListener('keydown', onKeydown)
})
// Refresh whenever Live signals that a capture happened (manual or auto).
watch(captureSignal, () => load())

// Global ←/→ to cycle bird crops in a multi-crop sighting. Skip when
// the user is typing in an input/textarea or when a modal is open.
function onKeydown(e: KeyboardEvent) {
  const isArrow =
    e.key === 'ArrowLeft' ||
    e.key === 'ArrowRight' ||
    e.key === 'ArrowUp' ||
    e.key === 'ArrowDown'
  if (!isArrow) return
  const t = e.target as HTMLElement | null
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
    return
  }
  if (pip.value) return

  // ←/→: cycle bird crops within the active sighting. Modal cycling
  // takes precedence when the info modal is open; otherwise the
  // right-pane viewer.
  if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
    const target = infoFor.value ?? viewing.value
    if (!target || cropCount(target) <= 1) return
    e.preventDefault()
    cycleCrop(target, e.key === 'ArrowLeft' ? -1 : +1)
    return
  }

  // ↑/↓: navigate the gallery list. Disabled while the modal is
  // open (those keys would be confusing alongside the modal's
  // species form). Wraps at both ends for fast skimming.
  if (infoFor.value) return
  const list = filteredPictures.value
  if (list.length === 0) return
  const cur = viewing.value
    ? list.findIndex((p) => p.name === viewing.value!.name)
    : -1
  let next: number
  if (e.key === 'ArrowDown') {
    next = cur < 0 ? 0 : (cur + 1) % list.length
  } else {
    next = cur < 0 ? list.length - 1 : (cur - 1 + list.length) % list.length
  }
  e.preventDefault()
  viewing.value = list[next]
  // Scroll the new row into view if it's off-screen.
  const scroller = thumbScroll.value
  if (scroller) {
    nextTick(() => {
      const rows = scroller.querySelectorAll<HTMLElement>('.thumb-row')
      rows.item(next)?.scrollIntoView({ block: 'nearest' })
    })
  }
}
</script>

<template>
  <div class="gallery-layout">
    <aside ref="thumbScroll" class="thumb-column">
      <div class="search-bar">
        <input
          v-model="search"
          type="search"
          class="search-input"
          placeholder="Filter by date, animal, or species…"
          aria-label="Filter gallery"
        />
        <button
          v-if="search"
          type="button"
          class="search-clear"
          aria-label="Clear filter"
          title="Clear filter"
          @click="search = ''"
        >×</button>
      </div>
      <div class="toolbar-row">
        <button
          type="button"
          class="secondary toolbar-btn"
          title="Run the full bird pipeline (per-frame YOLO + classifier + AI quality, multi-crop) on every picture that hasn't been processed by it yet"
          @click="openClassifyAll"
        >
          Reclassify
          <span v-if="classifyTargets.length > 0" class="toolbar-badge">{{ classifyTargets.length }}</span>
        </button>
        <button
          type="button"
          class="secondary toolbar-btn"
          :title="'Send pictures without an AI quality score to the configured AI endpoint and store the result.'"
          @click="openQualityScan"
        >
          AI quality scan
          <span v-if="qualityTargets.length > 0" class="toolbar-badge">{{ qualityTargets.length }}</span>
        </button>
      </div>
      <div v-if="err" class="error" style="padding: 0.5rem;">{{ err }}</div>
      <div v-if="pictures.length === 0 && !loading" class="hint" style="padding: 0.8rem;">
        No pictures yet.
      </div>
      <div
        v-else-if="filteredPictures.length === 0 && !loading"
        class="hint"
        style="padding: 0.8rem;"
      >
        No matches for "{{ search }}".
      </div>
      <div
        v-for="p in filteredPictures"
        :key="p.name"
        class="thumb-row"
        :class="{ active: viewing?.name === p.name }"
        role="button"
        tabindex="0"
        @click="viewing = p"
        @keydown.enter.prevent="viewing = p"
        @keydown.space.prevent="viewing = p"
      >
        <img
          :src="cropImageURL(p)"
          :alt="p.name"
          loading="lazy"
          decoding="async"
        />
        <div v-if="cropCount(p) > 1" class="multi-crop-badge">
          <button
            type="button"
            class="crop-arrow"
            title="Previous bird in this sighting"
            @click.stop="cycleCrop(p, -1, $event)"
          >‹</button>
          <span>{{ currentCropIdx(p) + 1 }} / {{ cropCount(p) }}</span>
          <button
            type="button"
            class="crop-arrow"
            title="Next bird in this sighting"
            @click.stop="cycleCrop(p, +1, $event)"
          >›</button>
        </div>
        <div class="thumb-meta">
          <div class="thumb-birds">
            <div
              v-for="(b, i) in birdsForRow(p)"
              :key="i"
              class="thumb-bird has-info"
              :class="{ 'thumb-bird-current': cropCount(p) > 1 && i === currentCropIdx(p) }"
              @mouseenter="onBirdNameHover(p.name + ':' + i, b.name, $event)"
              @mouseleave="onSpeciesLeave"
            >{{ b.display }}</div>
          </div>
          <div class="thumb-when">{{ formatTs(p) }}</div>
          <div class="thumb-sub">
            <span v-if="basicClass(p)" class="species-tag">{{ basicClass(p) }}</span>
            <span v-else-if="p.manual" class="manual-tag">manual</span>
            <span
              v-if="typeof aiQualityScore(p) === 'number'"
              class="ai-quality-tag"
              :style="{ background: aiQualityColor(aiQualityScore(p)!) }"
              :title="`AI quality score ${aiQualityScore(p)} / 100`"
            >
              AI {{ aiQualityScore(p) }}
            </span>
            <span
              v-else-if="p.ai_quality_error"
              class="ai-quality-tag ai-failed"
              :title="`AI scoring failed: ${p.ai_quality_error}`"
            >
              AI ⚠
            </span>
          </div>
        </div>
        <button
          type="button"
          class="reclassify-btn"
          :class="{ working: reclassifying.has(p.name) }"
          :disabled="reclassifying.has(p.name)"
          :title="reclassifying.has(p.name) ? 'Reclassifying…' : 'Reclassify this image'"
          @click="reclassify(p, $event)"
        >
          <span v-if="reclassifying.has(p.name)" class="spinner" aria-hidden="true" />
          <svg v-else viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
            <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
              d="M21 12a9 9 0 1 1-3.07-6.78L21 8M21 3v5h-5" />
          </svg>
        </button>
        <button
          v-if="pictureHasInfo(p)"
          type="button"
          class="info-btn"
          title="View / edit detection data"
          @click="openInfo(p, $event)"
        >
          <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
            <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="2" />
            <line x1="12" y1="11" x2="12" y2="16" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
            <circle cx="12" cy="8" r="1" fill="currentColor" />
          </svg>
        </button>
        <button
          type="button"
          class="heart-btn"
          :class="{ hearted: p.hearted }"
          :title="p.hearted ? 'Unheart (allow auto-deletion)' : 'Heart this picture (keep forever)'"
          @click.stop="toggleHeart(p, $event)"
          @keydown.enter.stop
          @keydown.space.stop
        >
          <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
            <path
              :fill="p.hearted ? 'currentColor' : 'none'"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"
            />
          </svg>
        </button>
        <button
          type="button"
          class="delete-btn"
          title="Delete this picture"
          @click.stop="remove(p)"
          @keydown.enter.stop
          @keydown.space.stop
        >
          <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
            <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
              d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m5 5v6m4-6v6" />
          </svg>
        </button>
      </div>
    </aside>
    <main class="preview-pane">
      <div v-if="viewing" class="preview-wrap">
        <div class="preview-stage">
          <img
            class="preview-img"
            :src="`/api/pictures/${encodeURIComponent(viewing.name)}`"
            :alt="viewing.name"
          />
          <img
            v-if="cropCount(viewing) > 0"
            class="preview-pip"
            :src="cropImageURL(viewing)"
            :alt="`Bird crop ${currentCropIdx(viewing) + 1}/${cropCount(viewing)} for ${viewing.name}`"
            title="The bird crop sent to the species classifier"
          />
          <img
            v-else-if="viewing.has_crop"
            class="preview-pip"
            :src="`/api/pictures/${encodeURIComponent(viewing.name)}/crop`"
            :alt="`Bird-classifier crop for ${viewing.name}`"
            title="The sub-image sent to the bird classifier"
          />
          <div v-if="cropCount(viewing) > 1" class="preview-crop-cycle">
            <button
              type="button"
              class="crop-arrow"
              title="Previous bird"
              @click.stop="cycleCrop(viewing, -1, $event)"
            >‹</button>
            <span>{{ currentCropIdx(viewing) + 1 }} / {{ cropCount(viewing) }}</span>
            <button
              type="button"
              class="crop-arrow"
              title="Next bird"
              @click.stop="cycleCrop(viewing, +1, $event)"
            >›</button>
          </div>
        </div>
        <div class="preview-footer">
          <div class="preview-meta">
            <div class="preview-name">{{ viewing.name }}</div>
            <div class="preview-sub">{{ formatTs(viewing) }}</div>
          </div>
          <div class="spacer" style="flex:1" />
          <a :href="`/api/pictures/${encodeURIComponent(viewing.name)}?download=1`">Download</a>
          <button class="secondary" @click="remove(viewing)">Delete</button>
        </div>
      </div>
      <div v-else class="preview-empty">
        <div class="hint">Select a picture from the list</div>
      </div>
    </main>

    <div
      v-if="infoFor && !pip"
      class="info-modal-backdrop"
      role="dialog"
      aria-modal="true"
      @click.self="closeInfo"
      @keydown.esc="closeInfo"
      tabindex="-1"
    >
      <div class="info-modal">
        <header class="info-header">
          <h3>{{ infoFor.name }}</h3>
          <div v-if="cropCount(infoFor) > 1" class="info-crop-cycle">
            <button
              type="button"
              class="crop-arrow"
              title="Previous bird (←)"
              @click="cycleCrop(infoFor, -1, $event)"
            >‹</button>
            <span>Bird {{ currentCropIdx(infoFor) + 1 }} / {{ cropCount(infoFor) }}</span>
            <button
              type="button"
              class="crop-arrow"
              title="Next bird (→)"
              @click="cycleCrop(infoFor, +1, $event)"
            >›</button>
          </div>
          <button type="button" class="tag-x" @click="closeInfo" aria-label="Close">×</button>
        </header>
        <div class="info-body">
          <div class="info-left-column">
            <div class="info-image-wrap">
              <div class="info-image-frame">
                <img
                  class="info-image"
                  :src="`/api/pictures/${encodeURIComponent(infoFor.name)}`"
                  :alt="infoFor.name"
                />
                <div class="info-bbox-layer">
                  <template v-for="(d, i) in modalDetections" :key="i">
                    <div
                      class="bbox"
                      :style="{
                        left: (d.box.x1 * 100) + '%',
                        top: (d.box.y1 * 100) + '%',
                        width: ((d.box.x2 - d.box.x1) * 100) + '%',
                        height: ((d.box.y2 - d.box.y1) * 100) + '%',
                      }"
                    >
                      <span class="bbox-label">{{ d.name || '?' }} {{ (d.confidence * 100).toFixed(0) }}%</span>
                    </div>
                  </template>
                </div>
              </div>
            </div>

            <section v-if="modalBirdLoading || modalBirdInfo" class="info-bird-section">
              <div v-if="modalBirdLoading && !modalBirdInfo" class="hint">Loading species info…</div>
              <template v-else-if="modalBirdInfo">
                <div class="bird-section-text">
                  <div class="bird-section-head">
                    <div class="bird-section-title">{{ modalBirdInfo.common_name }}</div>
                    <div v-if="modalBirdInfo.scientific_name" class="bird-section-sci">
                      <em>{{ modalBirdInfo.scientific_name }}</em>
                    </div>
                  </div>
                  <div class="bird-section-meta">
                    <span v-if="modalBirdInfo.family">Family: {{ modalBirdInfo.family }}</span>
                    <span v-if="modalBirdInfo.genus">Genus: {{ modalBirdInfo.genus }}</span>
                    <span v-if="modalBirdInfo.conservation_status" class="bird-status-tag">
                      {{ modalBirdInfo.conservation_status }}
                    </span>
                    <a v-if="modalBirdInfo.source" :href="modalBirdInfo.source" target="_blank" rel="noopener">
                      Wikipedia ↗
                    </a>
                    <a v-if="modalBirdInfo.sound" :href="modalBirdInfo.sound" target="_blank" rel="noopener">
                      Sound ↗
                    </a>
                  </div>
                  <p v-if="modalBirdInfo.description" class="bird-section-desc">
                    {{ modalBirdInfo.description }}
                  </p>
                </div>
                <div v-if="modalBirdImages.length > 0" class="bird-section-gallery">
                  <button
                    type="button"
                    class="bird-gallery-nav"
                    :disabled="modalBirdImages.length < 2"
                    @click="modalBirdPrev"
                    aria-label="Previous image"
                  >‹</button>
                  <div class="bird-gallery-frame">
                    <img
                      :src="modalBirdImages[modalBirdImageIdx].source"
                      :alt="modalBirdImages[modalBirdImageIdx].name || modalBirdInfo.common_name"
                    />
                    <div class="bird-gallery-caption">
                      <span v-if="modalBirdImages[modalBirdImageIdx].name">
                        {{ modalBirdImages[modalBirdImageIdx].name }}
                      </span>
                      <span class="bird-gallery-counter">
                        {{ modalBirdImageIdx + 1 }} / {{ modalBirdImages.length }}
                      </span>
                    </div>
                  </div>
                  <button
                    type="button"
                    class="bird-gallery-nav"
                    :disabled="modalBirdImages.length < 2"
                    @click="modalBirdNext"
                    aria-label="Next image"
                  >›</button>
                </div>
              </template>
            </section>
          </div>
          <aside class="info-side">
            <div class="info-tabs" role="tablist">
              <button
                type="button"
                class="info-tab"
                :class="{ active: infoTab === 'analysis' }"
                role="tab"
                :aria-selected="infoTab === 'analysis'"
                @click="infoTab = 'analysis'"
              >Analysis</button>
              <button
                type="button"
                class="info-tab"
                :class="{ active: infoTab === 'debug' }"
                role="tab"
                :aria-selected="infoTab === 'debug'"
                @click="infoTab = 'debug'"
              >Debug</button>
            </div>

            <div v-show="infoTab === 'analysis'">
            <div v-if="infoLoading" class="hint">Loading…</div>
            <div v-if="infoErr" class="error">{{ infoErr }}</div>

            <section v-if="cropCount(infoFor) > 0 || infoFor.has_crop" class="info-section">
              <h4>
                Bird-classifier crop<template v-if="cropCount(infoFor) > 1">
                  — Bird {{ currentCropIdx(infoFor) + 1 }} / {{ cropCount(infoFor) }}</template>
              </h4>
              <img
                class="info-crop"
                :key="`info-crop-${infoFor.name}-${currentCropIdx(infoFor)}`"
                :src="cropImageURL(infoFor)"
                :alt="`Crop ${currentCropIdx(infoFor) + 1} for ${infoFor.name}`"
                title="Click to view fullscreen with picture-in-picture"
                @click="openPip"
              />
            </section>

            <section
              v-if="typeof aiQualityScore(infoFor) === 'number'"
              class="info-section"
            >
              <h4>AI image quality</h4>
              <div class="ai-quality-row">
                <div class="ai-quality-bar">
                  <div
                    class="ai-quality-bar-fill"
                    :style="{
                      width: aiQualityScore(infoFor)! + '%',
                      background: aiQualityColor(aiQualityScore(infoFor)!),
                    }"
                  />
                </div>
                <div class="ai-quality-value">
                  {{ aiQualityScore(infoFor) }} / 100
                </div>
              </div>
              <div v-if="infoFor.ai_quality_at" class="hint" style="margin-top: 0.3rem">
                scored {{ new Date(infoFor.ai_quality_at).toLocaleString() }}
              </div>
            </section>

            <section v-if="modalSpecies.length > 0" class="info-section">
              <h4>Species guesses</h4>
              <ul class="readonly-list">
                <li v-for="(g, i) in modalSpecies" :key="i">
                  <span
                    class="row-name has-info"
                    @mouseenter="onBirdNameHover('modal:' + i, g.name, $event)"
                    @mouseleave="onSpeciesLeave"
                  >{{ g.name }}</span>
                  <span class="row-conf">{{ (g.confidence * 100).toFixed(1) }}%</span>
                </li>
              </ul>
            </section>

            <section v-if="modalDetections.length > 0" class="info-section">
              <h4>YOLO detections</h4>
              <ul class="readonly-list">
                <li v-for="(d, i) in modalDetections" :key="i">
                  <span class="row-name">{{ d.name || '?' }}</span>
                  <span class="row-conf">{{ (d.confidence * 100).toFixed(0) }}%</span>
                </li>
              </ul>
            </section>

            <section class="info-section">
              <h4>Your notes</h4>
              <template v-if="cropCount(infoFor) > 0">
                <label class="form-label">
                  Confirmed species —
                  Bird {{ currentCropIdx(infoFor) + 1 }} / {{ cropCount(infoFor) }}
                </label>
                <input
                  v-model="currentCropFormSpecies"
                  type="text"
                  class="info-input"
                  placeholder="e.g. Northern Cardinal"
                />
                <div class="hint" style="margin-top:0.2rem; font-size:0.78rem">
                  Cycle the bird selector at the top of the modal to label
                  each crop individually.
                </div>
              </template>
              <template v-else>
                <label class="form-label">Confirmed species</label>
                <input
                  v-model="formSpecies"
                  type="text"
                  class="info-input"
                  placeholder="e.g. Northern Cardinal"
                />
              </template>
              <label class="form-label" style="margin-top:0.5rem">Notes</label>
              <textarea
                v-model="formNotes"
                class="info-input info-textarea"
                rows="4"
                placeholder="Anything worth remembering about this capture…"
              ></textarea>
              <div class="info-actions">
                <button @click="saveInfo" :disabled="infoSaving">
                  {{ infoSaving ? 'Saving…' : 'Save' }}
                </button>
                <button class="secondary" @click="closeInfo">Close</button>
              </div>
            </section>

            <section v-if="infoMd?.analyzed_at || infoMd?.reclassified_at" class="info-section info-meta">
              <div v-if="infoMd?.analyzed_at" class="hint">
                analyzed {{ new Date(infoMd.analyzed_at).toLocaleString() }}
              </div>
              <div v-if="infoMd?.reclassified_at" class="hint">
                reclassified {{ new Date(infoMd.reclassified_at).toLocaleString() }}
              </div>
            </section>
            </div>

            <div v-show="infoTab === 'debug'" class="info-debug">
              <section class="info-section">
                <h4>AI quality — raw model response</h4>
                <pre v-if="infoMd?.ai_quality_raw" class="debug-pre"><code>{{ prettyJSON(infoMd.ai_quality_raw) }}</code></pre>
                <div v-else class="hint">No AI response captured yet for this picture.</div>
                <div v-if="infoMd?.ai_quality_error" class="error" style="margin-top:0.4rem">
                  Last scoring error: {{ infoMd.ai_quality_error }}
                </div>
              </section>
              <section class="info-section">
                <h4>Stored metadata</h4>
                <pre class="debug-pre"><code>{{ debugDump() }}</code></pre>
              </section>
            </div>
          </aside>
        </div>
      </div>
    </div>

    <div
      v-if="infoFor && pip"
      class="pip-backdrop"
      role="dialog"
      aria-modal="true"
      @click="closePip"
      @keydown.esc="closePip"
      tabindex="-1"
    >
      <img
        class="pip-main"
        :src="`/api/pictures/${encodeURIComponent(infoFor.name)}`"
        :alt="infoFor.name"
      />
      <img
        v-if="infoFor.has_crop"
        class="pip-crop"
        :src="`/api/pictures/${encodeURIComponent(infoFor.name)}/crop`"
        :alt="`Bird-classifier crop for ${infoFor.name}`"
      />
      <button type="button" class="pip-close" @click.stop="closePip" aria-label="Close fullscreen">×</button>
    </div>

    <div
      v-if="classifyOpen"
      class="info-modal-backdrop"
      role="dialog"
      aria-modal="true"
      @click.self="closeClassify"
      @keydown.esc="closeClassify"
      tabindex="-1"
    >
      <div class="classify-modal">
        <header class="info-header">
          <h3>Reclassify pictures</h3>
          <button type="button" class="tag-x" @click="closeClassify" aria-label="Close">×</button>
        </header>

        <div v-if="classifyState === 'idle'" class="classify-range">
          <label class="classify-range-field">
            <span>From</span>
            <input type="date" v-model="classifyFromDate" />
          </label>
          <label class="classify-range-field">
            <span>To</span>
            <input type="date" v-model="classifyToDate" />
          </label>
          <button
            v-if="classifyFromDate || classifyToDate"
            type="button"
            class="secondary range-clear"
            title="Clear date range"
            @click="classifyFromDate = ''; classifyToDate = ''"
          >Clear</button>
        </div>

        <div class="classify-status">
          <template v-if="classifyState === 'idle'">
            <template v-if="classifyTargets.length === 0">
              No pictures match the selected date range.
            </template>
            <template v-else>
              {{ classifyTargets.length }} pictures will be reclassified
              from scratch. Existing classifications are wiped and the
              full bird pipeline re-runs against each saved frame.
              Surviving crops become the new thumbnails; pictures with
              no detected bird are left blank.
            </template>
          </template>
          <template v-else-if="classifyState === 'running'">
            Processing {{ classifyDone + 1 }} of {{ classifyTotal }} —
            <strong>{{ classifyHits }}</strong> hit,
            <strong>{{ classifyEmpty }}</strong> empty,
            <strong>{{ classifySkipped }}</strong> non-bird,
            <strong>{{ classifyErrors }}</strong> failed.
          </template>
          <template v-else-if="classifyState === 'done'">
            Done. {{ classifyHits }} hit, {{ classifyEmpty }} empty,
            {{ classifySkipped }} non-bird, {{ classifyErrors }} failed.
          </template>
          <template v-else-if="classifyState === 'cancelled'">
            Stopped after {{ classifyDone }} of {{ classifyTotal }}.
            ({{ classifyHits }} hit, {{ classifyEmpty }} empty,
            {{ classifySkipped }} non-bird, {{ classifyErrors }} failed.)
          </template>
        </div>

        <div class="classify-preview">
          <div class="classify-preview-pane classify-pane-source">
            <div class="classify-pane-label">Source frame</div>
            <div class="classify-pane-frame">
              <img
                v-if="classifyCurrent"
                :key="`src-${classifyCurrent.name}`"
                :src="`/api/pictures/${encodeURIComponent(classifyCurrent.name)}?thumb=1`"
                :alt="classifyCurrent.name"
              />
              <div v-else class="classify-empty hint">—</div>
            </div>
          </div>
          <div class="classify-preview-pane">
            <div class="classify-pane-label">
              <template v-if="classifyCurrentNonBird">
                Non-bird — skipped
              </template>
              <template v-else-if="classifyCropTotal > 0 && classifyCropIdx >= 0">
                Crop {{ classifyCropIdx + 1 }} / {{ classifyCropTotal }}
              </template>
              <template v-else-if="classifyCropTotal === 0 && classifyCurrent && classifyState === 'running'">
                No bird crops kept
              </template>
              <template v-else>Crops</template>
            </div>
            <div class="classify-pane-frame">
              <img
                v-if="classifyCurrent && classifyCropIdx >= 0 && !classifyCurrentNonBird"
                :key="`crop-${classifyCurrent.name}-${classifyCropIdx}`"
                :src="`/api/pictures/${encodeURIComponent(classifyCurrent.name)}/crops/${classifyCropIdx}?t=${classifyDone}`"
                :alt="`Crop ${classifyCropIdx + 1}`"
              />
              <div v-else class="classify-empty hint">
                <template v-if="classifyCurrentNonBird">
                  bird pipeline skipped
                </template>
                <template v-else-if="classifyCurrent && classifyCropTotal === 0 && classifyState === 'running'">
                  none
                </template>
                <template v-else>—</template>
              </div>
            </div>
          </div>
        </div>

        <div v-if="classifyCurrent" class="classify-current-name">{{ classifyCurrent.name }}</div>

        <div class="classify-progress">
          <div class="classify-bar">
            <div class="classify-bar-fill" :style="{ width: classifyPercent + '%' }" />
          </div>
          <div class="classify-percent">{{ classifyPercent }}%</div>
        </div>

        <div class="classify-actions">
          <button
            v-if="classifyState === 'idle'"
            @click="startClassifyAll"
            :disabled="classifyTargets.length === 0"
          >
            Start
          </button>
          <button
            v-else-if="classifyState === 'running'"
            class="secondary"
            @click="cancelClassify"
          >
            Stop
          </button>
          <button class="secondary" @click="closeClassify">
            {{ classifyState === 'running' ? 'Close (will stop)' : 'Close' }}
          </button>
        </div>
      </div>
    </div>

    <div
      v-if="qualityOpen"
      class="info-modal-backdrop"
      role="dialog"
      aria-modal="true"
      @click.self="closeQualityScan"
      @keydown.esc="closeQualityScan"
      tabindex="-1"
    >
      <div class="classify-modal">
        <header class="info-header">
          <h3>AI image-quality scan</h3>
          <button type="button" class="tag-x" @click="closeQualityScan" aria-label="Close">×</button>
        </header>

        <div class="classify-status">
          <template v-if="qualityState === 'idle'">
            {{ qualityTargets.length }} pictures
            {{ qualityIncludeAnalyzed ? '' : 'without a score yet' }}.
          </template>
          <template v-else-if="qualityState === 'running'">
            Processing {{ qualityDone + 1 }} of {{ qualityTotal }} —
            <strong>{{ qualityScored }}</strong> scored,
            <strong>{{ qualityDeleted }}</strong> deleted,
            <strong>{{ qualityErrors }}</strong> failed.
          </template>
          <template v-else-if="qualityState === 'done'">
            Done. {{ qualityScored }} scored, {{ qualityDeleted }} deleted,
            {{ qualityErrors }} failed.
          </template>
          <template v-else-if="qualityState === 'cancelled'">
            Stopped after {{ qualityDone }} of {{ qualityTotal }}.
            ({{ qualityScored }} scored, {{ qualityDeleted }} deleted,
            {{ qualityErrors }} failed.)
          </template>
        </div>

        <div v-if="qualityState === 'idle'" class="quality-options">
          <label>
            <input type="checkbox" v-model="qualityAutoDelete" />
            Auto-delete pictures whose score falls below the discard threshold
          </label>
          <label>
            <input type="checkbox" v-model="qualityIncludeAnalyzed" />
            Include previously-analyzed pictures (re-score them)
          </label>
        </div>

        <div class="classify-preview">
          <div class="classify-preview-pane quality-preview-pane">
            <div class="classify-pane-label">Currently analyzing</div>
            <div class="classify-pane-frame">
              <img
                v-if="qualityCurrent"
                :key="`q-src-${qualityCurrent.name}`"
                :src="`/api/pictures/${encodeURIComponent(qualityCurrent.name)}?thumb=1`"
                :alt="qualityCurrent.name"
              />
              <div v-else class="classify-empty hint">—</div>
            </div>
          </div>
          <div class="classify-preview-pane quality-result-pane">
            <div class="classify-pane-label">Bird crop (scored by AI)</div>
            <div class="quality-result-card">
              <img
                v-if="qualityCurrent && qualityCurrent.has_crop"
                :key="`q-crop-${qualityCurrent.name}`"
                class="quality-result-img"
                :src="`/api/pictures/${encodeURIComponent(qualityCurrent.name)}/crop`"
                :alt="`Crop being scored for ${qualityCurrent.name}`"
              />
              <div class="quality-result-overlay">
                <template v-if="qualityCurrentError">
                  <div class="quality-overlay-error">{{ qualityCurrentError }}</div>
                </template>
                <template v-else-if="qualityCurrentScore !== null">
                  <div
                    class="quality-overlay-score"
                    :style="{ color: aiQualityColor(qualityCurrentScore) }"
                  >
                    {{ qualityCurrentScore }}<span class="quality-overlay-denom">/100</span>
                  </div>
                  <div class="quality-overlay-bar">
                    <div
                      class="quality-overlay-bar-fill"
                      :style="{
                        width: qualityCurrentScore + '%',
                        background: aiQualityColor(qualityCurrentScore),
                      }"
                    />
                  </div>
                  <div v-if="qualityCurrentDeleted" class="quality-overlay-deleted">
                    ✗ deleted (below threshold {{ qualityCurrentThreshold }})
                  </div>
                  <div
                    v-else-if="qualityCurrentThreshold !== null && qualityCurrentScore < qualityCurrentThreshold"
                    class="quality-overlay-hint"
                  >
                    below threshold {{ qualityCurrentThreshold }} — kept
                  </div>
                </template>
                <template v-else-if="qualityState === 'running'">
                  <div class="quality-overlay-hint">Sending crop to AI…</div>
                </template>
              </div>
            </div>
          </div>
        </div>

        <div v-if="qualityCurrent" class="classify-current-name">{{ qualityCurrent.name }}</div>

        <div class="classify-progress">
          <div class="classify-bar">
            <div class="classify-bar-fill" :style="{ width: qualityPercent + '%' }" />
          </div>
          <div class="classify-percent">{{ qualityPercent }}%</div>
        </div>

        <div class="classify-actions">
          <button
            v-if="qualityState === 'idle'"
            @click="startQualityScan"
            :disabled="qualityTargets.length === 0"
          >
            Start
          </button>
          <button
            v-else-if="qualityState === 'running'"
            class="secondary"
            @click="cancelQualityScan"
          >
            Stop
          </button>
          <button class="secondary" @click="closeQualityScan">
            {{ qualityState === 'running' ? 'Close (will stop)' : 'Close' }}
          </button>
        </div>
      </div>
    </div>

    <Teleport to="body">
      <div
        v-if="hoverFor && hoverPos && (hoverInfo || hoverLoading)"
        :ref="bindPopoverEl"
        class="bird-popover"
        :style="{ top: hoverPos.top + 'px', left: hoverPos.left + 'px' }"
        @mouseenter="onPopoverEnter"
        @mouseleave="onSpeciesLeave"
      >
        <div v-if="hoverLoading && !hoverInfo" class="hint">Looking up species…</div>
        <template v-else-if="hoverInfo">
          <div class="bird-popover-head">
            <div class="bird-popover-title">{{ hoverInfo.common_name }}</div>
            <div v-if="hoverInfo.scientific_name" class="bird-popover-sci">
              <em>{{ hoverInfo.scientific_name }}</em>
            </div>
          </div>
          <img
            v-if="hoverInfo.male_image || hoverInfo.female_image || hoverInfo.other_images?.[0]?.source"
            class="bird-popover-img"
            :src="hoverInfo.male_image || hoverInfo.female_image || hoverInfo.other_images![0].source"
            :alt="hoverInfo.common_name"
          />
          <div class="bird-popover-meta">
            <span v-if="hoverInfo.family">{{ hoverInfo.family }}</span>
            <span v-if="hoverInfo.conservation_status" class="bird-popover-status">
              {{ hoverInfo.conservation_status }}
            </span>
          </div>
          <p v-if="hoverInfo.description" class="bird-popover-desc">
            {{ hoverInfo.description.length > 240
                ? hoverInfo.description.slice(0, 240).trim() + '…'
                : hoverInfo.description }}
          </p>
        </template>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.gallery-layout {
  display: flex;
  gap: 1rem;
  padding: 1rem 1.2rem;
  height: calc(100vh - 60px); /* viewport minus the top nav bar */
  box-sizing: border-box;
}
.thumb-column {
  flex: 0 0 25%;
  max-width: 25%;
  overflow-y: auto;
  overflow-x: hidden;
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  /* stabilize sizing of thumbnails as images stream in */
  scrollbar-gutter: stable;
}
.search-bar {
  position: sticky;
  top: 0;
  z-index: 2;
  display: flex;
  align-items: center;
  gap: 0.3rem;
  padding: 0.5rem;
  background: #1a1a1a;
  border-bottom: 1px solid #2a2a2a;
}
.search-input {
  flex: 1;
  min-width: 0;
  padding: 0.35rem 0.5rem;
  font-size: 0.85rem;
  color: #ddd;
  background: #0f0f0f;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  outline: none;
}
.search-input:focus {
  border-color: #4d8bf0;
}
.search-input::placeholder { color: #666; }
.search-input::-webkit-search-cancel-button { display: none; }
.search-clear {
  width: 22px;
  height: 22px;
  padding: 0;
  font-size: 1.1rem;
  line-height: 1;
  color: #aaa;
  background: transparent;
  border: none;
  cursor: pointer;
  border-radius: 3px;
}
.search-clear:hover { color: #fff; background: #2a2a2a; }
.toolbar-row {
  position: sticky;
  top: 36px; /* sits directly under the .search-bar */
  z-index: 1;
  display: flex;
  gap: 0.4rem;
  padding: 0 0.5rem 0.5rem;
  background: #1a1a1a;
  border-bottom: 1px solid #2a2a2a;
}
.toolbar-btn {
  padding: 0.35rem 0.7rem;
  font-size: 0.85rem;
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
}
.toolbar-badge {
  display: inline-block;
  background: #2b7a2b;
  color: #fff;
  border-radius: 10px;
  padding: 0.05rem 0.45rem;
  font-size: 0.72rem;
  font-weight: 600;
  line-height: 1.2;
}
.thumb-row {
  display: flex;
  align-items: stretch;
  gap: 0.6rem;
  width: 100%;
  padding: 0.5rem;
  background: transparent;
  border: none;
  border-bottom: 1px solid #232323;
  text-align: left;
  cursor: pointer;
  color: inherit;
  border-radius: 0;
  position: relative;
  box-sizing: border-box;
}
.thumb-row:focus-visible {
  outline: 2px solid #4d8bf0;
  outline-offset: -2px;
}
.thumb-row:hover { background: #222; }
.thumb-row.active { background: #1e3868; }
.thumb-row img {
  /* Fixed-size thumbnail frame: 40% of the row wide, constant height,
     `object-fit: contain` so the full image is visible and letterboxed
     in black. `min-width: 0` and `max-width: 40%` are required —
     without them the flex item defaults to the image's natural pixel
     width as its min-size, which would blow past 40% on real photos. */
  flex: 0 0 40%;
  min-width: 0;
  max-width: 40%;
  height: 110px;
  object-fit: contain;
  background: #000;
  border-radius: 3px;
  overflow: hidden;
}
.thumb-meta {
  display: flex;
  flex-direction: column;
  justify-content: center;
  min-width: 0;
  flex: 1;
}
.thumb-birds {
  display: flex;
  flex-direction: column;
  margin-bottom: 0.15rem;
}
.thumb-bird {
  font-size: 0.8rem;
  color: #b8e6b8;
  font-style: italic;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  line-height: 1.2;
}
.thumb-bird + .thumb-bird {
  margin-top: 0.05rem;
}
.thumb-bird.has-info {
  cursor: help;
  text-decoration: underline dotted rgba(184, 230, 184, 0.4);
  text-underline-offset: 2px;
}
.thumb-bird.thumb-bird-current {
  /* Solid underline marks which bird-crop is currently shown in
     the thumbnail; cycling via the multi-crop arrows shifts it. */
  text-decoration: underline solid #b8e6b8;
  text-underline-offset: 2px;
  text-decoration-thickness: 2px;
  color: #d6f5d6;
}
.thumb-when {
  font-size: 0.8rem;
  color: #ddd;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.thumb-sub { margin-top: 0.2rem; display: flex; gap: 0.3rem; }
.species-tag {
  display: inline-block;
  background: #2b7a2b;
  color: #fff;
  font-size: 0.7rem;
  padding: 0.05rem 0.3rem;
  border-radius: 3px;
  text-transform: uppercase;
  letter-spacing: 0.3px;
}
.manual-tag {
  display: inline-block;
  background: #444;
  color: #ccc;
  font-size: 0.7rem;
  padding: 0.05rem 0.3rem;
  border-radius: 3px;
  text-transform: uppercase;
  letter-spacing: 0.3px;
}
.ai-quality-tag {
  display: inline-block;
  color: #000;
  font-size: 0.7rem;
  padding: 0.05rem 0.35rem;
  border-radius: 3px;
  font-weight: 600;
  letter-spacing: 0.3px;
  font-variant-numeric: tabular-nums;
}
.ai-quality-tag.ai-failed {
  background: #5a3a18;
  color: #ffd9a8;
  cursor: help;
}
.reclassify-btn {
  position: absolute;
  right: 0.4rem;
  bottom: 0.4rem;
  width: 22px;
  height: 22px;
  padding: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.55);
  color: #cde;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  cursor: pointer;
  opacity: 0.4;
  transition: opacity 120ms ease, background 120ms ease, color 120ms ease;
}
.thumb-row:hover .reclassify-btn,
.reclassify-btn.working,
.reclassify-btn:focus-visible {
  opacity: 1;
}
.reclassify-btn:hover {
  background: #2b7a2b;
  color: #fff;
}
.reclassify-btn:disabled {
  cursor: progress;
}
.spinner {
  width: 12px;
  height: 12px;
  border: 2px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  animation: spin 700ms linear infinite;
  display: inline-block;
}
@keyframes spin {
  to { transform: rotate(360deg); }
}

.preview-pane {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  overflow: hidden;
}
.preview-wrap {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.preview-stage {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #000;
  overflow: hidden;
}
.preview-img {
  max-width: 100%;
  max-height: 100%;
  object-fit: contain;
  display: block;
}
.multi-crop-badge {
  position: absolute;
  left: 0.4rem;
  bottom: 0.4rem;
  display: inline-flex;
  align-items: center;
  gap: 0.2rem;
  background: rgba(0, 0, 0, 0.7);
  color: #eee;
  font-size: 0.72rem;
  font-weight: 600;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  padding: 0.05rem 0.2rem;
  pointer-events: auto;
}
.multi-crop-badge .crop-arrow {
  background: transparent;
  border: 0;
  color: #cde;
  font-size: 1rem;
  line-height: 1;
  padding: 0 0.2rem;
  cursor: pointer;
  border-radius: 2px;
}
.multi-crop-badge .crop-arrow:hover {
  background: #333;
  color: #fff;
}
.preview-crop-cycle {
  position: absolute;
  left: 50%;
  bottom: 0.6rem;
  transform: translateX(-50%);
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  background: rgba(0, 0, 0, 0.65);
  color: #eee;
  font-weight: 600;
  border-radius: 6px;
  padding: 0.2rem 0.4rem;
  z-index: 5;
}
.preview-crop-cycle .crop-arrow {
  background: transparent;
  border: 0;
  color: #cde;
  font-size: 1.4rem;
  line-height: 1;
  padding: 0 0.3rem;
  cursor: pointer;
  border-radius: 3px;
}
.preview-crop-cycle .crop-arrow:hover {
  background: #333;
  color: #fff;
}
.preview-pip {
  position: absolute;
  right: 0.6rem;
  bottom: 0.6rem;
  width: 22%;
  max-width: 280px;
  min-width: 120px;
  border: 2px solid rgba(255, 255, 255, 0.85);
  border-radius: 4px;
  box-shadow: 0 4px 18px rgba(0, 0, 0, 0.6);
  background: #000;
}
.preview-footer {
  display: flex;
  align-items: center;
  gap: 0.8rem;
  padding: 0.6rem 1rem;
  border-top: 1px solid #2a2a2a;
  flex-wrap: wrap;
}
.preview-meta { min-width: 0; }
.preview-name { font-size: 0.95rem; }
.preview-sub { font-size: 0.8rem; color: #aaa; }
.preview-empty {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
}
.hint { color: #888; font-size: 0.9rem; }

.heart-btn {
  position: absolute;
  left: 0.4rem;
  top: 0.4rem;
  width: 22px;
  height: 22px;
  padding: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.55);
  color: #cde;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 120ms ease, background 120ms ease, color 120ms ease, border-color 120ms ease;
}
.thumb-row:hover .heart-btn,
.heart-btn:focus-visible,
.heart-btn.hearted {
  opacity: 1;
}
.heart-btn.hearted {
  color: #ff6b9b;
  border-color: #5a2840;
}
.heart-btn:hover {
  background: #4a2030;
  color: #ff8fb1;
  border-color: #5a2840;
}

.delete-btn {
  position: absolute;
  right: 0.4rem;
  top: 0.4rem;
  width: 22px;
  height: 22px;
  padding: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.55);
  color: #cde;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 120ms ease, background 120ms ease, color 120ms ease, border-color 120ms ease;
}
.thumb-row:hover .delete-btn,
.delete-btn:focus-visible {
  opacity: 1;
}
.delete-btn:hover {
  background: #5a2828;
  color: #fff;
  border-color: #5a2828;
}

.info-btn {
  position: absolute;
  right: 0.4rem;
  bottom: 2.0rem; /* above the reclassify button */
  width: 22px;
  height: 22px;
  padding: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.55);
  color: #cde;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  cursor: pointer;
  opacity: 0.4;
  transition: opacity 120ms ease, background 120ms ease, color 120ms ease;
}
.thumb-row:hover .info-btn,
.info-btn:focus-visible { opacity: 1; }
.info-btn:hover { background: #1e3868; color: #fff; }

.info-modal-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.78);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
  padding: 1.2rem;
  box-sizing: border-box;
}
.info-modal {
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  width: 100%;
  max-width: 1400px;
  max-height: 100%;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.info-header {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.6rem 1rem;
  border-bottom: 1px solid #2a2a2a;
}
.info-header h3 {
  margin: 0;
  font-size: 0.95rem;
  flex: 1;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.info-crop-cycle {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  background: #0d0d0d;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  padding: 0.15rem 0.4rem;
  font-size: 0.85rem;
  color: #ddd;
  flex-shrink: 0;
}
.info-crop-cycle .crop-arrow {
  background: transparent;
  border: 0;
  color: #cde;
  font-size: 1.2rem;
  line-height: 1;
  padding: 0 0.3rem;
  cursor: pointer;
  border-radius: 2px;
}
.info-crop-cycle .crop-arrow:hover {
  background: #2a2a2a;
  color: #fff;
}
.info-body {
  display: flex;
  gap: 1rem;
  padding: 1rem;
  flex: 1;
  min-height: 0;
}
.info-image-wrap {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  background: #000;
  border-radius: 4px;
  overflow: auto;
  max-height: calc(100vh - 8rem);
}
/* The frame is sized to match the image's actual rendered bounds (full
   width of the wrap, natural height from the image), so bboxes positioned
   at percentage coords land in exactly the right place. */
.info-image-frame {
  position: relative;
  width: 100%;
  display: block;
}
.info-image {
  display: block;
  width: 100%;
  height: auto;
}
.info-bbox-layer {
  position: absolute;
  inset: 0;
  pointer-events: none;
}
.info-bbox-layer .bbox {
  position: absolute;
  border: 2px solid #7fff7f;
  box-sizing: border-box;
}
.info-bbox-layer .bbox-label {
  position: absolute;
  top: 0;
  left: 0;
  transform: translateY(-100%);
  background: #7fff7f;
  color: #000;
  padding: 1px 4px;
  font: 0.72rem ui-monospace, SFMono-Regular, Menlo, monospace;
  white-space: nowrap;
}
.info-side {
  flex: 0 0 320px;
  max-width: 320px;
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
  overflow-y: auto;
}
.info-tabs {
  display: flex;
  gap: 0;
  margin-bottom: 0.4rem;
  border-bottom: 1px solid #2a2a2a;
}
.info-tab {
  background: transparent;
  border: none;
  border-bottom: 2px solid transparent;
  color: #888;
  padding: 0.4rem 0.8rem;
  cursor: pointer;
  font-size: 0.85rem;
  border-radius: 0;
  margin: 0;
}
.info-tab:hover { color: #ccc; }
.info-tab.active {
  color: #cde;
  border-bottom-color: #4d8bf0;
}
.info-debug {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.debug-pre {
  background: #050505;
  border: 1px solid #1f1f1f;
  border-radius: 3px;
  padding: 0.5rem;
  margin: 0;
  font-size: 0.72rem;
  line-height: 1.4;
  color: #cde;
  overflow: auto;
  max-height: 320px;
  white-space: pre-wrap;
  word-break: break-word;
  font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
}
.info-section {
  background: #0f0f0f;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  padding: 0.7rem;
}
.info-section h4 {
  margin: 0 0 0.5rem;
  font-size: 0.8rem;
  color: #aaa;
  text-transform: uppercase;
  letter-spacing: 0.4px;
}
.info-crop {
  display: block;
  width: 100%;
  border-radius: 3px;
  background: #000;
  cursor: zoom-in;
  transition: outline-color 120ms ease;
  outline: 2px solid transparent;
}
.info-crop:hover {
  outline-color: #4d8bf0;
}

.ai-quality-row {
  display: flex;
  align-items: center;
  gap: 0.6rem;
}
.ai-quality-bar {
  flex: 1;
  height: 12px;
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  overflow: hidden;
}
.ai-quality-bar-fill {
  height: 100%;
  transition: width 200ms ease, background 200ms ease;
}
.ai-quality-value {
  font: 0.85rem ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #ddd;
  min-width: 4rem;
  text-align: right;
}
.readonly-list {
  margin: 0;
  padding: 0;
  list-style: none;
  font-size: 0.85rem;
  display: flex;
  flex-direction: column;
}
.readonly-list li {
  display: flex;
  align-items: baseline;
  gap: 0.4rem;
  padding: 0.25rem 0;
  border-bottom: 1px solid #232323;
}
.readonly-list li:last-child { border-bottom: none; }
.row-name {
  flex: 1;
  min-width: 0;
  color: #ddd;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.row-name.has-info {
  cursor: help;
  text-decoration: underline dotted rgba(255, 255, 255, 0.3);
  text-underline-offset: 2px;
}
.row-conf {
  font: 0.8rem ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #b8d4b8;
  min-width: 3.4rem;
  text-align: right;
}
.form-label {
  display: block;
  font-size: 0.75rem;
  color: #aaa;
  text-transform: uppercase;
  letter-spacing: 0.3px;
  margin-bottom: 0.2rem;
}
.info-input {
  width: 100%;
  padding: 0.4rem 0.5rem;
  font-size: 0.9rem;
  color: #ddd;
  background: #1a1a1a;
  border: 1px solid #333;
  border-radius: 4px;
  box-sizing: border-box;
  outline: none;
}
.info-input:focus { border-color: #4d8bf0; }
.info-textarea { resize: vertical; min-height: 4em; font-family: inherit; }
.info-actions {
  display: flex;
  gap: 0.5rem;
  margin-top: 0.6rem;
  flex-wrap: wrap;
}
.info-meta { background: transparent; border: none; padding: 0.3rem 0.7rem; }
@media (max-width: 1100px) {
  .info-body { flex-direction: column; }
  .info-side { flex: 1 1 auto; max-width: none; }
}

.pip-backdrop {
  position: fixed;
  inset: 0;
  background: #000;
  z-index: 200;
  cursor: zoom-out;
  display: flex;
  align-items: center;
  justify-content: center;
}
.pip-main {
  max-width: 100%;
  max-height: 100%;
  object-fit: contain;
  display: block;
}
.pip-crop {
  position: absolute;
  right: 1rem;
  bottom: 1rem;
  width: 22%;
  max-width: 360px;
  min-width: 140px;
  border-radius: 6px;
  border: 2px solid rgba(255, 255, 255, 0.8);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.6);
  background: #000;
  cursor: default;
  pointer-events: none;
}
.pip-close {
  position: absolute;
  top: 0.6rem;
  right: 0.6rem;
  width: 36px;
  height: 36px;
  padding: 0;
  font-size: 1.6rem;
  line-height: 1;
  color: #fff;
  background: rgba(0, 0, 0, 0.55);
  border: 1px solid rgba(255, 255, 255, 0.3);
  border-radius: 4px;
  cursor: pointer;
}
.pip-close:hover { background: rgba(255, 255, 255, 0.15); }

.classify-modal {
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  width: 100%;
  max-width: 900px;
  display: flex;
  flex-direction: column;
}
.classify-status {
  padding: 0.6rem 1rem;
  font-size: 0.9rem;
  color: #ddd;
  border-bottom: 1px solid #2a2a2a;
}
.classify-range {
  display: flex;
  align-items: flex-end;
  gap: 0.8rem;
  padding: 0.6rem 1rem;
  border-bottom: 1px solid #2a2a2a;
}
.classify-range-field {
  display: flex;
  flex-direction: column;
  gap: 0.2rem;
  font-size: 0.78rem;
  color: #bbb;
}
.classify-range-field input {
  background: #0d0d0d;
  border: 1px solid #2a2a2a;
  color: #ddd;
  border-radius: 3px;
  padding: 0.25rem 0.4rem;
  font: 0.85rem ui-monospace, SFMono-Regular, Menlo, monospace;
}
.range-clear {
  align-self: flex-end;
  padding: 0.25rem 0.6rem;
  font-size: 0.8rem;
}
.classify-preview {
  display: flex;
  gap: 0.8rem;
  padding: 0.8rem 1rem 0.4rem;
}
.classify-preview-pane {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 0.3rem;
}
.classify-preview-pane.classify-pane-source {
  flex: 1.6; /* user wanted the source frame to be the bigger pane */
}
.classify-pane-label {
  font-size: 0.75rem;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  color: #888;
}
.classify-pane-frame {
  position: relative;
  background: #000;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  aspect-ratio: 16 / 9;
  overflow: hidden;
  display: flex;
  align-items: center;
  justify-content: center;
}
.classify-pane-frame img {
  max-width: 100%;
  max-height: 100%;
  width: auto;
  height: auto;
  object-fit: contain;
}
.classify-empty {
  font-size: 0.85rem;
  color: #666;
}
.classify-current-name {
  padding: 0 1rem;
  font: 0.8rem ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #aaa;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.classify-progress {
  display: flex;
  align-items: center;
  gap: 0.6rem;
  padding: 0.6rem 1rem;
}
.classify-bar {
  flex: 1;
  height: 10px;
  background: #0f0f0f;
  border: 1px solid #2a2a2a;
  border-radius: 5px;
  overflow: hidden;
}
.classify-bar-fill {
  height: 100%;
  background: linear-gradient(90deg, #2b7a2b, #4d8bf0);
  transition: width 200ms ease;
}
.classify-percent {
  font: 0.85rem ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #ccc;
  min-width: 3rem;
  text-align: right;
}
.classify-actions {
  display: flex;
  gap: 0.5rem;
  justify-content: flex-end;
  padding: 0.6rem 1rem 1rem;
  border-top: 1px solid #2a2a2a;
}
.quality-options {
  display: flex;
  flex-direction: column;
  gap: 0.4rem;
  padding: 0.4rem 1rem 0.6rem;
  font-size: 0.9rem;
  color: #ddd;
}
.quality-options label {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  cursor: pointer;
}
/* Result card now hosts the bird crop image with an absolutely-positioned
   overlay holding the score text + bar. */
.quality-result-card {
  position: relative;
  background: #000;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  flex: 1;
  aspect-ratio: 16 / 9;
  overflow: hidden;
}
.quality-result-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
  display: block;
}
.quality-result-overlay {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: flex-end;
  gap: 0.4rem;
  padding: 0.8rem;
  pointer-events: none;
  /* Subtle bottom gradient so the score is readable over light crops. */
  background: linear-gradient(to top, rgba(0, 0, 0, 0.65) 0%, rgba(0, 0, 0, 0) 50%);
}
.quality-overlay-score {
  font-size: 3rem;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  line-height: 1;
  text-shadow: 0 2px 6px rgba(0, 0, 0, 0.85);
}
.quality-overlay-denom {
  font-size: 1.3rem;
  font-weight: 500;
  color: #ccc;
  margin-left: 0.2rem;
}
.quality-overlay-bar {
  width: 80%;
  max-width: 320px;
  height: 10px;
  background: rgba(0, 0, 0, 0.5);
  border: 1px solid rgba(255, 255, 255, 0.25);
  border-radius: 5px;
  overflow: hidden;
}
.quality-overlay-bar-fill {
  height: 100%;
  transition: width 200ms ease, background 200ms ease;
}
.quality-overlay-deleted {
  color: #ff8080;
  font-weight: 600;
  font-size: 0.9rem;
  text-shadow: 0 1px 4px rgba(0, 0, 0, 0.9);
}
.quality-overlay-hint {
  color: #eee;
  font-size: 0.85rem;
  text-shadow: 0 1px 4px rgba(0, 0, 0, 0.9);
}
.quality-overlay-error {
  color: #ff8080;
  font-size: 0.85rem;
  text-align: center;
  padding: 0.5rem;
  background: rgba(0, 0, 0, 0.6);
  border-radius: 4px;
}

/* Modal: stack the picture (2/3 height) + bird-info section (1/3 height)
   in the left column. The flex children must opt-in to min-height: 0 so
   they're allowed to shrink below their natural content height. */
.info-left-column {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 0.8rem;
  min-height: 0;
}
.info-image-wrap {
  flex: 2 1 0;
  min-height: 0;
}
.info-bird-section {
  flex: 1 1 0;
  min-height: 0;
  display: flex;
  gap: 1rem;
  background: #0f0f0f;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  padding: 0.8rem 1rem;
  font-size: 0.9rem;
  color: #ddd;
}
/* Left half: text content (header + meta + description) — independently
   scrollable so a long description doesn't push the gallery out. */
.bird-section-text {
  flex: 1 1 0;
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
}
.bird-section-head { margin-bottom: 0.4rem; }
.bird-section-title {
  font-size: 1rem;
  font-weight: 600;
  color: #fff;
}
.bird-section-sci {
  font-size: 0.85rem;
  color: #aaa;
}
.bird-section-meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.4rem 0.8rem;
  margin-bottom: 0.5rem;
  font-size: 0.85rem;
  color: #bbb;
}
.bird-status-tag {
  background: #2b7a2b;
  color: #fff;
  padding: 0.05rem 0.4rem;
  border-radius: 3px;
  font-size: 0.72rem;
  text-transform: uppercase;
  letter-spacing: 0.4px;
}
.bird-section-desc {
  margin: 0;
  line-height: 1.45;
  color: #ddd;
}
/* Right half: image gallery — image fills available height, caption sits
   at the bottom inside the frame. */
.bird-section-gallery {
  flex: 1 1 0;
  min-width: 0;
  min-height: 0;
  display: flex;
  align-items: stretch;
  gap: 0.5rem;
}
.bird-gallery-nav {
  flex: 0 0 36px;
  width: 36px;
  padding: 0;
  font-size: 1.5rem;
  line-height: 1;
  background: #1a1a1a;
  color: #ddd;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  cursor: pointer;
  align-self: stretch;
}
.bird-gallery-nav:hover:not(:disabled) { background: #2a2a2a; }
.bird-gallery-nav:disabled { opacity: 0.3; cursor: default; }
.bird-gallery-frame {
  flex: 1;
  min-width: 0;
  min-height: 0;
  background: #000;
  border: 1px solid #2a2a2a;
  border-radius: 4px;
  overflow: hidden;
  display: flex;
  flex-direction: column;
}
.bird-gallery-frame img {
  flex: 1;
  min-height: 0;
  width: 100%;
  height: 100%;
  object-fit: contain;
  background: #000;
}
.bird-gallery-caption {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
  padding: 0.3rem 0.5rem;
  font-size: 0.75rem;
  color: #aaa;
  border-top: 1px solid #232323;
  background: #0f0f0f;
  flex: 0 0 auto;
}
.bird-gallery-counter {
  font: 0.75rem ui-monospace, SFMono-Regular, Menlo, monospace;
}

/* Hover popover (Teleported to <body>, so styles aren't scoped to .gallery
   — use unique class names that won't collide). */
.bird-popover {
  position: fixed;
  z-index: 300;
  width: 360px;
  max-width: calc(100vw - 16px);
  background: #1a1a1a;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  padding: 0.7rem 0.8rem;
  font-size: 0.85rem;
  color: #ddd;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.6);
  /* Avoid blocking the row's own mouseleave when the cursor is on the
     trigger (we still want pointer events on the popover itself). */
  pointer-events: auto;
}
.bird-popover-head { margin-bottom: 0.35rem; }
.bird-popover-title { font-weight: 600; color: #fff; }
.bird-popover-sci { color: #aaa; font-size: 0.8rem; }
.bird-popover-img {
  display: block;
  width: 100%;
  height: auto;
  border-radius: 4px;
  background: #000;
  margin-bottom: 0.4rem;
}
.bird-popover-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 0.3rem 0.6rem;
  margin-bottom: 0.35rem;
  font-size: 0.8rem;
  color: #bbb;
}
.bird-popover-status {
  background: #2b7a2b;
  color: #fff;
  padding: 0.05rem 0.4rem;
  border-radius: 3px;
  font-size: 0.7rem;
  text-transform: uppercase;
  letter-spacing: 0.3px;
}
.bird-popover-desc {
  margin: 0;
  line-height: 1.4;
  font-size: 0.82rem;
  color: #ccc;
}
</style>
