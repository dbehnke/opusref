<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { Recording } from '../lib/types'
import StatusBadge from '../components/StatusBadge.vue'

const recordings = ref<Recording[]>([]); const filter = ref('all'); const active = ref<string>(); const elapsed = ref(0); const loading = ref(true); const error = ref('')
async function load() { try { recordings.value = await api.request<Recording[]>(`/api/v1/recordings?status=${filter.value}`) } catch { error.value = 'The recording library is unavailable.' } finally { loading.value = false } }
function toggle(id: string) { active.value = active.value === id ? undefined : id }
onMounted(load)
</script>
<template>
  <div class="space-y-6"><header><h1 class="text-4xl font-black">Recordings</h1><p class="mt-2 text-slate-400">Play retained transmissions. Recordings expire after the configured retention period.</p></header>
    <div class="max-w-xs"><label class="label" for="recording-filter">Recording status</label><select id="recording-filter" v-model="filter" class="field" @change="load"><option value="all">All</option><option value="complete">Complete</option><option value="partial">Partial</option></select></div>
    <p v-if="loading" role="status">Loading recordings…</p><p v-if="error" role="alert" class="text-red-200">{{ error }}</p>
    <ul v-if="recordings.length" class="space-y-4" aria-label="Recording library">
      <li v-for="item in recordings" :key="item.id" class="surface p-5"><div class="flex flex-wrap items-start justify-between gap-4"><div><div class="flex items-center gap-3"><h2 class="text-xl font-bold">{{ item.source_callsign }}</h2><StatusBadge :state="item.status" /></div><p class="mt-2 text-sm text-slate-400">{{ new Date(item.started_at).toLocaleString() }} · {{ Math.round(item.duration_ms / 1000) }} s · {{ item.end_reason }}</p></div><button class="button-primary" type="button" @click="toggle(item.id)">{{ active === item.id ? 'Pause' : 'Play' }}</button></div>
        <div v-if="active === item.id" class="mt-5"><label class="label" :for="`seek-${item.id}`">Playback position: {{ Math.round(elapsed / 1000) }} seconds</label><input :id="`seek-${item.id}`" v-model.number="elapsed" class="w-full accent-cyan-300" type="range" min="0" :max="item.duration_ms" step="20"></div></li>
    </ul><p v-else-if="!loading && !error" class="surface p-6 text-slate-400">No recordings match this filter.</p>
  </div>
</template>
