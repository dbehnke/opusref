<script setup lang="ts">
defineProps<{ reason: string }>()
</script>
<template>
  <section class="surface border-amber-500/70 p-5" role="alert" aria-labelledby="capability-title">
    <h2 id="capability-title" class="text-lg font-bold text-amber-200">Audio is not available</h2>
    <p class="mt-2 text-slate-200">{{ reason }}</p>
    <p class="mt-2 text-sm text-slate-400">You can continue to use monitoring and account controls.</p>
  </section>
</template>
