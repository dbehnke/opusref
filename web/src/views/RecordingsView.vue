<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { Page, Recording } from '../lib/types'
import StatusBadge from '../components/StatusBadge.vue'
import { BrowserAudioSession } from '../lib/audio-session'
import { useSessionStore } from '../stores/session'

const store = useSessionStore(); const recordings = ref<Recording[]>([]); const filter = ref('all'); const callsign = ref(''); const from = ref(''); const to = ref(''); const nextCursor = ref<string>(); const active = ref<string>(); const elapsed = ref(0); const loading = ref(true); const actionBusy = ref(false); const error = ref(''); const capabilityError = ref(''); const adminPassword = ref(''); let audio: BrowserAudioSession | undefined
async function load(append = false) { loading.value = true; try { const query = new URLSearchParams({ limit: '50' }); if (filter.value !== 'all') query.set('status', filter.value); if (callsign.value) query.set('callsign', callsign.value.toUpperCase()); if (from.value) query.set('from', new Date(from.value).toISOString()); if (to.value) query.set('to', new Date(to.value).toISOString()); if (append && nextCursor.value) query.set('cursor', nextCursor.value); const page = await api.request<Page<Recording>>(`/api/v1/recordings?${query}`); recordings.value = append ? [...recordings.value, ...page.items] : page.items; nextCursor.value = page.next_cursor; error.value = '' } catch { error.value = 'The recording library is unavailable.' } finally { loading.value = false } }
async function toggle(id: string) {
  if (actionBusy.value) return
  if (active.value === id && audio?.state.playback) { audio.playback(audio.state.playback.state === 'playing' ? 'playback_pause' : 'playback_resume', audio.state.playback.channelId); return }
  actionBusy.value = true
  try {
    if (!audio) { audio = new BrowserAudioSession(store.csrf); audio.addEventListener('state', () => { if (!audio) return; if (audio.state.error) error.value = audio.state.error; if (audio.state.playback) { active.value = audio.state.playback.recordingId; elapsed.value = audio.state.playback.elapsedMs } }); if (!await audio.start()) throw new Error(audio.state.error) }
    if (audio.state.playback) audio.playback('playback_close', audio.state.playback.channelId)
    audio.openPlayback(id)
  } catch { capabilityError.value = audio?.state.error ?? 'Playback could not start on this device.' }
  finally { actionBusy.value = false }
}
function seek() { if (audio?.state.playback) audio.seek(audio.state.playback.channelId, elapsed.value) }
async function remove(item: Recording) { if (!confirm(`Delete the recording from ${item.source_callsign}?`)) return; actionBusy.value = true; try { const proof = await api.request<{ reauth_token: string }>('/api/v1/me/reauth/password', { method: 'POST', csrf: store.csrf, body: { password: adminPassword.value } }); if (audio?.state.playback?.recordingId === item.id) audio.playback('playback_close', audio.state.playback.channelId); await api.request(`/api/v1/admin/recordings/${item.id}`, { method: 'DELETE', csrf: store.csrf, reauth: proof.reauth_token }); await load() } catch { error.value = 'The recording was not deleted. Check your administrator password.' } finally { actionBusy.value = false } }
onMounted(load)
onBeforeUnmount(() => void audio?.close())
</script>
<template>
  <div class="space-y-6"><header><h1 class="text-4xl font-black">Recordings</h1><p class="mt-2 text-slate-400">Play retained transmissions. Recordings expire after the configured retention period.</p></header>
    <form class="surface grid gap-4 p-5 sm:grid-cols-2 lg:grid-cols-4" aria-label="Recording filters" @submit.prevent="load(false)"><div><label class="label" for="recording-filter">Recording status</label><select id="recording-filter" v-model="filter" class="field"><option value="all">All</option><option value="complete">Complete</option><option value="partial">Partial</option></select></div><div><label class="label" for="recording-callsign">Source callsign</label><input id="recording-callsign" v-model="callsign" class="field uppercase" maxlength="10"></div><div><label class="label" for="recording-from">From</label><input id="recording-from" v-model="from" class="field" type="datetime-local"></div><div><label class="label" for="recording-to">To</label><input id="recording-to" v-model="to" class="field" type="datetime-local"></div><button class="button-secondary" type="submit" :disabled="loading">Apply filters</button><div v-if="store.isAdmin" class="w-full max-w-xs"><label class="label" for="recording-admin-password">Administrator password</label><input id="recording-admin-password" v-model="adminPassword" class="field" type="password" autocomplete="current-password"><p class="mt-1 text-sm text-slate-400">Required only for deletion.</p></div></form>
    <p v-if="loading" role="status">Loading recordings…</p><p v-if="error" role="alert" class="text-red-200">{{ error }}</p><section v-if="capabilityError" class="surface border-amber-500 p-4" role="alert"><h2 class="font-bold text-amber-200">Audio is not available</h2><p>{{ capabilityError }}</p><p class="mt-1 text-sm text-slate-400">You can continue to browse recording details and manage your account.</p></section>
    <ul v-if="recordings.length" class="space-y-4" aria-label="Recording library">
      <li v-for="item in recordings" :key="item.id" class="surface p-5"><div class="flex flex-wrap items-start justify-between gap-4"><div><div class="flex items-center gap-3"><h2 class="text-xl font-bold">{{ item.source_callsign }}</h2><StatusBadge :state="item.status" /></div><p class="mt-2 text-sm text-slate-400">{{ new Date(item.started_at).toLocaleString() }} · {{ Math.round(item.duration_ms / 1000) }} s · {{ item.end_reason }}</p></div><div class="flex gap-2"><button class="button-primary" type="button" :disabled="actionBusy || !!capabilityError" @click="toggle(item.id)">{{ active === item.id && audio?.state.playback?.state === 'playing' ? 'Pause' : 'Play' }}</button><button v-if="store.isAdmin" class="button-danger" type="button" :disabled="actionBusy || !adminPassword" @click="remove(item)">Delete</button></div></div>
        <div v-if="active === item.id" class="mt-5"><label class="label" :for="`seek-${item.id}`">Playback position: {{ Math.round(elapsed / 1000) }} seconds</label><input :id="`seek-${item.id}`" v-model.number="elapsed" class="w-full accent-cyan-300" type="range" min="0" :max="item.duration_ms" step="20" @change="seek"></div></li>
    </ul><p v-else-if="!loading && !error" class="surface p-6 text-slate-400">No recordings match these filters.</p><button v-if="nextCursor" class="button-secondary" type="button" :disabled="loading" @click="load(true)">Load more recordings</button>
  </div>
</template>
