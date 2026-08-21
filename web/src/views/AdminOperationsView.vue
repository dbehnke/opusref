<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../lib/api'
import type { AuditEvent, OperatorEvent, Page, PublicStatus } from '../lib/types'

interface ClientSummary { node_callsign: string; source_callsign?: string; connected_at: string; last_active_at: string }
const status = ref<PublicStatus>()
const clients = ref<ClientSummary[]>([])
const audit = ref<AuditEvent[]>([])
const events = ref<OperatorEvent[]>([])
const clientCursor = ref<string>()
const auditCursor = ref<string>()
const loading = ref(true)
const eventsLoading = ref(true)
const error = ref('')
const eventsError = ref('')

async function loadCore() {
  loading.value = true
  try {
    const [nextStatus, nextClients, nextAudit] = await Promise.all([
      api.request<PublicStatus>('/api/v1/public/status'),
      api.request<Page<ClientSummary>>('/api/v1/admin/clients?limit=50'),
      api.request<Page<AuditEvent>>('/api/v1/admin/audit?limit=50'),
    ])
    status.value = nextStatus
    clients.value = nextClients.items
    clientCursor.value = nextClients.next_cursor
    audit.value = nextAudit.items
    auditCursor.value = nextAudit.next_cursor
    error.value = ''
  } catch { error.value = 'Status, client, or audit details are unavailable. Select Refresh to try again.' }
  finally { loading.value = false }
}

async function loadEvents() {
  eventsLoading.value = true
  try { events.value = (await api.request<{ items: OperatorEvent[] }>('/api/v1/admin/events')).items; eventsError.value = '' }
  catch { eventsError.value = 'Operator alerts are unavailable. Select Refresh to try again.' }
  finally { eventsLoading.value = false }
}

async function load() { await Promise.all([loadCore(), loadEvents()]) }
async function moreClients() { if (!clientCursor.value) return; const page = await api.request<Page<ClientSummary>>(`/api/v1/admin/clients?limit=50&cursor=${encodeURIComponent(clientCursor.value)}`); clients.value.push(...page.items); clientCursor.value = page.next_cursor }
async function moreAudit() { if (!auditCursor.value) return; const page = await api.request<Page<AuditEvent>>(`/api/v1/admin/audit?limit=50&cursor=${encodeURIComponent(auditCursor.value)}`); audit.value.push(...page.items); auditCursor.value = page.next_cursor }
onMounted(load)
</script>

<template>
  <div class="space-y-6">
    <header class="flex flex-wrap items-end justify-between gap-3"><div><h1 class="text-4xl font-black">Operations</h1><p class="mt-2 text-slate-400">Review storage, clients, alerts, and security actions.</p></div><button class="button-secondary" type="button" :disabled="loading || eventsLoading" @click="load">Refresh</button></header>
    <p v-if="loading" role="status">Loading operator details…</p><p v-if="error" role="alert" class="text-red-200">{{ error }}</p>
    <section v-if="status" class="card-grid"><article class="surface p-5"><p class="text-sm text-slate-400">Recorder</p><p class="mt-2 text-xl font-bold">{{ status.recording.available ? 'Available' : 'Unavailable' }}</p></article><article class="surface p-5"><p class="text-sm text-slate-400">Archive quota</p><p class="mt-2 text-xl font-bold">{{ status.recording.quota_full ? 'Full' : 'Available' }}</p></article><article class="surface p-5 sm:col-span-2"><p class="text-sm text-slate-400">Service readiness</p><p class="mt-2 text-xl font-bold">{{ status.ready ? 'Ready' : 'Not ready' }}</p></article></section>

    <section class="surface p-6" aria-labelledby="operator-alerts-title"><div class="flex flex-wrap items-baseline justify-between gap-2"><h2 id="operator-alerts-title" class="text-2xl font-bold">Operator alerts</h2><p class="text-sm text-slate-400">Newest first · Up to 256 recent alerts</p></div><p v-if="eventsLoading" class="mt-4" role="status">Loading operator alerts…</p><p v-else-if="eventsError" class="mt-4 text-red-200" role="alert">{{ eventsError }}</p><ul v-else-if="events.length" class="mt-4 divide-y divide-slate-700" aria-live="polite"><li v-for="event in events" :key="event.id" class="grid gap-2 py-4 sm:grid-cols-[8rem_1fr_auto]"><div><span class="badge" :class="event.severity === 'error' ? 'border-red-400 text-red-200' : event.severity === 'warning' ? 'border-amber-400 text-amber-200' : 'border-cyan-400 text-cyan-200'">{{ event.severity }}</span><p class="mt-2 text-xs uppercase tracking-wide text-slate-400">{{ event.kind.replaceAll('_', ' ') }}</p></div><p>{{ event.message }}</p><time class="text-sm text-slate-400" :datetime="event.time">{{ new Date(event.time).toLocaleString() }}</time></li></ul><p v-else class="mt-4 text-slate-400">No operator alerts are available.</p></section>

    <section class="surface overflow-x-auto p-6"><h2 class="text-2xl font-bold">Connected clients</h2><table class="mt-4 w-full min-w-[40rem] text-left"><thead><tr class="border-b border-slate-600"><th class="p-3">Node callsign</th><th class="p-3">Source callsign</th><th class="p-3">Connected</th><th class="p-3">Last active</th></tr></thead><tbody><tr v-for="client in clients" :key="client.node_callsign + client.connected_at" class="border-b border-slate-800"><td class="p-3">{{ client.node_callsign }}</td><td class="p-3">{{ client.source_callsign ?? 'Idle' }}</td><td class="p-3">{{ new Date(client.connected_at).toLocaleString() }}</td><td class="p-3">{{ new Date(client.last_active_at).toLocaleString() }}</td></tr><tr v-if="!clients.length"><td class="p-3 text-slate-400" colspan="4">No clients are connected.</td></tr></tbody></table></section>
    <section class="surface overflow-x-auto p-6"><h2 class="text-2xl font-bold">Audit events</h2><table class="mt-4 w-full min-w-[36rem] text-left"><thead><tr class="border-b border-slate-600"><th class="p-3">Time</th><th class="p-3">Action</th><th class="p-3">Result</th><th class="p-3">Actor</th></tr></thead><tbody><tr v-for="event in audit" :key="event.id" class="border-b border-slate-800"><td class="p-3">{{ new Date(event.occurred_at).toLocaleString() }}</td><td class="p-3">{{ event.action }}</td><td class="p-3">{{ event.result }}</td><td class="p-3">{{ event.actor ?? 'System' }}</td></tr><tr v-if="!audit.length"><td class="p-3 text-slate-400" colspan="4">No audit events are available.</td></tr></tbody></table></section>
    <div class="flex flex-wrap gap-3"><button v-if="clientCursor" class="button-secondary" type="button" @click="moreClients">Load more clients</button><button v-if="auditCursor" class="button-secondary" type="button" @click="moreAudit">Load more audit events</button></div>
  </div>
</template>
