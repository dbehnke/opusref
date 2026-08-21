<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { AudioCapability } from '../lib/capability'
import { BrowserAudioSession, type AudioSessionState } from '../lib/audio-session'
import type { PublicStatus } from '../lib/types'
import { useSessionStore } from '../stores/session'
import StatusBadge from '../components/StatusBadge.vue'
import CapabilityNotice from '../components/CapabilityNotice.vue'
import AudioControl from '../components/AudioControl.vue'
import PTTControl from '../components/PTTControl.vue'

const session = useSessionStore()
const status = ref<PublicStatus>()
const capability = ref<AudioCapability>()
const listening = ref(false)
const error = ref('')
const ptt = ref<InstanceType<typeof PTTControl>>()
let audio: BrowserAudioSession | undefined
const audioState = ref<AudioSessionState>({ connected: false, listening: false, ptt: 'idle', busy: false, activity: [], pttAvailable: false })
let timer = 0
const floorLabel = computed(() => status.value?.floor.active ? status.value.floor.source_callsign ?? 'Active source' : 'Channel idle')

async function refresh() { try { status.value = await api.request<PublicStatus>('/api/v1/public/status'); error.value = '' } catch { error.value = 'Status is temporarily unavailable.' } }
async function toggleListen() {
  if (listening.value) { await audio?.close(); audio = undefined; listening.value = false; return }
  audio = new BrowserAudioSession(session.csrf)
  audio.addEventListener('session-invalid', () => void session.refresh())
  audio.addEventListener('state', () => {
    if (!audio) return
    audioState.value = { ...audio.state }
    listening.value = audio.state.listening
    if (audio.state.error) error.value = audio.state.error
    if (audio.state.ptt === 'transmitting') ptt.value?.granted()
    if (audio.state.ptt === 'idle') ptt.value?.ended()
  })
  const supported = await audio.start()
  capability.value = supported ? { supported: true } : { supported: false, reason: audio.state.error }
}
onMounted(() => { refresh(); timer = window.setInterval(refresh, 1000) })
onBeforeUnmount(() => { clearInterval(timer); void audio?.close() })
</script>

<template>
  <div class="space-y-6">
    <header><p class="font-bold uppercase tracking-[.2em] text-cyan-300">Reflector console</p><h1 class="mt-2 text-4xl font-black tracking-tight sm:text-5xl">Live channel</h1><p class="mt-3 max-w-2xl text-slate-300">Monitor the shared channel. Start audio only when you are ready to listen.</p></header>
    <p v-if="error" class="surface border-red-500 p-4" role="alert">{{ error }}</p>
    <p v-if="status && !status.ready" class="surface border-amber-500 p-4" role="status">The reflector is not ready. Monitoring can remain available while connections recover.</p>
    <p v-if="status?.recording.quota_full" class="surface border-amber-500 p-4" role="status">Archive quota full. Live audio remains available. New recordings are stopped.</p>
    <p v-else-if="status && !status.recording.available" class="surface border-amber-500 p-4" role="status">Recording is unavailable. Live audio can remain available.</p>
    <section v-if="status" class="card-grid" aria-label="Reflector status">
      <article class="surface p-5"><p class="text-sm text-slate-400">Service</p><StatusBadge class="mt-3" :state="status.health" /></article>
      <article class="surface p-5"><p class="text-sm text-slate-400">Reflector</p><p class="mt-2 text-xl font-bold">{{ status.reflector.display_name }}</p><p class="text-sm text-slate-400">{{ status.reflector.id }}</p></article>
      <article class="surface p-5"><p class="text-sm text-slate-400">Connected clients</p><p class="mt-2 text-3xl font-black">{{ status.client_count }}</p></article>
      <article class="surface p-5"><p class="text-sm text-slate-400">Floor</p><p class="mt-2 text-xl font-bold">{{ floorLabel }}</p><p v-if="status.floor.remaining_seconds !== undefined" class="text-sm text-slate-300">{{ status.floor.remaining_seconds }} seconds remain</p></article>
    </section>
    <CapabilityNotice v-if="capability && !capability.supported" :reason="capability.reason ?? 'Audio is unavailable.'" />
    <section class="surface flex flex-wrap items-center justify-between gap-4 p-5" aria-labelledby="listen-title">
      <div><h2 id="listen-title" class="text-xl font-bold">Live audio</h2><p class="text-slate-400">Audio does not start automatically.</p></div>
      <AudioControl :active="listening" :disabled="capability?.supported === false" @toggle="toggleListen" />
    </section>
    <PTTControl v-if="session.authenticated" ref="ptt" :disabled="!listening || !audioState.pttAvailable || !capability?.supported || session.session.forced_password_change" :busy="audioState.busy || (status?.floor.active && audioState.ptt === 'idle')" :remaining="audioState.remaining" :stop-reason="audioState.error" @request="audio?.requestPTT()" @stop="audio?.stopPTT()" />
    <p v-if="session.authenticated && !session.session.source_callsign" class="surface border-amber-500 p-4" role="status">An administrator must assign a source callsign before you can talk.</p>
    <section class="surface p-5"><h2 class="text-xl font-bold">Recent activity</h2><ul v-if="audioState.activity.length" class="mt-3 space-y-2" aria-live="polite"><li v-for="item in audioState.activity" :key="item.at" class="flex flex-wrap justify-between gap-2 border-b border-slate-800 py-2"><span>{{ item.text }}</span><time class="text-sm text-slate-400">{{ new Date(item.at).toLocaleTimeString() }}</time></li></ul><p v-else class="mt-3 text-slate-400">No recent activity.</p></section>
  </div>
</template>
