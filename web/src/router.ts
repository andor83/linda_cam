import { createRouter, createWebHashHistory } from 'vue-router'
import Login from './views/Login.vue'
import Live from './views/Live.vue'
import Gallery from './views/Gallery.vue'
import Settings from './views/Settings.vue'
import Detections from './views/Detections.vue'
import Statistics from './views/Statistics.vue'

export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: { name: 'live' } },
    { path: '/login', name: 'login', component: Login },
    { path: '/live', name: 'live', component: Live },
    { path: '/gallery', name: 'gallery', component: Gallery },
    { path: '/detections', name: 'detections', component: Detections },
    { path: '/statistics', name: 'statistics', component: Statistics },
    { path: '/settings', name: 'settings', component: Settings },
  ],
})
