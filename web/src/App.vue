<script setup lang="ts">
import { onMounted } from 'vue'
import { RouterLink, RouterView } from 'vue-router'
import { useSessionStore } from './stores/session'

const session = useSessionStore()
onMounted(() => session.refresh())
</script>

<template>
  <div class="min-h-screen">
    <header class="border-b border-slate-700/80 bg-slate-950/70 backdrop-blur">
      <nav aria-label="Main navigation" class="mx-auto flex max-w-7xl flex-wrap items-center gap-2 px-4 py-3 sm:px-6">
        <RouterLink class="mr-auto inline-flex min-h-11 min-w-11 items-center text-xl font-black tracking-tight text-white" :to="session.session.forced_password_change ? '/security' : '/'" aria-label="OpusRef">Opus<span class="text-cyan-300">Ref</span></RouterLink>
        <RouterLink v-if="!session.session.forced_password_change" class="button-secondary" to="/">Live</RouterLink>
        <RouterLink v-if="session.authenticated && !session.session.forced_password_change" class="button-secondary" to="/recordings">Recordings</RouterLink>
        <RouterLink v-if="session.authenticated" class="button-secondary" to="/security">Security</RouterLink>
        <RouterLink v-if="session.isAdmin && !session.session.forced_password_change" class="button-secondary" to="/admin/accounts">Accounts</RouterLink>
        <RouterLink v-if="session.isAdmin && !session.session.forced_password_change" class="button-secondary" to="/admin/operations">Operations</RouterLink>
        <RouterLink v-if="!session.authenticated" class="button-primary" to="/login">Sign in</RouterLink>
        <button v-else class="button-secondary" type="button" @click="session.logout()">Sign out</button>
      </nav>
    </header>
    <main id="main-content" class="mx-auto max-w-7xl px-4 py-8 sm:px-6" tabindex="-1">
      <RouterView />
    </main>
  </div>
</template>
