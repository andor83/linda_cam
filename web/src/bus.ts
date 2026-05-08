import { ref } from 'vue'

// Bumped every time a new picture is saved to disk (manual or auto).
// Views that show pictures (Gallery) watch this and reload when it changes.
export const captureSignal = ref(0)
export function signalCapture() { captureSignal.value++ }
