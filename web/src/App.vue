<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { api } from './api'
import Live from './views/Live.vue'
import Gallery from './views/Gallery.vue'
import Detections from './views/Detections.vue'
import Statistics from './views/Statistics.vue'
import Settings from './views/Settings.vue'
import Login from './views/Login.vue'

const router = useRouter()
const route = useRoute()
const authed = ref(false)
const firstRun = ref(false)
const loaded = ref(false)

async function logout() {
  await api.logout()
  authed.value = false
  router.replace({ name: 'login' })
}

onMounted(async () => {
  try {
    const s = await api.session()
    firstRun.value = s.first_run
    authed.value = s.authenticated
    if (!authed.value && route.name !== 'login') {
      router.replace({ name: 'login' })
    } else if (authed.value && route.name === 'login') {
      router.replace({ name: 'live' })
    }
  } finally {
    loaded.value = true
  }
})
</script>

<template>
  <div v-if="!loaded" class="container">Loading…</div>
  <template v-else>
    <template v-if="authed">
      <nav class="nav">
        <div class="brand">Linda_Cam</div>
        <router-link :to="{ name: 'live' }" active-class="active">Live</router-link>
        <router-link :to="{ name: 'gallery' }" active-class="active">Gallery</router-link>
        <router-link :to="{ name: 'detections' }" active-class="active">Detections</router-link>
        <router-link :to="{ name: 'statistics' }" active-class="active">Statistics</router-link>
        <router-link :to="{ name: 'settings' }" active-class="active">Settings</router-link>
        <div class="spacer" />
        <button class="secondary" @click="logout">Log out</button>
      </nav>
      <!-- All authed views stay mounted; CSS hides inactive tabs so the
           live stream in Live.vue keeps buffering/playing in the background. -->
      <div v-show="route.name === 'live'"><Live /></div>
      <div v-show="route.name === 'gallery'"><Gallery /></div>
      <div v-show="route.name === 'detections'"><Detections /></div>
      <div v-show="route.name === 'statistics'"><Statistics /></div>
      <div v-show="route.name === 'settings'"><Settings /></div>
    </template>
    <Login v-else />
  </template>
</template>
