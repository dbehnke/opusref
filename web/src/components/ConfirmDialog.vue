<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

defineProps<{ title: string; description: string; confirmLabel: string; busy?: boolean }>()
const emit = defineEmits<{ confirm: []; cancel: [] }>()
const dialog = ref<HTMLDialogElement>()
const cancelButton = ref<HTMLButtonElement>()
const confirmButton = ref<HTMLButtonElement>()
const backgroundState = new Map<HTMLElement, boolean>()

function setBackgroundInert() {
  const modal = dialog.value
  if (!modal) return
  for (const child of Array.from(document.body.children)) {
    if (!(child instanceof HTMLElement) || child === modal) continue
    backgroundState.set(child, child.inert)
    child.inert = true
  }
}

function restoreBackground() {
  for (const [element, inert] of backgroundState) element.inert = inert
  backgroundState.clear()
}

function containFocus(event: KeyboardEvent) {
  if (event.key === 'Escape') { event.preventDefault(); emit('cancel'); return }
  if (event.key !== 'Tab') return
  const first = cancelButton.value
  const last = confirmButton.value
  if (!first || !last) return
  const active = document.activeElement
  if (event.shiftKey && (active === first || !dialog.value?.contains(active))) { event.preventDefault(); last.focus() }
  else if (!event.shiftKey && (active === last || !dialog.value?.contains(active))) { event.preventDefault(); first.focus() }
}

function guardFocus(event: FocusEvent) {
  if (dialog.value && !dialog.value.contains(event.target as Node)) cancelButton.value?.focus()
}

onMounted(async () => {
  setBackgroundInert()
  document.addEventListener('focusin', guardFocus, true)
  if (typeof dialog.value?.showModal === 'function') dialog.value.showModal()
  else dialog.value?.setAttribute('open', '')
  await nextTick()
  cancelButton.value?.focus()
})

onBeforeUnmount(() => {
  document.removeEventListener('focusin', guardFocus, true)
  restoreBackground()
  if (dialog.value?.open && typeof dialog.value.close === 'function') dialog.value.close()
})
</script>

<template>
  <Teleport to="body">
    <dialog ref="dialog" class="m-auto max-h-[calc(100vh-2rem)] w-[calc(100%-2rem)] max-w-md overflow-auto bg-transparent p-0 text-slate-100 backdrop:bg-slate-950/80" role="alertdialog" aria-modal="true" aria-labelledby="confirm-dialog-title" aria-describedby="confirm-dialog-description" @keydown="containFocus" @cancel.prevent="$emit('cancel')">
      <section class="surface p-6">
        <h2 id="confirm-dialog-title" class="text-2xl font-bold">{{ title }}</h2>
        <p id="confirm-dialog-description" class="mt-3 text-slate-300">{{ description }}</p>
        <div class="mt-5 flex flex-wrap gap-3">
          <button ref="cancelButton" class="button-secondary" type="button" :disabled="busy" @click="$emit('cancel')">Cancel</button>
          <button ref="confirmButton" class="button-danger" type="button" :disabled="busy" @click="$emit('confirm')">{{ confirmLabel }}</button>
        </div>
      </section>
    </dialog>
  </Teleport>
</template>
