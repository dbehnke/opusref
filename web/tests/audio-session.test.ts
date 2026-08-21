import { describe, expect, it } from 'vitest'
import { BrowserAudioSession } from '../src/lib/audio-session'
import type { OpusRefSocket } from '../src/lib/socket'

describe('browser audio lifecycle', () => {
  it('stops PTT on pagehide even when visibility remains visible', async () => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
    const sent: { type: string; body: unknown }[] = []
    const socket = Object.assign(new EventTarget(), { send(type: string, body: unknown) { sent.push({ type, body }); return 'request' }, close() {}, get bufferedAmount() { return 0 } }) as unknown as OpusRefSocket
    const audio = new BrowserAudioSession(undefined, socket)
    audio.state.ptt = 'transmitting'
    ;(audio as any).pttChannel = 18446744073709551615n
    window.dispatchEvent(new PageTransitionEvent('pagehide'))
    await Promise.resolve()
    expect(audio.state.ptt).toBe('idle')
    expect(audio.state.listening).toBe(false)
    expect(sent).toContainEqual({ type: 'ptt_stop', body: { channel_id: '18446744073709551615' } })
  })

  it('pauses at the last accepted playback position and resumes only on request', () => {
    const sent: { type: string; body: unknown }[] = []
    const socket = Object.assign(new EventTarget(), { send(type: string, body: unknown) { sent.push({ type, body }); return 'request' }, close() {}, get bufferedAmount() { return 0 } }) as unknown as OpusRefSocket
    const audio = new BrowserAudioSession(undefined, socket)
    audio.state.playback = { channelId: '18446744073709551615', recordingId: 'recording-1', state: 'playing', elapsedMs: 900, durationMs: 5000, status: 'complete' }
    ;(audio as any).onSocket({ control: { api_version: 1, type: 'playback_state', body: { channel_id: '18446744073709551615', state: 'paused', elapsed_ms: 960 } } })
    expect(audio.state.playback).toMatchObject({ state: 'paused', elapsedMs: 960 })
    expect(sent).toEqual([])
    audio.playback('playback_resume', '18446744073709551615')
    expect(sent).toEqual([{ type: 'playback_resume', body: { channel_id: '18446744073709551615' } }])
  })
})
