<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'

const router = useRouter()
const firstRun = ref(false)
const password = ref('')
const confirm = ref('')
const error = ref('')
const busy = ref(false)

onMounted(async () => {
  const s = await api.session()
  firstRun.value = s.first_run
  if (s.authenticated) router.replace({ name: 'live' })
})

async function submit() {
  error.value = ''
  busy.value = true
  try {
    if (firstRun.value) {
      if (password.value.length < 6) throw new Error('Password must be at least 6 characters')
      if (password.value !== confirm.value) throw new Error('Passwords do not match')
      await api.firstRunSetup(password.value)
    } else {
      await api.login(password.value)
    }
    window.location.reload()
  } catch (e: any) {
    error.value = e?.message ?? 'Login failed'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <div class="card">
      <h2>{{ firstRun ? 'Set up Linda_Cam' : 'Linda_Cam Login' }}</h2>
      <p v-if="firstRun">Choose a password for this device. You'll use it every time you log in.</p>
      <form @submit.prevent="submit">
        <div class="form-row">
          <label>Password</label>
          <input v-model="password" type="password" autofocus autocomplete="current-password" />
        </div>
        <div class="form-row" v-if="firstRun">
          <label>Confirm password</label>
          <input v-model="confirm" type="password" autocomplete="new-password" />
        </div>
        <button :disabled="busy" type="submit">{{ firstRun ? 'Create password' : 'Log in' }}</button>
        <div class="error" v-if="error">{{ error }}</div>
      </form>
    </div>
  </div>
</template>
