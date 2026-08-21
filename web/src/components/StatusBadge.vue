<script setup lang="ts">
defineProps<{ state: 'ok' | 'degraded' | 'unavailable' | 'complete' | 'partial'; label?: string }>()
</script>
<template>
  <span class="badge" :class="state === 'ok' || state === 'complete' ? 'border-emerald-400 text-emerald-300' : state === 'degraded' || state === 'partial' ? 'border-amber-400 text-amber-200' : 'border-red-400 text-red-200'">
    <span aria-hidden="true" class="mr-1.5">●</span>{{ label ?? state }}
  </span>
</template>
