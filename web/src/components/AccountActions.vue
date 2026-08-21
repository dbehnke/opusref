<script setup lang="ts">
import type { Account } from '../lib/types'

defineProps<{ account: Account; busy: boolean; authorized: boolean }>()
defineEmits<{ edit: []; toggle: []; revoke: []; clear: []; remove: [] }>()
</script>

<template>
  <div class="flex flex-wrap gap-2" aria-label="Account actions">
    <button class="button-secondary" type="button" @click="$emit('edit')">Edit</button>
    <button class="button-secondary" type="button" :disabled="busy || !authorized" @click="$emit('toggle')">{{ account.disabled ? 'Enable' : 'Disable' }}</button>
    <button class="button-secondary" type="button" :disabled="busy || !authorized" @click="$emit('revoke')">Revoke sessions</button>
    <button class="button-secondary" type="button" :disabled="busy || !authorized" @click="$emit('clear')">Clear passkeys</button>
    <button class="button-danger" type="button" :disabled="busy || !authorized" @click="$emit('remove')">Delete</button>
  </div>
</template>
