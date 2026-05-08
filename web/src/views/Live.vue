<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import Hls from 'hls.js'
import { api, type Detection, type Status } from '../api'
import { signalCapture } from '../bus'

const videoEl = ref<HTMLVideoElement | null>(null)
const status = ref<Status | null>(null)
const streamError = ref('')
const captureMsg = ref('')

const debugOpen = ref(false)
const debugData = ref<Detection[]>([])
const onlyWatched = ref(true)
const showGenericBoxes = ref(true)
const showBirdBoxes = ref(true)
const watchedSet = ref<Set<string>>(new Set())

function normalizeName(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]/g, '')
}

function isBird(d: Detection): boolean {
  return normalizeName(d.name) === 'bird'
}

const visibleDetections = computed(() => {
  let list = debugData.value
  if (onlyWatched.value) {
    // Always include birds when their toggle is on, regardless of the
    // watched-animals filter — bird is now its own pipeline and isn't
    // in WatchedAnimals by default.
    list = list.filter(
      (d) => watchedSet.value.has(normalizeName(d.name)) || (showBirdBoxes.value && isBird(d)),
    )
  }
  return list
})

// The bbox-layer only renders boxes whose class type's toggle is on.
// Used by the overlay; the JSON panel still shows all `visibleDetections`.
const visibleBoxes = computed(() =>
  visibleDetections.value.filter((d) =>
    isBird(d) ? showBirdBoxes.value : showGenericBoxes.value,
  ),
)
const debugJson = computed(() =>
  visibleDetections.value.length === 0 ? '[]' : JSON.stringify(visibleDetections.value, null, 2),
)

let hls: Hls | null = null
let statusTimer: number | undefined
let debugTimer: number | undefined

function startStream() {
  streamError.value = ''
  const v = videoEl.value
  if (!v) return
  const src = '/api/live/stream.m3u8'

  if (Hls.isSupported()) {
    hls = new Hls({
      lowLatencyMode: false,
      liveSyncDurationCount: 3,
      enableWorker: false,
      xhrSetup: (xhr) => { xhr.withCredentials = true },
    })
    hls.loadSource(src)
    hls.attachMedia(v)
    hls.on(Hls.Events.ERROR, (_e, data) => {
      if (!data.fatal) return
      streamError.value = `Stream error: ${data.type}/${data.details}`
      if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
        hls?.startLoad()
      } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
        hls?.recoverMediaError()
      }
    })
  } else if (v.canPlayType('application/vnd.apple.mpegurl')) {
    v.src = src
  } else {
    streamError.value = 'HLS is not supported in this browser'
  }
}

function stopStream() {
  if (hls) {
    hls.destroy()
    hls = null
  }
  if (videoEl.value) {
    videoEl.value.removeAttribute('src')
    videoEl.value.load()
  }
}

async function refreshStatus() {
  try {
    status.value = await api.status()
  } catch {
    /* ignore */
  }
}

async function takePicture() {
  captureMsg.value = ''
  try {
    const r = await api.capture()
    captureMsg.value = `Saved ${r.name}`
    signalCapture()
    setTimeout(() => (captureMsg.value = ''), 3000)
  } catch (e: any) {
    captureMsg.value = `Failed: ${e?.message ?? 'error'}`
  }
}

// Fire the capture signal when the detector records an auto-capture, so
// the Gallery tab picks up new detection JPEGs without polling.
watch(
  () => status.value?.last_capture_at,
  (now, prev) => {
    if (now && prev && now !== prev) signalCapture()
  },
)

async function pollDebug() {
  try {
    debugData.value = await api.detectDebug(0.1, 20)
  } catch {
    /* ignore — detector may be warming up or endpoint 503 */
  }
}

async function refreshWatchedSet() {
  try {
    const cfg = await api.getConfig()
    watchedSet.value = new Set(cfg.watched_animals.map((a) => normalizeName(a.name)))
  } catch {
    watchedSet.value = new Set()
  }
}

function toggleDebug() {
  if (debugOpen.value) {
    debugOpen.value = false
    if (debugTimer) {
      window.clearInterval(debugTimer)
      debugTimer = undefined
    }
    debugData.value = []
  } else {
    debugOpen.value = true
    refreshWatchedSet()
    pollDebug()
    debugTimer = window.setInterval(pollDebug, 1000)
  }
}

onMounted(() => {
  startStream()
  refreshStatus()
  statusTimer = window.setInterval(refreshStatus, 1500)
})

onUnmounted(() => {
  stopStream()
  if (statusTimer) window.clearInterval(statusTimer)
  if (debugTimer) window.clearInterval(debugTimer)
})
</script>

<template>
  <div class="container">
    <div class="card">
      <div class="live-layout" :class="{ 'debug-open': debugOpen }">
        <div class="video-wrap">
          <video ref="videoEl" autoplay playsinline muted controls />
          <div v-if="debugOpen" class="bbox-layer">
            <template v-for="(d, i) in visibleBoxes" :key="i">
              <div
                v-if="d.box"
                class="bbox"
                :class="{ bird: isBird(d) }"
                :style="{
                  left: (d.box.x1 * 100) + '%',
                  top: (d.box.y1 * 100) + '%',
                  width: ((d.box.x2 - d.box.x1) * 100) + '%',
                  height: ((d.box.y2 - d.box.y1) * 100) + '%',
                }"
              >
                <span class="bbox-label">
                  {{ d.name }} {{ (d.confidence * 100).toFixed(0) }}%<template
                    v-if="d.species && d.species.length"
                  ><br />{{ d.species[0].name }} {{ (d.species[0].confidence * 100).toFixed(0) }}%</template>
                </span>
              </div>
            </template>
          </div>
        </div>
        <aside v-if="debugOpen" class="debug-panel">
          <header class="debug-header">
            <h3>Debug output</h3>
            <button type="button" class="tag-x" @click="toggleDebug" aria-label="Close">×</button>
          </header>
          <label class="debug-filter">
            <input type="checkbox" v-model="onlyWatched" />
            Only watched animals
          </label>
          <label class="debug-filter">
            <input type="checkbox" v-model="showGenericBoxes" />
            <span class="swatch generic" />
            Generic detection bboxes
          </label>
          <label class="debug-filter">
            <input type="checkbox" v-model="showBirdBoxes" />
            <span class="swatch bird" />
            Bird detection bboxes
          </label>
          <pre class="json-block"><code>{{ debugJson }}</code></pre>
        </aside>
      </div>
      <div class="controls-row">
        <button @click="takePicture">Capture</button>
        <button class="secondary" @click="toggleDebug">
          {{ debugOpen ? 'Hide debug' : 'Debug output' }}
        </button>
        <span v-if="captureMsg" class="success">{{ captureMsg }}</span>
        <div class="spacer" style="flex:1" />
        <div class="bird-indicator" :class="{ active: (status?.animals_present?.length ?? 0) > 0 }">
          <span class="dot" />
          <span v-if="(status?.animals_present?.length ?? 0) > 0">
            Animal detected:
            <span v-for="(a, i) in status!.animals_present" :key="a.class_id">
              {{ i > 0 ? ', ' : '' }}{{ a.name }} ({{ (a.confidence * 100).toFixed(0) }}%)<span
                v-if="a.species && a.species.length"
                class="species-tag"
              >
                — {{ a.species[0].name }} {{ (a.species[0].confidence * 100).toFixed(0) }}%
              </span>
            </span>
          </span>
          <span v-else-if="status?.detector_ready">No animal</span>
          <span v-else>Detector loading…</span>
        </div>
      </div>
      <div class="error" v-if="streamError">{{ streamError }}</div>
      <div class="error" v-else-if="status && !status.rtsp_connected">
        Not connected to camera — check the RTSP URL in
        <router-link :to="{ name: 'settings' }">Settings</router-link>.
      </div>
    </div>
  </div>
</template>

<style scoped>
.live-layout {
  display: flex;
  gap: 1rem;
  align-items: flex-start;
}
.video-wrap {
  flex: 1;
  min-width: 0;
  position: relative;
}
.video-wrap video {
  width: 100%;
  display: block;
}
.bbox-layer {
  position: absolute;
  inset: 0;
  pointer-events: none;
}
.bbox {
  position: absolute;
  border: 2px solid #7fff7f;
  box-sizing: border-box;
}
.bbox.bird {
  border-color: #6aa9ff;
}
.bbox-label {
  position: absolute;
  top: 0;
  left: 0;
  transform: translateY(-100%);
  background: #7fff7f;
  color: #000;
  padding: 1px 4px;
  font: 0.72rem ui-monospace, SFMono-Regular, Menlo, monospace;
  white-space: nowrap;
  line-height: 1.1;
}
.bbox.bird .bbox-label {
  background: #6aa9ff;
}
.swatch {
  display: inline-block;
  width: 0.7rem;
  height: 0.7rem;
  border-radius: 2px;
  margin-right: 0.1rem;
}
.swatch.generic { background: #7fff7f; }
.swatch.bird { background: #6aa9ff; }
.species-tag {
  color: #cdb;
  font-style: italic;
  margin-left: 0.15rem;
}
.debug-panel {
  flex: 0 0 360px;
  display: flex;
  flex-direction: column;
  background: #0f0f0f;
  border: 1px solid #2a2a2a;
  border-radius: 6px;
  padding: 0.8rem;
}
.debug-header {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin-bottom: 0.6rem;
}
.debug-header h3 {
  margin: 0;
  font-size: 0.95rem;
  flex: 1;
}
.debug-filter {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  font-size: 0.85rem;
  color: #bbb;
  margin-bottom: 0.5rem;
  user-select: none;
}
.debug-filter input[type='checkbox'] { margin: 0; }
.tag-x {
  background: transparent;
  border: none;
  color: #bbb;
  cursor: pointer;
  padding: 0;
  font-size: 1.2rem;
  line-height: 1;
}
.tag-x:hover { color: #fff; }
.json-block {
  background: #050505;
  border: 1px solid #333;
  border-radius: 4px;
  padding: 0.8rem;
  color: #ddd;
  margin: 0;
  font: 0.8rem ui-monospace, SFMono-Regular, Menlo, monospace;
  overflow: auto;
  max-height: 70vh;
  white-space: pre-wrap;
  word-break: break-word;
}
@media (max-width: 900px) {
  .live-layout { flex-direction: column; }
  .debug-panel { flex: 1 1 auto; width: 100%; }
}
</style>
