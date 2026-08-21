<script setup lang="ts">
defineProps<{ active: boolean; disabled?: boolean; label?: string }>()
defineEmits<{ toggle: [] }>()
</script>
<template>
  <button class="button-primary gap-2" type="button" :disabled="disabled" :aria-pressed="active" @click="$emit('toggle')">
    <span aria-hidden="true">{{ active ? '■' : '▶' }}</span>{{ active ? 'Stop listening' : (label ?? 'Listen live') }}
  </button>
</template>
