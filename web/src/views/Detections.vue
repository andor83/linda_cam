<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, type DetectionLogEntry } from '../api'

const PAGE_SIZE = 50
const REFRESH_MS = 5000

const entries = ref<DetectionLogEntry[]>([])
const loading = ref(false)
const err = ref('')
// cursorStack holds the `before` id we used to load each page after page 1.
// Length 0 => we're on page 1 (latest entries). Length N => on page N+1.
const cursorStack = ref<number[]>([])
// Whether the current page hit the end of the log (fewer than PAGE_SIZE rows).
const atEnd = ref(false)

const pageNumber = computed(() => cursorStack.value.length + 1)
const isFirstPage = computed(() => cursorStack.value.length === 0)
const canGoNext = computed(() => !atEnd.value && entries.value.length === PAGE_SIZE)

let refreshTimer: number | undefined

async function fetchPage(before?: number) {
  loading.value = true
  err.value = ''
  try {
    const rows = await api.detections({ limit: PAGE_SIZE, before })
    entries.value = rows
    atEnd.value = rows.length < PAGE_SIZE
  } catch (e: any) {
    err.value = e?.message ?? 'load failed'
  } finally {
    loading.value = false
  }
}

async function reloadCurrent() {
  const before = cursorStack.value.length > 0
    ? cursorStack.value[cursorStack.value.length - 1]
    : undefined
  await fetchPage(before)
}

async function goNext() {
  if (!canGoNext.value || loading.value) return
  const last = entries.value[entries.value.length - 1]
  if (!last) return
  cursorStack.value.push(last.id)
  await fetchPage(last.id)
}

async function goPrev() {
  if (isFirstPage.value || loading.value) return
  cursorStack.value.pop()
  await reloadCurrent()
}

async function goFirst() {
  if (isFirstPage.value) return
  cursorStack.value = []
  await fetchPage(undefined)
}

function formatTs(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString()
}

onMounted(() => {
  fetchPage(undefined)
  refreshTimer = window.setInterval(() => {
    // Auto-refresh only when the user is viewing the latest page; deeper
    // pages are historical and don't benefit from re-fetching.
    if (isFirstPage.value && !loading.value) reloadCurrent()
  }, REFRESH_MS)
})

onUnmounted(() => {
  if (refreshTimer) window.clearInterval(refreshTimer)
})
</script>

<template>
  <div class="container">
    <div class="card">
      <h2>Detection log</h2>
      <div v-if="err" class="error">{{ err }}</div>
      <table class="log" v-if="entries.length > 0">
        <thead>
          <tr>
            <th>When</th>
            <th>Animals</th>
            <th>Top</th>
            <th>Picture</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in entries" :key="e.id">
            <td>{{ formatTs(e.timestamp) }}</td>
            <td>{{ e.classes.join(', ') }}</td>
            <td>{{ e.top_class }} ({{ (e.top_confidence * 100).toFixed(0) }}%)</td>
            <td>
              <a v-if="e.picture" :href="`/api/pictures/${encodeURIComponent(e.picture)}`" target="_blank">
                {{ e.picture }}
              </a>
              <span v-else class="hint">—</span>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-else-if="!loading" class="hint">No detections yet.</div>
      <div class="pager">
        <button class="secondary" :disabled="isFirstPage || loading" @click="goFirst" title="Jump to latest">« First</button>
        <button class="secondary" :disabled="isFirstPage || loading" @click="goPrev">‹ Newer</button>
        <span class="page-label">Page {{ pageNumber }}</span>
        <button class="secondary" :disabled="!canGoNext || loading" @click="goNext">Older ›</button>
        <div class="spacer" style="flex:1" />
        <span v-if="loading" class="hint">Loading…</span>
        <span v-else-if="isFirstPage" class="hint">Auto-refresh: 5s</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
table.log {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.9rem;
}
table.log th, table.log td {
  text-align: left;
  padding: 0.35rem 0.5rem;
  border-bottom: 1px solid #333;
}
table.log th {
  color: #aaa;
  font-weight: 500;
}
.hint { color: #888; }
.pager {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  margin-top: 0.75rem;
}
.page-label {
  font-size: 0.9rem;
  color: #bbb;
  padding: 0 0.5rem;
}
</style>
