import { defineStore } from 'pinia'
import { api } from '../lib/api'
import type { SessionInfo } from '../lib/types'

const anonymous: SessionInfo = { authenticated: false, passkey_available: false }

export const useSessionStore = defineStore('session', {
  state: () => ({ session: anonymous as SessionInfo, loaded: false }),
  getters: {
    authenticated: state => state.session.authenticated,
    isAdmin: state => state.session.role === 'admin',
    csrf: state => state.session.csrf_token,
  },
  actions: {
    async refresh() {
      try { this.session = await api.request<SessionInfo>('/api/v1/session') }
      catch { this.session = anonymous }
      finally { this.loaded = true }
    },
    async login(username: string, password: string) {
      this.session = await api.request<SessionInfo>('/api/v1/auth/login', { method: 'POST', body: { username, password } })
    },
    async logout() {
      await api.request('/api/v1/auth/logout', { method: 'POST', csrf: this.csrf })
      this.session = anonymous
    },
  },
})
