<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useSessionStore } from '../stores/session'
import { api } from '../lib/api'
import { assertionOptions, serializeCredential, type CeremonyOptions } from '../lib/passkeys'
import type { SessionInfo } from '../lib/types'

const username = ref(''); const password = ref(''); const busy = ref(false); const error = ref('')
const session = useSessionStore(); const route = useRoute(); const router = useRouter()
async function submit() { busy.value = true; error.value = ''; try { await session.login(username.value, password.value); await router.replace(typeof route.query.next === 'string' ? route.query.next : '/') } catch { error.value = 'Sign-in failed. Check your credentials and try again.' } finally { busy.value = false } }
async function passkey() {
  busy.value = true; error.value = ''
  try {
    const options = await api.request<CeremonyOptions>('/api/v1/auth/passkey/options', { method: 'POST', body: {} })
    const credential = await navigator.credentials.get(assertionOptions(options)) as PublicKeyCredential | null
    if (!credential) throw new Error('No credential')
    session.session = await api.request<SessionInfo>('/api/v1/auth/passkey/verify', { method: 'POST', body: { ceremony_id: options.ceremony_id, credential: serializeCredential(credential) } })
    await router.replace(typeof route.query.next === 'string' ? route.query.next : '/')
  } catch { error.value = 'Sign-in failed. Check your credentials and try again.' }
  finally { busy.value = false }
}
</script>
<template>
  <section class="surface mx-auto max-w-md p-6 sm:p-8" aria-labelledby="login-title">
    <h1 id="login-title" class="text-3xl font-black">Sign in</h1><p class="mt-2 text-slate-400">Sign in to talk or play recordings.</p>
    <form class="mt-6 space-y-5" @submit.prevent="submit">
      <div><label class="label" for="username">Username</label><input id="username" v-model="username" class="field" autocomplete="username" required></div>
      <div><label class="label" for="password">Password</label><input id="password" v-model="password" class="field" type="password" autocomplete="current-password" required></div>
      <p v-if="error" role="alert" class="text-red-200">{{ error }}</p>
      <button class="button-primary w-full" type="submit" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
      <button v-if="session.session.passkey_available" class="button-secondary w-full" type="button" @click="passkey">Use a passkey</button>
    </form>
  </section>
</template>
