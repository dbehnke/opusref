import { MediaKind } from './orwb'
import { OpusRefSocket, type SocketEvent } from './socket'
import { parseChannelId } from './channel-id'
import type { PublicStatus } from './types'

export interface AudioSessionState {
  connected: boolean
  listening: boolean
  ptt: 'idle' | 'requesting' | 'transmitting' | 'stopping'
  busy: boolean
  remaining?: number
  error?: string
  capabilityError?: string
  playback?: { channelId: string; state: 'playing' | 'paused'; elapsedMs: number; recordingId?: string; durationMs?: number; status?: 'complete' | 'partial' }
  currentSource?: string
  activity: { text: string; at: string }[]
  pttAvailable: boolean
  status?: PublicStatus
}

export class BrowserAudioSession extends EventTarget {
  readonly state: AudioSessionState = { connected: false, listening: false, ptt: 'idle', busy: false, activity: [], pttAvailable: false }
  private readonly socket: OpusRefSocket
  private worker?: Worker
  private context?: AudioContext
  private audioNode?: AudioWorkletNode
  private media?: MediaStream
  private pttChannel?: bigint
  private liveChannel?: bigint
  private retired = new Set<bigint>()
  private cancelPending = false
  private totTimer = 0
  private playbackTimer = 0
  private playoutEpoch = 0
  private playbackAccepting = false
  private playbackSequenceAccounting = false
  private playbackExpectedSequence?: number
  private readonly playbackRequests = new Map<string, { action: 'open' | 'pause' | 'resume' | 'seek' | 'close'; epoch: number }>()
  private capabilityResolve?: (supported: boolean) => void
  private readinessResolve?: (ready: boolean) => void
  private closing = false
  private readonly visibilityStop = () => { if (document.visibilityState === 'hidden') void this.close() }
  private readonly pageHideStop = () => { void this.close() }

  constructor(private readonly csrf?: string, socket = new OpusRefSocket()) {
    super()
    this.socket = socket
    this.socket.addEventListener('event', event => this.onSocket((event as CustomEvent<SocketEvent>).detail))
    document.addEventListener('visibilitychange', this.visibilityStop)
    window.addEventListener('pagehide', this.pageHideStop)
  }

  private update(change: Partial<AudioSessionState>) { Object.assign(this.state, change); this.dispatchEvent(new Event('state')) }
  private activity(text: string) { this.update({ activity: [{ text, at: new Date().toISOString() }, ...this.state.activity].slice(0, 20) }) }
  private retire(id: bigint) { this.retired.add(id); while (this.retired.size > 128) this.retired.delete(this.retired.values().next().value!) }

  async start(): Promise<boolean> {
    this.closing = false
    this.worker = new Worker(new URL('../workers/audio.worker.ts', import.meta.url), { type: 'module' })
    this.worker.addEventListener('message', event => this.onWorker(event.data))
    const supported = await new Promise<boolean>(resolve => { this.capabilityResolve = resolve; this.worker!.postMessage({ type: 'capability' }); setTimeout(() => resolve(false), 5000) })
    if (!supported) { this.worker.terminate(); this.worker = undefined; const message = 'This browser does not support raw Opus audio.'; this.update({ error: message, capabilityError: message }); return false }
    this.context = new AudioContext({ sampleRate: 48000 })
    if (this.context.sampleRate !== 48000) { await this.context.close(); this.worker.terminate(); const message = 'This device cannot use a 48 kHz audio context.'; this.update({ error: message, capabilityError: message }); return false }
    await this.context.audioWorklet.addModule('/opusref-audio-worklet.js')
    this.audioNode = new AudioWorkletNode(this.context, 'opusref-audio', { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] })
    this.audioNode.connect(this.context.destination)
    this.audioNode.port.addEventListener('message', event => {
      if (event.data.type === 'capture') { this.worker?.postMessage({ type: 'socket-buffer', bytes: this.socket.bufferedAmount }); this.worker?.postMessage({ type: 'pcm', pcm: event.data.pcm }, [event.data.pcm.buffer]) }
      if (event.data.type === 'capture-overflow') { this.update({ error: 'PTT stopped because the capture queue was full.' }); this.stopPTT() }
      if (event.data.type === 'playback-overflow' && event.data.epoch === this.playoutEpoch) this.pausePlaybackForOverload('Playback paused because the device audio queue was full.')
    })
    this.audioNode.port.start()
    const ready = new Promise<boolean>(resolve => {
      this.readinessResolve = resolve
      window.setTimeout(() => { if (this.readinessResolve === resolve) { this.readinessResolve = undefined; resolve(false) } }, 5000)
    })
    this.socket.connect({ encoder: true, decoder: true, context_rate: 48000 }, this.csrf)
    this.update({ listening: true, error: undefined, capabilityError: undefined })
    if (await ready) return true
    this.closing = true
    this.socket.close()
    this.update({ listening: false, connected: false, error: 'The live connection did not become ready. Select Play or Retry playback to reconnect.' })
    return false
  }

  async requestPTT(): Promise<void> {
    if (!this.state.connected) { this.update({ error: 'The live connection is not ready.' }); return }
    this.cancelPending = false; this.update({ ptt: 'requesting', busy: false, error: undefined })
    this.send('ptt_start', {})
  }

  stopPTT(): void {
    if (this.pttChannel) this.send('ptt_stop', { channel_id: this.pttChannel.toString() }); else this.cancelPending = true
    this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.update({ ptt: 'stopping' })
  }

  openPlayback(recordingId: string): void { this.playbackBarrier(); this.playbackExpectedSequence = undefined; this.sendPlayback('playback_open', 'open', { recording_id: recordingId }) }
  playback(type: 'playback_pause' | 'playback_resume' | 'playback_close', channelId: string): void {
    this.playbackBarrier()
    const action = type === 'playback_pause' ? 'pause' : type === 'playback_resume' ? 'resume' : 'close'
    const sent = this.sendPlayback(type, action, { channel_id: channelId })
    if (type === 'playback_pause' && sent) this.playbackSequenceAccounting = true
    if (type === 'playback_pause' && this.state.playback) { clearInterval(this.playbackTimer); this.update({ playback: { ...this.state.playback, state: 'paused' } }) }
    if (type === 'playback_close') this.closePlayback()
  }
  seek(channelId: string, elapsedMs: number): void {
    const target = Math.max(0, Math.round(elapsedMs))
    this.playbackBarrier()
    if (this.state.playback) { clearInterval(this.playbackTimer); this.update({ playback: { ...this.state.playback, state: 'paused', elapsedMs: target } }) }
    this.sendPlayback('playback_seek', 'seek', { channel_id: channelId, elapsed_ms: target })
  }

  async close(): Promise<void> {
    this.closing = true
    if (this.state.ptt !== 'idle') this.stopPTT()
    if (this.state.playback) this.closePlayback()
    clearInterval(this.totTimer)
    clearInterval(this.playbackTimer)
    this.socket.close(); this.worker?.terminate(); this.audioNode?.disconnect(); this.stopMicrophone(); await this.context?.close()
    document.removeEventListener('visibilitychange', this.visibilityStop)
    window.removeEventListener('pagehide', this.pageHideStop)
    this.update({ connected: false, listening: false, ptt: 'idle' })
  }

  private async startMicrophone(channelId: bigint) {
    try {
      this.media = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, sampleRate: 48000, echoCancellation: false, noiseSuppression: false, autoGainControl: false }, video: false })
      this.context?.createMediaStreamSource(this.media).connect(this.audioNode!)
      this.pttChannel = channelId; this.worker?.postMessage({ type: 'start-transmit', channelId: channelId.toString() }); this.update({ ptt: 'transmitting' })
    } catch { this.send('ptt_stop', { channel_id: channelId.toString() }); this.update({ ptt: 'stopping', error: 'Microphone access failed. PTT stopped.' }) }
  }

  private stopMicrophone() { this.media?.getTracks().forEach(track => track.stop()); this.media = undefined }

  private onWorker(message: any) {
    if (message.type === 'capability') { this.capabilityResolve?.(Boolean(message.supported)); this.capabilityResolve = undefined }
    else if (message.type === 'capture-ack') this.audioNode?.port.postMessage({ type: 'capture-ack' })
    else if (message.type === 'packet') {
      if (!this.socket.sendMedia(message.packet)) { this.update({ error: 'PTT stopped because the transmit queue was full.' }); this.stopPTT() }
    } else if (message.type === 'pcm' && message.epoch === this.playoutEpoch) this.audioNode?.port.postMessage({ type: 'play', epoch: message.epoch, pcm: message.pcm }, [message.pcm.buffer])
    else if (message.type === 'jitter-reset' && message.epoch === this.playoutEpoch) { this.audioNode?.port.postMessage({ type: 'clear', epoch: message.epoch }); if (message.reason === 'overflow') this.pausePlaybackForOverload('Playback paused because the decoded audio queue was full.') }
    else if (message.type === 'error') { this.update({ error: message.message }); if (message.code === 'receive_overload') this.pausePlaybackForOverload('Playback paused because the decoder queue was full.'); else if (this.state.ptt !== 'idle') this.stopPTT() }
  }

  private onSocket(event: SocketEvent) {
    if (event.closed) { this.readinessResolve?.(false); this.readinessResolve = undefined; this.stopMicrophone(); if (this.state.playback) this.closePlayback(); else this.resetPlayout(); this.update({ connected: false, listening: false, ptt: 'idle', error: this.closing ? this.state.error : 'The live connection closed. Select Listen live to reconnect.' }); return }
    if (event.media && this.state.ptt !== 'transmitting') {
      const live = event.media.kind === MediaKind.Live && event.media.channelId === this.liveChannel && !this.state.playback
      const playback = event.media.kind === MediaKind.Playback && event.media.channelId.toString() === this.state.playback?.channelId
      if (live && !this.retired.has(event.media.channelId)) this.worker?.postMessage({ type: 'media', epoch: this.playoutEpoch, packet: event.media })
      else if (playback && !this.retired.has(event.media.channelId) && (this.playbackAccepting || this.playbackSequenceAccounting)) {
        if (event.media.sequence !== this.playbackExpectedSequence) { this.playbackBarrier(); this.pausePlaybackForOverload('Playback paused because the server media sequence was invalid.'); return }
        this.playbackExpectedSequence = (event.media.sequence + 1) >>> 0
        if (this.playbackAccepting) this.worker?.postMessage({ type: 'media', epoch: this.playoutEpoch, packet: event.media })
      }
    }
    const control = event.control
    if (!control) return
    if (control.type === 'hello_ok') { const body = control.body as { ptt_available?: boolean; status?: PublicStatus }; this.readinessResolve?.(true); this.readinessResolve = undefined; this.update({ connected: true, pttAvailable: body.ptt_available === true, status: body.status, error: undefined }) }
    else if (control.type === 'status') this.update({ status: control.body as PublicStatus })
    else if (control.type === 'stream_start') { const body = control.body as { channel_id: string; source_callsign: string }; try { const id = parseChannelId(body.channel_id); if (this.liveChannel) this.retire(this.liveChannel); this.liveChannel = id; if (!this.state.playback) this.resetPlayout(); this.update({ currentSource: body.source_callsign }); this.activity(`${body.source_callsign} started a transmission.`) } catch { this.socket.close() } }
    else if (control.type === 'stream_end') { const body = control.body as { channel_id: string; reason: string }; try { const id = parseChannelId(body.channel_id); this.retire(id); if (id === this.liveChannel) { this.liveChannel = undefined; this.update({ currentSource: undefined }); this.activity(`The transmission ended: ${body.reason}.`) } } catch { this.socket.close() } }
    else if (control.type === 'ptt_granted') { const body = control.body as { channel_id: string; tot_seconds: number }; try { const id = parseChannelId(body.channel_id); if (this.cancelPending) { this.send('ptt_stop', { channel_id: body.channel_id }); this.update({ ptt: 'stopping' }) } else { void this.startMicrophone(id); this.startTOT(body.tot_seconds) } } catch { this.update({ ptt: 'idle', error: 'The server returned an invalid channel ID.' }); this.socket.close() } }
    else if (control.type === 'ptt_busy') { this.update({ ptt: 'idle', busy: true, error: 'The reflector floor is busy.' }); setTimeout(() => this.update({ busy: false }), 3000) }
    else if (control.type === 'ptt_ended') { clearInterval(this.totTimer); this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.pttChannel = undefined; this.cancelPending = false; this.update({ ptt: 'idle', remaining: undefined }) }
    else if (control.type === 'discontinuity') { const body = control.body as { old_channel_id: string; new_channel_id: string }; try { const oldID = parseChannelId(body.old_channel_id); const newID = parseChannelId(body.new_channel_id); this.retire(oldID); if (oldID === this.liveChannel) this.liveChannel = newID; if (!this.state.playback) this.resetPlayout(); this.activity('Live audio restarted after a network discontinuity.') } catch { this.socket.close() } }
    else if (control.type === 'playback_opened') { const body = control.body as { channel_id: string; recording_id: string; duration_ms: number; status: 'complete' | 'partial' }; const request = control.request_id ? this.playbackRequests.get(control.request_id) : undefined; if (control.request_id) this.playbackRequests.delete(control.request_id); if (!request || request.action !== 'open' || request.epoch !== this.playoutEpoch) return; try { parseChannelId(body.channel_id); this.resetPlayout(); this.playbackExpectedSequence = 0; this.playbackSequenceAccounting = false; this.playbackAccepting = true; this.update({ playback: { channelId: body.channel_id, recordingId: body.recording_id, durationMs: body.duration_ms, status: body.status, state: 'playing', elapsedMs: 0 } }); this.startPlaybackClock() } catch { this.socket.close() } }
    else if (control.type === 'playback_state') { const body = control.body as { channel_id: string; state: 'playing' | 'paused' | 'closed'; elapsed_ms: number }; const request = control.request_id ? this.playbackRequests.get(control.request_id) : undefined; if (control.request_id) this.playbackRequests.delete(control.request_id); if (request && request.epoch !== this.playoutEpoch) return; if (this.state.playback?.channelId === body.channel_id) { if (body.state === 'closed') this.closePlayback(); else { this.resetPlayout(); if (request?.action === 'seek') this.playbackExpectedSequence = 0; this.playbackSequenceAccounting = false; this.playbackAccepting = body.state === 'playing'; this.update({ playback: { ...this.state.playback, state: body.state, elapsedMs: body.elapsed_ms } }); if (body.state === 'playing') this.startPlaybackClock(); else clearInterval(this.playbackTimer) } } }
    else if (control.type === 'error') { const request = control.request_id ? this.playbackRequests.get(control.request_id) : undefined; if (control.request_id) this.playbackRequests.delete(control.request_id); if (request?.epoch === this.playoutEpoch) this.playbackSequenceAccounting = false; const body = control.body as { code?: string; text?: string }; if (body.code === 'session_invalid') this.downgradeSession(); else this.update({ error: body.text ?? 'The live request failed.' }) }
  }

  private startTOT(seconds: number) {
    clearInterval(this.totTimer)
    const deadline = performance.now() + seconds * 1000
    this.update({ remaining: seconds })
    this.totTimer = window.setInterval(() => { const remaining = Math.max(0, Math.ceil((deadline - performance.now()) / 1000)); this.update({ remaining }); if (remaining === 0) { clearInterval(this.totTimer); this.stopPTT() } }, 250)
  }

  private startPlaybackClock() {
    clearInterval(this.playbackTimer)
    let previous = performance.now()
    this.playbackTimer = window.setInterval(() => { if (!this.state.playback || this.state.playback.state !== 'playing') return; const now = performance.now(); const elapsedMs = Math.min(this.state.playback.durationMs ?? Number.MAX_SAFE_INTEGER, this.state.playback.elapsedMs + now - previous); previous = now; this.update({ playback: { ...this.state.playback, elapsedMs } }) }, 250)
  }

  private closePlayback() { clearInterval(this.playbackTimer); this.playbackBarrier(); this.playbackExpectedSequence = undefined; this.playbackRequests.clear(); this.update({ playback: undefined }) }
  private downgradeSession() { if (this.state.ptt !== 'idle') this.stopPTT(); if (this.state.playback) this.closePlayback(); this.update({ pttAvailable: false }); this.activity('Your session ended. Live listening remains available.'); this.dispatchEvent(new Event('session-invalid')) }
  private resetPlayout() { this.playoutEpoch++; this.worker?.postMessage({ type: 'reset-playout', epoch: this.playoutEpoch }); this.audioNode?.port.postMessage({ type: 'clear', epoch: this.playoutEpoch }) }
  private playbackBarrier() { this.playbackAccepting = false; this.playbackSequenceAccounting = false; this.resetPlayout() }
  private pausePlaybackForOverload(message: string) { if (this.state.playback && this.state.playback.state !== 'paused') this.playback('playback_pause', this.state.playback.channelId); this.update({ error: message }) }
  private sendPlayback(type: string, action: 'open' | 'pause' | 'resume' | 'seek' | 'close', body: unknown): boolean { const requestID = this.send(type, body); if (!requestID) return false; this.playbackRequests.set(requestID, { action, epoch: this.playoutEpoch }); return true }
  private send(type: string, body: unknown): string | undefined { try { return this.socket.send(type, body) } catch { this.update({ error: 'The live request queue is full. Reconnect and try again.' }); return undefined } }
}
