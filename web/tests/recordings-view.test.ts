import { fireEvent, render, screen, waitFor } from '@testing-library/vue'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import RecordingsView from '../src/views/RecordingsView.vue'
import { useSessionStore } from '../src/stores/session'

const fixture = vi.hoisted(() => ({ starts: 0, opened: [] as string[] }))
vi.mock('../src/lib/api', () => ({
  api: { request: vi.fn(async (path: string) => path.includes('?')
    ? { items: [{ id: 'rec-1', source_callsign: 'N0CALL', started_at: '2026-08-20T12:00:00Z', duration_ms: 5000, status: 'complete', end_reason: 'stream_end' }] }
    : { id: 'rec-1', source_callsign: 'N0CALL', started_at: '2026-08-20T12:00:00Z', duration_ms: 5000, status: 'complete', end_reason: 'stream_end' }), },
}))
vi.mock('../src/lib/audio-session', () => ({
  BrowserAudioSession: class extends EventTarget {
    state: any = { connected: false, playback: undefined }
    async start() { fixture.starts++; if (fixture.starts === 1) { await new Promise(resolve => setTimeout(resolve, 20)); this.state.error = 'The live connection did not become ready. Select Play or Retry playback to reconnect.'; return false } this.state.connected = true; return true }
    openPlayback(id: string) { fixture.opened.push(id) }
    playback() {}
    seek() {}
    async close() {}
  },
}))

describe('recording playback recovery', () => {
  beforeEach(() => { fixture.starts = 0; fixture.opened.length = 0 })

  it('keeps Play and Retry available after the first connection is not ready', async () => {
    const pinia = createPinia()
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/recordings', component: RecordingsView }, { path: '/login', component: { template: '<p>Login</p>' } }] })
    await router.push('/recordings'); await router.isReady()
    const session = useSessionStore(pinia)
    session.session = { authenticated: true, username: 'listener', role: 'user', csrf_token: 'csrf', passkey_available: false }
    session.loaded = true
    render(RecordingsView, { global: { plugins: [pinia, router] } })
    await screen.findByText('N0CALL')
    await fireEvent.click(screen.getByRole('button', { name: 'Play' }))
    expect(await screen.findByRole('heading', { name: 'Playback did not connect' })).toBeVisible()
    expect(screen.getByRole('button', { name: 'Play' })).toBeEnabled()
    await fireEvent.click(screen.getByRole('button', { name: 'Retry playback' }))
    await waitFor(() => expect(fixture.opened).toEqual(['rec-1']))
    expect(screen.queryByRole('heading', { name: 'Playback did not connect' })).not.toBeInTheDocument()
  })
})
