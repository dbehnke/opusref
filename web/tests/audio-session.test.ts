import { describe, expect, it } from 'vitest'
import { BrowserAudioSession } from '../src/lib/audio-session'
import type { OpusRefSocket } from '../src/lib/socket'
import { MediaKind } from '../src/lib/orwb'

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

  it('retires queued playback across pause, resume, seek, and close epochs', () => {
    const sent: { id: string; type: string; body: unknown }[] = []
    let request = 0
    const socket = Object.assign(new EventTarget(), { send(type: string, body: unknown) { const id = `request-${++request}`; sent.push({ id, type, body }); return id }, close() {}, get bufferedAmount() { return 0 } }) as unknown as OpusRefSocket
    const workerMessages: any[] = []; const workletMessages: any[] = []
    const audio = new BrowserAudioSession(undefined, socket)
    ;(audio as any).worker = { postMessage(message: any) { workerMessages.push(message) } }
    ;(audio as any).audioNode = { port: { postMessage(message: any) { workletMessages.push(message) } } }
    const control = (type: string, requestId: string | undefined, body: unknown) => (audio as any).onSocket({ control: { api_version: 1, type, ...(requestId ? { request_id: requestId } : {}), body } })
    const media = (sequence: number, timestamp: number) => (audio as any).onSocket({ media: { kind: MediaKind.Playback, channelId: 9n, sequence, timestamp, payload: new Uint8Array([0xf8]) } })

    audio.openPlayback('recording-1')
    control('playback_opened', 'request-1', { channel_id: '9', recording_id: 'recording-1', duration_ms: 10_000, status: 'complete' })
    media(0, 0)
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0])

    audio.playback('playback_pause', '9')
    const pauseEpoch = (audio as any).playoutEpoch
    media(1, 960)
    control('playback_state', 'request-2', { channel_id: '9', state: 'paused', elapsed_ms: 20 })
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0])

    audio.playback('playback_resume', '9')
    control('playback_state', 'request-3', { channel_id: '9', state: 'playing', elapsed_ms: 20 })
    media(1, 960)
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0, 1])

    audio.seek('9', 1000)
    media(2, 1920)
    control('playback_state', 'request-4', { channel_id: '9', state: 'playing', elapsed_ms: 1000 })
    const seekEpoch = (audio as any).playoutEpoch
    media(0, 48_000)
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0, 1, 0])
    ;(audio as any).onWorker({ type: 'pcm', epoch: pauseEpoch, pcm: new Float32Array(960) })
    ;(audio as any).onWorker({ type: 'pcm', epoch: seekEpoch, pcm: new Float32Array(960) })
    expect(workletMessages.filter(message => message.type === 'play')).toHaveLength(1)

    audio.seek('9', 2000)
    audio.seek('9', 3000)
    control('playback_state', 'request-5', { channel_id: '9', state: 'playing', elapsed_ms: 2000 })
    media(0, 96_000)
    expect(workerMessages.filter(message => message.type === 'media')).toHaveLength(3)
    control('playback_state', 'request-6', { channel_id: '9', state: 'playing', elapsed_ms: 3000 })
    media(0, 144_000)
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0, 1, 0, 0])

    audio.playback('playback_close', '9')
    media(1, 48_960)
    expect(workerMessages.filter(message => message.type === 'media').map(message => message.packet.sequence)).toEqual([0, 1, 0, 0])
  })
})
