<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { api, type Config } from '../api'

const DEFAULT_THRESHOLD = 0.35

const config = ref<Config | null>(null)
const classes = ref<string[]>([])
const saveMsg = ref('')
const saveErr = ref('')
const testing = ref(false)
const testMsg = ref('')

const oldPw = ref('')
const newPw = ref('')
const newPw2 = ref('')
const pwMsg = ref('')
const pwErr = ref('')

const animalInput = ref('')

function normalize(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]/g, '')
}

const watchedSet = computed(() =>
  new Set((config.value?.watched_animals ?? []).map((a) => normalize(a.name)))
)

const watchedHasBird = computed(() => watchedSet.value.has('bird'))

const suggestions = computed(() => {
  const q = normalize(animalInput.value)
  if (!q) return []
  return classes.value
    .filter((c) => normalize(c).includes(q) && !watchedSet.value.has(normalize(c)))
    .slice(0, 12)
})

async function load() {
  config.value = await api.getConfig()
  try {
    classes.value = await api.classes()
  } catch {
    classes.value = []
  }
}

async function save() {
  saveMsg.value = ''
  saveErr.value = ''
  try {
    config.value = await api.saveConfig(config.value!)
    saveMsg.value = 'Saved'
    setTimeout(() => (saveMsg.value = ''), 2500)
  } catch (e: any) {
    saveErr.value = e?.message ?? 'Save failed'
  }
}

async function testRtsp() {
  testing.value = true
  testMsg.value = ''
  try {
    const r = await api.testConnection(config.value!.rtsp_url)
    testMsg.value = r.ok ? 'Connection OK' : `Failed: ${r.error}`
  } catch (e: any) {
    testMsg.value = `Failed: ${e?.message ?? 'error'}`
  } finally {
    testing.value = false
  }
}

function addAnimal(name: string) {
  if (!config.value) return
  const n = name.trim()
  if (!n) return
  if (watchedSet.value.has(normalize(n))) return
  config.value.watched_animals = [
    ...config.value.watched_animals,
    { name: n, threshold: DEFAULT_THRESHOLD },
  ]
  animalInput.value = ''
}

const applyingCorrections = ref(false)
const applyCorrectionsMsg = ref('')
const applyCorrectionsErr = ref('')

const aiTesting = ref(false)
const aiTestMsg = ref('')
const aiTestErr = ref('')
const aiTestRaw = ref('')

async function testAIQualityConn() {
  if (!config.value || aiTesting.value) return
  aiTestMsg.value = ''
  aiTestErr.value = ''
  aiTestRaw.value = ''
  aiTesting.value = true
  try {
    const r = await api.testAIQuality({
      url: config.value.ai_quality?.url,
      model: config.value.ai_quality?.model,
      bearer_token: config.value.ai_quality?.bearer_token,
    })
    if (r.ok) {
      const lat = r.latency_ms ? ` in ${r.latency_ms} ms` : ''
      aiTestMsg.value = `OK — model returned score ${r.score}${lat}.`
    } else {
      const status = r.http_status ? ` (HTTP ${r.http_status})` : ''
      const lat = r.latency_ms ? `, ${r.latency_ms} ms` : ''
      aiTestErr.value = `Failed: ${r.error}${status}${lat}`
    }
    if (r.raw_response) {
      aiTestRaw.value = r.raw_response
    }
  } catch (e: any) {
    aiTestErr.value = e?.message ?? 'request failed'
  } finally {
    aiTesting.value = false
  }
}

const ebirdTesting = ref(false)
const ebirdTestMsg = ref('')
const ebirdTestErr = ref('')
const ebirdTestSample = ref<string[]>([])

async function testEBirdConn() {
  if (!config.value || ebirdTesting.value) return
  ebirdTestMsg.value = ''
  ebirdTestErr.value = ''
  ebirdTestSample.value = []
  ebirdTesting.value = true
  try {
    const r = await api.testEBird({
      api_key: config.value.ebird?.api_key,
      region: config.value.ebird?.region,
      lat: config.value.ebird?.lat,
      lng: config.value.ebird?.lng,
      dist_km: config.value.ebird?.dist_km,
      back_days: config.value.ebird?.back_days,
    })
    if (r.ok) {
      const lat = r.latency_ms ? ` in ${r.latency_ms} ms` : ''
      ebirdTestMsg.value = `OK — ${r.count ?? 0} species fetched${lat}.`
      ebirdTestSample.value = r.sample ?? []
    } else {
      const lat = r.latency_ms ? ` (${r.latency_ms} ms)` : ''
      ebirdTestErr.value = `Failed: ${r.error}${lat}`
    }
  } catch (e: any) {
    ebirdTestErr.value = e?.message ?? 'request failed'
  } finally {
    ebirdTesting.value = false
  }
}

async function applyCorrectionsToAll() {
  if (applyingCorrections.value) return
  applyCorrectionsMsg.value = ''
  applyCorrectionsErr.value = ''
  if (!confirm(
    'Apply current correction rules to every existing picture\'s saved species data?\n\nThis rewrites bird-species names in metadata sidecars (your manual notes are not touched).'
  )) return
  applyingCorrections.value = true
  try {
    const r = await api.applyCorrections()
    applyCorrectionsMsg.value = `Scanned ${r.processed} pictures, updated ${r.modified}.`
    if (r.first_error) {
      applyCorrectionsErr.value = `First error: ${r.first_error}`
    }
  } catch (e: any) {
    applyCorrectionsErr.value = e?.message ?? 'apply failed'
  } finally {
    applyingCorrections.value = false
  }
}

function addCorrectionRule() {
  if (!config.value) return
  config.value.classifier_corrections = [
    ...(config.value.classifier_corrections ?? []),
    { detected: '', correction: '', regex: false },
  ]
}

function removeCorrectionRule(idx: number) {
  if (!config.value) return
  config.value.classifier_corrections = (config.value.classifier_corrections ?? []).filter(
    (_, i) => i !== idx,
  )
}

function removeAnimal(name: string) {
  if (!config.value) return
  config.value.watched_animals = config.value.watched_animals.filter((a) => a.name !== name)
}

function onAnimalKey(e: KeyboardEvent) {
  if (e.key === 'Enter') {
    e.preventDefault()
    if (suggestions.value.length > 0) {
      addAnimal(suggestions.value[0])
    } else if (animalInput.value.trim()) {
      addAnimal(animalInput.value.trim())
    }
  }
}

async function changePw() {
  pwMsg.value = ''
  pwErr.value = ''
  if (newPw.value.length < 6) {
    pwErr.value = 'New password must be at least 6 characters'
    return
  }
  if (newPw.value !== newPw2.value) {
    pwErr.value = 'New passwords do not match'
    return
  }
  try {
    await api.changePassword(oldPw.value, newPw.value)
    pwMsg.value = 'Password changed'
    oldPw.value = ''
    newPw.value = ''
    newPw2.value = ''
  } catch (e: any) {
    pwErr.value = e?.message ?? 'Change failed'
  }
}

onMounted(load)
</script>

<template>
  <div class="container" v-if="config">
    <div class="card">
      <h2>Camera</h2>
      <div class="form-row">
        <label>RTSP URL</label>
        <input v-model="config.rtsp_url" type="url" placeholder="rtsp://user:pass@192.168.1.10:554/stream" />
      </div>
      <div class="controls-row">
        <button @click="save">Save</button>
        <button class="secondary" :disabled="testing" @click="testRtsp">
          {{ testing ? 'Testing…' : 'Test connection' }}
        </button>
        <span v-if="saveMsg" class="success">{{ saveMsg }}</span>
        <span v-if="saveErr" class="error">{{ saveErr }}</span>
        <span v-if="testMsg" :class="testMsg.startsWith('Connection OK') ? 'success' : 'error'">
          {{ testMsg }}
        </span>
      </div>
    </div>

    <div class="card">
      <h2>Animal detection</h2>
      <div class="form-row">
        <label>
          <input type="checkbox" v-model="config.auto_capture_enabled" />
          Automatically capture when a watched animal is detected
        </label>
      </div>
      <div class="form-row">
        <label>Minimum seconds between auto-captures (different sightings)</label>
        <input type="number" min="1" max="60" v-model.number="config.detection_cooldown_s" />
      </div>
      <div class="form-row">
        <label>Session timeout (seconds)</label>
        <input type="number" min="5" max="600" v-model.number="config.session_timeout_s" />
        <div class="hint" style="margin-top: 0.3rem">
          While a sighting keeps re-detecting within this window, only the
          single best frame is kept on disk — additional frames overwrite the
          previous best when they score better. Set higher to dedupe longer
          visits, lower to capture more frames.
        </div>
      </div>
      <div class="form-row">
        <label>Watched animals</label>
        <table v-if="config.watched_animals.length > 0" class="watched-table">
          <thead>
            <tr>
              <th>Animal</th>
              <th>Confidence threshold</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in config.watched_animals" :key="a.name">
              <td class="name-cell">{{ a.name }}</td>
              <td class="slider-cell">
                <input
                  type="range"
                  min="0.1"
                  max="0.9"
                  step="0.05"
                  v-model.number="a.threshold"
                />
                <span class="threshold-value">{{ a.threshold.toFixed(2) }}</span>
              </td>
              <td class="action-cell">
                <button type="button" class="tag-x" @click="removeAnimal(a.name)" aria-label="Remove">×</button>
              </td>
            </tr>
          </tbody>
        </table>
        <div v-else class="hint">(none — no auto-capture will fire)</div>
        <div class="animal-picker">
          <input
            v-model="animalInput"
            type="text"
            placeholder="Type to search species and press Enter"
            @keydown="onAnimalKey"
          />
          <div v-if="suggestions.length > 0" class="suggestions">
            <button
              v-for="s in suggestions"
              :key="s"
              type="button"
              class="suggestion"
              @click="addAnimal(s)"
            >{{ s }}</button>
          </div>
        </div>
        <div v-if="classes.length === 0" class="hint">
          (class list unavailable — the detector model may not be loaded)
        </div>
      </div>
      <button @click="save">Save</button>
    </div>

    <div class="card">
      <h2>Classifier corrections</h2>
      <div class="hint" style="margin-bottom: 0.6rem">
        Rewrite classifier names that don't match the bird-info database (e.g.
        <em>Tit mouse</em> → <em>Tufted Titmouse</em>). Literal entries match
        case-insensitively. Tick the regex box to use a Go-flavored regex —
        also case-insensitive; the correction is always plain text (no
        backreferences). First matching rule wins.
      </div>
      <table
        v-if="(config.classifier_corrections ?? []).length > 0"
        class="watched-table"
      >
        <thead>
          <tr>
            <th>Detected</th>
            <th class="cc-regex-col">Regex?</th>
            <th>Correction</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(rule, idx) in config.classifier_corrections" :key="idx">
            <td class="cc-input-cell">
              <input
                v-model="rule.detected"
                type="text"
                placeholder="e.g. Tit mouse"
              />
            </td>
            <td class="cc-regex-cell">
              <input type="checkbox" v-model="rule.regex" />
            </td>
            <td class="cc-input-cell">
              <input
                v-model="rule.correction"
                type="text"
                placeholder="e.g. Tufted Titmouse"
              />
            </td>
            <td class="action-cell">
              <button
                type="button"
                class="tag-x"
                @click="removeCorrectionRule(idx)"
                aria-label="Remove rule"
              >×</button>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-else class="hint">(no rules — classifier output is used as-is)</div>
      <div class="controls-row" style="margin-top: 0.6rem">
        <button class="secondary" @click="addCorrectionRule">Add rule</button>
        <button @click="save">Save</button>
        <button
          class="secondary"
          :disabled="applyingCorrections || (config.classifier_corrections ?? []).length === 0"
          :title="(config.classifier_corrections ?? []).length === 0
                  ? 'Add at least one rule first'
                  : 'Rewrite saved bird_species in every picture\'s metadata'"
          @click="applyCorrectionsToAll"
        >
          {{ applyingCorrections ? 'Applying…' : 'Apply to existing pictures' }}
        </button>
        <span v-if="saveMsg" class="success">{{ saveMsg }}</span>
        <span v-if="saveErr" class="error">{{ saveErr }}</span>
        <span v-if="applyCorrectionsMsg" class="success">{{ applyCorrectionsMsg }}</span>
        <span v-if="applyCorrectionsErr" class="error">{{ applyCorrectionsErr }}</span>
      </div>
    </div>

    <div class="card">
      <h2>AI image-quality scoring</h2>
      <div class="hint" style="margin-bottom: 0.6rem">
        At the end of each sighting, the top
        <strong>{{ config.ai_quality?.max_candidates ?? 5 }}</strong>
        candidate frames are sent to your AI endpoint with the classifier's
        species + confidence. The endpoint returns a 0–100 quality score per
        image. The highest-scoring frame becomes the kept picture; if even
        the best falls below the discard threshold the picture is deleted
        from the gallery. Endpoint must speak OpenAI-compatible
        <code>/v1/chat/completions</code> with vision (e.g. OpenAI,
        Ollama, vLLM, llama.cpp server). Failures never delete pictures.
      </div>
      <div class="form-row">
        <label>
          <input type="checkbox" v-model="config.ai_quality.enabled" />
          Enable AI quality scoring
        </label>
      </div>
      <div class="form-row">
        <label>Endpoint URL</label>
        <input
          v-model="config.ai_quality.url"
          type="url"
          placeholder="https://api.openai.com/v1/chat/completions"
        />
      </div>
      <div class="form-row">
        <label>Model</label>
        <input
          v-model="config.ai_quality.model"
          type="text"
          placeholder="gpt-4o-mini"
        />
      </div>
      <div class="form-row">
        <label>Bearer token</label>
        <input
          v-model="config.ai_quality.bearer_token"
          type="password"
          autocomplete="off"
          placeholder="sk-..."
        />
      </div>
      <div class="form-row">
        <label>
          Discard threshold:
          {{ config.ai_quality.discard_threshold ?? 50 }} / 100
        </label>
        <input
          type="range"
          min="0"
          max="100"
          step="1"
          v-model.number="config.ai_quality.discard_threshold"
          style="width: 100%"
        />
      </div>
      <div class="form-row">
        <label>Normalize width (px)</label>
        <input
          type="number"
          min="64"
          max="4096"
          v-model.number="config.ai_quality.normalize_width"
        />
      </div>
      <div class="form-row">
        <label>Max candidates per session</label>
        <input
          type="number"
          min="1"
          max="10"
          v-model.number="config.ai_quality.max_candidates"
        />
      </div>
      <div class="controls-row">
        <button @click="save">Save</button>
        <button
          class="secondary"
          :disabled="aiTesting || !config.ai_quality?.url || !config.ai_quality?.model"
          :title="!config.ai_quality?.url || !config.ai_quality?.model
                  ? 'Fill in URL and model first'
                  : 'Send a tiny synthetic image to the endpoint and report what comes back'"
          @click="testAIQualityConn"
        >
          {{ aiTesting ? 'Testing…' : 'Test connection' }}
        </button>
        <span v-if="saveMsg" class="success">{{ saveMsg }}</span>
        <span v-if="saveErr" class="error">{{ saveErr }}</span>
        <span v-if="aiTestMsg" class="success">{{ aiTestMsg }}</span>
        <span v-if="aiTestErr" class="error">{{ aiTestErr }}</span>
      </div>
      <details v-if="aiTestRaw" class="ai-test-raw">
        <summary>Raw model response</summary>
        <pre><code>{{ aiTestRaw }}</code></pre>
      </details>
    </div>

    <div class="card">
      <h2>Bird detection</h2>
      <div class="hint" style="margin-bottom: 0.6rem">
        Birds use a dedicated pipeline: the bird-specific YOLO model
        runs every tick, every detected bird crop is buffered through
        the sighting window, and at session close the top
        <strong>{{ config.bird_max_crops ?? 3 }}</strong>
        crops by classifier confidence are sent to AI quality scoring.
        Crops above the AI discard threshold (set in the AI Quality
        card) are kept; sightings with no surviving crops are silently
        dropped. Other animals (deer, fox, cat, dog) use the watched-
        animals list above and are unaffected by these settings.
      </div>
      <div class="form-row">
        <label>
          Bird detection confidence:
          {{ ((config.bird_confidence_threshold ?? 0.30) * 100).toFixed(0) }}%
        </label>
        <input
          type="range"
          min="0.05"
          max="0.95"
          step="0.05"
          v-model.number="config.bird_confidence_threshold"
          style="width: 100%"
        />
      </div>
      <div class="form-row">
        <label>Max bird crops kept per session (1–10)</label>
        <input
          type="number"
          min="1"
          max="10"
          v-model.number="config.bird_max_crops"
        />
      </div>
      <div class="form-row" v-if="watchedHasBird">
        <div class="hint" style="color: #b4884c">
          Heads up: your watched-animals list includes "bird". The
          generic pipeline ignores it now — bird detection is fully
          handled by the controls above. Feel free to remove the
          row; it has no effect.
        </div>
      </div>
      <button @click="save">Save</button>
    </div>

    <div class="card">
      <h2>Location-aware species filter (eBird)</h2>
      <div class="hint" style="margin-bottom: 0.6rem">
        When enabled, the bird-species classifier's guesses are filtered
        against the set of species recently observed near you according to
        the public eBird API. Species not on the local list are dropped, so
        a Mountain Bluebird suggestion in Pennsylvania becomes the next-best
        guess instead. Refreshes once on startup and every 24h. Get a free
        API key at
        <a href="https://ebird.org/api/keygen" target="_blank" rel="noopener">
          ebird.org/api/keygen</a>. Provide latitude+longitude (preferred)
        or an eBird region code (e.g. <code>US-PA-101</code>).
      </div>
      <div class="form-row">
        <label>
          <input type="checkbox" v-model="config.ebird.enabled" />
          Enable eBird species filter
        </label>
      </div>
      <div class="form-row">
        <label>API key</label>
        <input
          v-model="config.ebird.api_key"
          type="password"
          autocomplete="off"
          placeholder="eBird API token"
        />
      </div>
      <div class="form-row">
        <label>Latitude</label>
        <input
          v-model.number="config.ebird.lat"
          type="number"
          step="0.000001"
          placeholder="40.0379"
        />
      </div>
      <div class="form-row">
        <label>Longitude</label>
        <input
          v-model.number="config.ebird.lng"
          type="number"
          step="0.000001"
          placeholder="-76.3055"
        />
      </div>
      <div class="form-row">
        <label>Region code (used when lat/lng are 0)</label>
        <input
          v-model="config.ebird.region"
          type="text"
          placeholder="US-PA-101"
        />
      </div>
      <div class="form-row">
        <label>Search radius (km, geo only — max 50)</label>
        <input
          type="number"
          min="1"
          max="50"
          v-model.number="config.ebird.dist_km"
        />
      </div>
      <div class="form-row">
        <label>Look-back window (days, max 30)</label>
        <input
          type="number"
          min="1"
          max="30"
          v-model.number="config.ebird.back_days"
        />
      </div>
      <div class="controls-row">
        <button @click="save">Save</button>
        <button
          class="secondary"
          :disabled="ebirdTesting || !config.ebird?.api_key
                    || (!(config.ebird?.lat || config.ebird?.lng) && !config.ebird?.region)"
          :title="!config.ebird?.api_key
                  ? 'Enter an API key first'
                  : 'Fetch the species list with the current settings'"
          @click="testEBirdConn"
        >
          {{ ebirdTesting ? 'Testing…' : 'Test connection' }}
        </button>
        <span v-if="saveMsg" class="success">{{ saveMsg }}</span>
        <span v-if="saveErr" class="error">{{ saveErr }}</span>
        <span v-if="ebirdTestMsg" class="success">{{ ebirdTestMsg }}</span>
        <span v-if="ebirdTestErr" class="error">{{ ebirdTestErr }}</span>
      </div>
      <details v-if="ebirdTestSample.length" class="ai-test-raw">
        <summary>Sample species ({{ ebirdTestSample.length }})</summary>
        <pre><code>{{ ebirdTestSample.join('\n') }}</code></pre>
      </details>
    </div>

    <div class="card">
      <h2>Change password</h2>
      <div class="form-row">
        <label>Current password</label>
        <input v-model="oldPw" type="password" autocomplete="current-password" />
      </div>
      <div class="form-row">
        <label>New password</label>
        <input v-model="newPw" type="password" autocomplete="new-password" />
      </div>
      <div class="form-row">
        <label>Confirm new password</label>
        <input v-model="newPw2" type="password" autocomplete="new-password" />
      </div>
      <button @click="changePw">Change password</button>
      <div v-if="pwMsg" class="success">{{ pwMsg }}</div>
      <div v-if="pwErr" class="error">{{ pwErr }}</div>
    </div>
  </div>
</template>

<style scoped>
.tag-list {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
  margin-bottom: 0.5rem;
}
.tag {
  display: inline-flex;
  align-items: center;
  gap: 0.3rem;
  padding: 0.2rem 0.5rem;
  background: #2a2a2a;
  border: 1px solid #444;
  border-radius: 4px;
  font-size: 0.9rem;
}
.tag-x {
  background: transparent;
  border: none;
  color: #bbb;
  cursor: pointer;
  padding: 0;
  font-size: 1rem;
  line-height: 1;
}
.tag-x:hover { color: #fff; }
.watched-table {
  width: 100%;
  border-collapse: collapse;
  margin-bottom: 0.6rem;
}
.watched-table th {
  text-align: left;
  font-weight: 500;
  color: #aaa;
  font-size: 0.8rem;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  padding: 0.3rem 0.5rem;
  border-bottom: 1px solid #333;
}
.watched-table td {
  padding: 0.4rem 0.5rem;
  border-bottom: 1px solid #2a2a2a;
  vertical-align: middle;
}
.watched-table tr:last-child td { border-bottom: none; }
.name-cell {
  font-family: inherit;
  font-size: 0.95rem;
  white-space: nowrap;
  width: 1%;
  padding-right: 1rem;
}
.slider-cell {
  display: flex;
  align-items: center;
  gap: 0.6rem;
}
.slider-cell input[type='range'] { flex: 1; }
.threshold-value {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.85rem;
  color: #ccc;
  min-width: 2.6rem;
  text-align: right;
}
.action-cell { width: 1%; text-align: right; }
.cc-input-cell input[type='text'] {
  width: 100%;
  padding: 0.3rem 0.45rem;
  font-size: 0.9rem;
}
.cc-regex-col { width: 1%; white-space: nowrap; }
.cc-regex-cell { text-align: center; }
.cc-regex-cell input[type='checkbox'] { margin: 0; }
.ai-test-raw {
  margin-top: 0.6rem;
  font-size: 0.85rem;
  color: #ccc;
}
.ai-test-raw summary {
  cursor: pointer;
  color: #aaa;
  user-select: none;
}
.ai-test-raw summary:hover { color: #fff; }
.ai-test-raw pre {
  margin: 0.4rem 0 0;
  padding: 0.6rem;
  background: #0f0f0f;
  border: 1px solid #333;
  border-radius: 4px;
  max-height: 240px;
  overflow: auto;
  font: 0.8rem ui-monospace, SFMono-Regular, Menlo, monospace;
  white-space: pre-wrap;
  word-break: break-word;
}
.animal-picker { position: relative; }
.suggestions {
  display: flex;
  flex-wrap: wrap;
  gap: 0.25rem;
  margin-top: 0.3rem;
}
.suggestion {
  background: #1e1e1e;
  border: 1px solid #444;
  color: #ddd;
  border-radius: 4px;
  padding: 0.15rem 0.4rem;
  font-size: 0.85rem;
  cursor: pointer;
}
.suggestion:hover { border-color: #888; }
.hint { color: #888; font-size: 0.85rem; }
</style>
