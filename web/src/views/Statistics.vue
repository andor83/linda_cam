<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { Chart, registerables } from 'chart.js'
import { api, type StatsBundle } from '../api'

Chart.register(...registerables)
Chart.defaults.color = '#bbb'
Chart.defaults.borderColor = '#2a2a2a'
Chart.defaults.font.family =
  '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'

const route = useRoute()
const data = ref<StatsBundle | null>(null)
const loading = ref(false)
const err = ref('')
const fetchedAt = ref<Date | null>(null)
const tickNow = ref(Date.now())

const top7dRef = ref<HTMLCanvasElement | null>(null)
const yearTrendRef = ref<HTMLCanvasElement | null>(null)
const hourRef = ref<HTMLCanvasElement | null>(null)

let top7dChart: Chart | null = null
let yearTrendChart: Chart | null = null
let hourChart: Chart | null = null
let chartsBuilt = false

// Distinct enough in dark mode for 5 lines.
const seriesColors = ['#ffb454', '#69d3ff', '#7fff7f', '#ff7fb1', '#c389ff']

async function load() {
  loading.value = true
  err.value = ''
  try {
    data.value = await api.getStats()
    fetchedAt.value = new Date()
    if (route.name === 'statistics') {
      await nextTick()
      buildCharts()
    } else {
      // Defer until the tab is shown for the first time.
      chartsBuilt = false
    }
  } catch (e: any) {
    err.value = e?.message ?? 'load failed'
  } finally {
    loading.value = false
  }
}

function destroyCharts() {
  top7dChart?.destroy(); top7dChart = null
  yearTrendChart?.destroy(); yearTrendChart = null
  hourChart?.destroy(); hourChart = null
  chartsBuilt = false
}

function buildCharts() {
  if (!data.value) return
  destroyCharts()

  if (top7dRef.value) {
    top7dChart = new Chart(top7dRef.value, {
      type: 'bar',
      data: {
        labels: data.value.top_7d.map((s) => s.name),
        datasets: [
          {
            label: 'Sightings',
            data: data.value.top_7d.map((s) => s.count),
            backgroundColor: '#2a6df4',
            borderRadius: 3,
          },
        ],
      },
      options: {
        indexAxis: 'y',
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { grid: { color: '#2a2a2a' }, ticks: { precision: 0 } },
          y: { grid: { display: false } },
        },
      },
    })
  }

  if (yearTrendRef.value) {
    const yt = data.value.year_trend
    const datasets = yt.species.map((name, i) => ({
      label: name,
      data: yt.series[i],
      borderColor: seriesColors[i % seriesColors.length],
      backgroundColor: seriesColors[i % seriesColors.length],
      borderWidth: 2,
      pointRadius: 0,
      tension: 0.25,
      fill: false,
    }))
    yearTrendChart = new Chart(yearTrendRef.value, {
      type: 'line',
      data: { labels: yt.weeks, datasets },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: { legend: { position: 'bottom' } },
        scales: {
          x: {
            grid: { color: '#2a2a2a' },
            ticks: {
              maxRotation: 0,
              autoSkip: true,
              maxTicksLimit: 12,
              callback(_v, idx) {
                const wk = (this as any).getLabelForValue(idx) as string
                if (!wk) return ''
                const d = new Date(wk + 'T00:00:00')
                return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
              },
            },
          },
          y: {
            grid: { color: '#2a2a2a' },
            ticks: { precision: 0 },
            beginAtZero: true,
          },
        },
      },
    })
  }

  if (hourRef.value) {
    hourChart = new Chart(hourRef.value, {
      type: 'bar',
      data: {
        labels: Array.from({ length: 24 }, (_, h) => `${h}:00`),
        datasets: [
          {
            label: 'Sightings',
            data: data.value.hour_of_day,
            backgroundColor: '#69d3ff',
            borderRadius: 3,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { grid: { display: false } },
          y: { grid: { color: '#2a2a2a' }, ticks: { precision: 0 }, beginAtZero: true },
        },
      },
    })
  }

  chartsBuilt = true
}

watch(
  () => route.name,
  (name) => {
    if (name === 'statistics' && data.value && !chartsBuilt) {
      nextTick(() => buildCharts())
    }
  },
)

function formatBytes(n: number | undefined): string {
  if (typeof n !== 'number' || n < 0) return '—'
  if (n < 1024) return `${n} B`
  const kb = n / 1024
  if (kb < 1024) return `${kb.toFixed(1)} KB`
  const mb = kb / 1024
  if (mb < 1024) return `${mb.toFixed(mb < 10 ? 1 : 0)} MB`
  const gb = mb / 1024
  return `${gb.toFixed(gb < 10 ? 2 : 1)} GB`
}

const ageHint = computed(() => {
  if (!fetchedAt.value) return ''
  const sec = Math.max(0, Math.floor((tickNow.value - fetchedAt.value.getTime()) / 1000))
  if (sec < 5) return 'just now'
  if (sec < 60) return `${sec}s ago`
  return `${Math.floor(sec / 60)}m ago`
})

let ageTimer: number | undefined

onMounted(async () => {
  ageTimer = window.setInterval(() => (tickNow.value = Date.now()), 1000)
  await load()
})

onUnmounted(() => {
  if (ageTimer) clearInterval(ageTimer)
  destroyCharts()
})
</script>

<template>
  <div class="container">
    <div class="page-head">
      <h1>Statistics</h1>
      <div class="head-meta">
        <span v-if="fetchedAt" class="hint">refreshed {{ ageHint }}</span>
        <button :disabled="loading" @click="load">
          {{ loading ? 'Loading…' : 'Refresh' }}
        </button>
      </div>
    </div>

    <div v-if="err" class="card error">{{ err }}</div>

    <template v-if="data">
      <div class="kpi-row">
        <div class="card kpi">
          <div class="kpi-label">Pictures</div>
          <div class="kpi-value">{{ data.totals.pictures.toLocaleString() }}</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">Sightings today</div>
          <div class="kpi-value">{{ data.totals.sightings_today.toLocaleString() }}</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">Sightings, last 7d</div>
          <div class="kpi-value">{{ data.totals.sightings_7d.toLocaleString() }}</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">Distinct species, 30d</div>
          <div class="kpi-value">{{ data.totals.species_30d.toLocaleString() }}</div>
        </div>
        <div class="card kpi">
          <div class="kpi-label">Disk used</div>
          <div class="kpi-value">{{ formatBytes(data.totals.disk_bytes) }}</div>
        </div>
      </div>

      <div class="card">
        <h2>Top species, last 7 days</h2>
        <div v-if="!data.top_7d.length" class="hint">No identified sightings yet.</div>
        <div v-else class="chart-wrap" :style="{ height: Math.max(180, data.top_7d.length * 28 + 60) + 'px' }">
          <canvas ref="top7dRef" />
        </div>
      </div>

      <div class="card">
        <h2>Year trend — top 5 species (weekly)</h2>
        <div v-if="!data.year_trend.species.length" class="hint">No identified sightings yet.</div>
        <div v-else class="chart-wrap" style="height: 320px">
          <canvas ref="yearTrendRef" />
        </div>
      </div>

      <div class="card">
        <h2>Activity by hour of day</h2>
        <div class="hint" style="margin-bottom: 0.4rem">
          When the camera most often catches identified birds (server local time).
        </div>
        <div class="chart-wrap" style="height: 240px">
          <canvas ref="hourRef" />
        </div>
      </div>
    </template>

    <div v-else-if="!err" class="card hint">Loading stats…</div>
  </div>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 1rem;
}
.head-meta {
  display: flex;
  align-items: center;
  gap: 0.8rem;
}
.kpi-row {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 1rem;
  margin-bottom: 1rem;
}
.kpi {
  margin-bottom: 0;
}
.kpi-label {
  color: #bbb;
  font-size: 0.85rem;
}
.kpi-value {
  margin-top: 0.4rem;
  font-size: 1.8rem;
  font-weight: 600;
  color: #eee;
}
.chart-wrap {
  position: relative;
  width: 100%;
}
h2 {
  margin: 0 0 0.6rem 0;
  font-size: 1rem;
  color: #eee;
}
.error {
  color: #ff6b6b;
}
</style>
