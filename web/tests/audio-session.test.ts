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
})
