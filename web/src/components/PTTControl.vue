<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { PTTMachine, type PTTState } from '../lib/ptt'

const props = defineProps<{ disabled?: boolean; busy?: boolean; remaining?: number; stopReason?: string }>()
const emit = defineEmits<{ request: []; stop: [] }>()
const machine = new PTTMachine()
const state = ref<PTTState>('idle')
const latched = ref(localStorage.getItem('opusref.ptt.latched') === 'true')
const announcement = ref('')
const label = computed(() => ({ idle: 'Push to talk', requesting: 'Requesting…', transmitting: latched.value ? 'Stop transmitting' : 'Transmitting', stopping: 'Stopping…' })[state.value])
const instructions = computed(() => latched.value ? 'Latch mode is on. Activate Push to talk once to start. Activate it again to stop.' : 'Hold the button to transmit. Release it to stop.')

watch(latched, (value, previous) => { localStorage.setItem('opusref.ptt.latched', String(value)); machine.latched = value; if (previous !== undefined) announcement.value = value ? 'Latch mode is on. Release will not stop transmission.' : 'Latch mode is off. Release stops transmission.' }, { immediate: true })
function run(effect: ReturnType<PTTMachine['activate']>) { state.value = machine.state; if (effect === 'request') { announcement.value = 'Requesting the reflector floor.'; emit('request') } if (effect === 'stop') { announcement.value = 'Stopping transmission.'; emit('stop') } }
function activate(event?: PointerEvent) { if (event?.currentTarget instanceof Element && event.pointerId >= 0) (event.currentTarget as HTMLElement).setPointerCapture?.(event.pointerId); if (!props.disabled && !props.busy) run(machine.activate()) }
function release() { run(machine.release()) }
function keydown(event: KeyboardEvent) { if (event.code === 'Space' && !event.repeat) { event.preventDefault(); activate() } }
function keyup(event: KeyboardEvent) { if (event.code === 'Space') { event.preventDefault(); release() } }
function safetyStop() { if (document.visibilityState === 'hidden') run(machine.safetyStop()) }
function granted() { machine.granted(); state.value = machine.state; announcement.value = 'Transmission started.' }
function ended() { machine.ended(); state.value = machine.state; announcement.value = 'Transmission stopped.' }
defineExpose({ granted, ended, safetyStop })
onMounted(() => { document.addEventListener('visibilitychange', safetyStop); window.addEventListener('pagehide', safetyStop) })
onBeforeUnmount(() => { safetyStop(); document.removeEventListener('visibilitychange', safetyStop); window.removeEventListener('pagehide', safetyStop) })
</script>

<template>
  <section class="surface p-5" aria-labelledby="ptt-title">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h2 id="ptt-title" class="text-lg font-bold">Talk</h2><p class="text-sm text-slate-400">The reflector permits one source at a time.</p></div>
      <label class="flex min-h-11 items-center gap-2 rounded-lg px-2"><input v-model="latched" type="checkbox" class="size-5 accent-cyan-300">Latch PTT</label>
    </div>
    <p id="ptt-instructions" class="mt-3 text-sm text-slate-300">{{ instructions }}</p>
    <button class="mt-5 min-h-16 w-full rounded-2xl border-2 border-cyan-200 bg-cyan-300 px-6 text-lg font-black text-slate-950 disabled:border-slate-600 disabled:bg-slate-700 disabled:text-slate-300"
      type="button" :disabled="disabled || busy || state === 'stopping'" aria-describedby="ptt-instructions" :aria-pressed="state === 'transmitting'" @pointerdown="activate" @pointerup="release" @pointercancel="release" @keydown="keydown" @keyup="keyup" @blur="release">
      {{ label }}<span v-if="remaining !== undefined"> · {{ remaining }} s</span>
    </button>
    <p class="sr-only" role="status" aria-live="polite">{{ announcement }}</p>
    <p class="mt-3 min-h-6 text-center" aria-live="assertive">{{ busy ? 'The channel is busy.' : stopReason }}</p>
  </section>
</template>
