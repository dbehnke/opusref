import { MediaKind } from './orwb'
import { OpusRefSocket, type SocketEvent } from './socket'
import { parseChannelId } from './channel-id'

export interface AudioSessionState {
  connected: boolean
  listening: boolean
  ptt: 'idle' | 'requesting' | 'transmitting' | 'stopping'
  busy: boolean
  remaining?: number
  error?: string
  playback?: { channelId: string; state: 'playing' | 'paused'; elapsedMs: number; recordingId?: string; durationMs?: number }
  currentSource?: string
  activity: { text: string; at: string }[]
  pttAvailable: boolean
}

export class BrowserAudioSession extends EventTarget {
  readonly state: AudioSessionState = { connected: false, listening: false, ptt: 'idle', busy: false, activity: [], pttAvailable: false }
  private readonly socket = new OpusRefSocket()
  private worker?: Worker
  private context?: AudioContext
  private audioNode?: AudioWorkletNode
  private media?: MediaStream
  private pttChannel?: bigint
  private liveChannel?: bigint
  private retired = new Set<bigint>()
  private cancelPending = false
  private totTimer = 0
  private capabilityResolve?: (supported: boolean) => void
  private closing = false
  private readonly lifecycleStop = () => { if (document.visibilityState === 'hidden') void this.close() }

  constructor(private readonly csrf?: string) {
    super()
    this.socket.addEventListener('event', event => this.onSocket((event as CustomEvent<SocketEvent>).detail))
    document.addEventListener('visibilitychange', this.lifecycleStop)
    window.addEventListener('pagehide', this.lifecycleStop)
  }

  private update(change: Partial<AudioSessionState>) { Object.assign(this.state, change); this.dispatchEvent(new Event('state')) }
  private activity(text: string) { this.update({ activity: [{ text, at: new Date().toISOString() }, ...this.state.activity].slice(0, 20) }) }
  private retire(id: bigint) { this.retired.add(id); while (this.retired.size > 128) this.retired.delete(this.retired.values().next().value!) }

  async start(): Promise<boolean> {
    this.closing = false
    this.worker = new Worker(new URL('../workers/audio.worker.ts', import.meta.url), { type: 'module' })
    this.worker.addEventListener('message', event => this.onWorker(event.data))
    const supported = await new Promise<boolean>(resolve => { this.capabilityResolve = resolve; this.worker!.postMessage({ type: 'capability' }); setTimeout(() => resolve(false), 5000) })
    if (!supported) { this.worker.terminate(); this.worker = undefined; this.update({ error: 'This browser does not support raw Opus audio.' }); return false }
    this.context = new AudioContext({ sampleRate: 48000 })
    if (this.context.sampleRate !== 48000) { await this.context.close(); this.worker.terminate(); this.update({ error: 'This device cannot use a 48 kHz audio context.' }); return false }
    await this.context.audioWorklet.addModule('/opusref-audio-worklet.js')
    this.audioNode = new AudioWorkletNode(this.context, 'opusref-audio', { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] })
    this.audioNode.connect(this.context.destination)
    this.audioNode.port.addEventListener('message', event => {
      if (event.data.type === 'capture') { this.worker?.postMessage({ type: 'socket-buffer', bytes: this.socket.bufferedAmount }); this.worker?.postMessage({ type: 'pcm', pcm: event.data.pcm }, [event.data.pcm.buffer]) }
      if (event.data.type === 'capture-overflow') { this.update({ error: 'PTT stopped because the capture queue was full.' }); this.stopPTT() }
      if (event.data.type === 'playback-overflow') { this.worker?.postMessage({ type: 'reset-playout' }); this.update({ error: 'Audio restarted because the playback queue was full.' }) }
    })
    this.audioNode.port.start()
    this.socket.connect({ encoder: true, decoder: true, context_rate: 48000 }, this.csrf)
    this.update({ listening: true })
    return true
  }

  async requestPTT(): Promise<void> {
    if (!this.state.connected) { this.update({ error: 'The live connection is not ready.' }); return }
    this.cancelPending = false; this.update({ ptt: 'requesting', busy: false, error: undefined })
    this.socket.send('ptt_start', {})
  }

  stopPTT(): void {
    if (this.pttChannel) this.socket.send('ptt_stop', { channel_id: this.pttChannel.toString() }); else this.cancelPending = true
    this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.update({ ptt: 'stopping' })
  }

  openPlayback(recordingId: string): void { this.socket.send('playback_open', { recording_id: recordingId }) }
  playback(type: 'playback_pause' | 'playback_resume' | 'playback_close', channelId: string): void { this.socket.send(type, { channel_id: channelId }); if (type === 'playback_close') { this.worker?.postMessage({ type: 'reset-playout' }); this.audioNode?.port.postMessage({ type: 'clear' }); this.update({ playback: undefined }) } }
  seek(channelId: string, elapsedMs: number): void { this.worker?.postMessage({ type: 'reset-playout' }); this.audioNode?.port.postMessage({ type: 'clear' }); this.socket.send('playback_seek', { channel_id: channelId, elapsed_ms: Math.max(0, Math.round(elapsedMs)) }) }

  async close(): Promise<void> {
    this.closing = true
    if (this.state.ptt !== 'idle') this.stopPTT()
    clearInterval(this.totTimer)
    this.socket.close(); this.worker?.terminate(); this.audioNode?.disconnect(); this.stopMicrophone(); await this.context?.close()
    document.removeEventListener('visibilitychange', this.lifecycleStop)
    window.removeEventListener('pagehide', this.lifecycleStop)
    this.update({ connected: false, listening: false, ptt: 'idle' })
  }

  private async startMicrophone(channelId: bigint) {
    try {
      this.media = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, sampleRate: 48000, echoCancellation: false, noiseSuppression: false, autoGainControl: false }, video: false })
      this.context?.createMediaStreamSource(this.media).connect(this.audioNode!)
      this.pttChannel = channelId; this.worker?.postMessage({ type: 'start-transmit', channelId: channelId.toString() }); this.update({ ptt: 'transmitting' })
    } catch { this.socket.send('ptt_stop', { channel_id: channelId.toString() }); this.update({ ptt: 'stopping', error: 'Microphone access failed. PTT stopped.' }) }
  }

  private stopMicrophone() { this.media?.getTracks().forEach(track => track.stop()); this.media = undefined }

  private onWorker(message: any) {
    if (message.type === 'capability') { this.capabilityResolve?.(Boolean(message.supported)); this.capabilityResolve = undefined }
    else if (message.type === 'capture-ack') this.audioNode?.port.postMessage({ type: 'capture-ack' })
    else if (message.type === 'packet') {
      if (!this.socket.sendMedia(message.packet)) { this.update({ error: 'PTT stopped because the transmit queue was full.' }); this.stopPTT() }
    } else if (message.type === 'pcm') this.audioNode?.port.postMessage({ type: 'play', pcm: message.pcm }, [message.pcm.buffer])
    else if (message.type === 'jitter-reset') this.audioNode?.port.postMessage({ type: 'clear' })
    else if (message.type === 'error') { this.update({ error: message.message }); if (this.state.ptt !== 'idle') this.stopPTT() }
  }

  private onSocket(event: SocketEvent) {
    if (event.closed) { this.stopMicrophone(); this.update({ connected: false, listening: false, ptt: 'idle', error: this.closing ? undefined : 'The live connection closed. Select Listen live to reconnect.' }); return }
    if (event.media && this.state.ptt !== 'transmitting') {
      const live = event.media.kind === MediaKind.Live && event.media.channelId === this.liveChannel && !this.state.playback
      const playback = event.media.kind === MediaKind.Playback && event.media.channelId.toString() === this.state.playback?.channelId
      if ((live || playback) && !this.retired.has(event.media.channelId)) this.worker?.postMessage({ type: 'media', packet: event.media })
    }
    const control = event.control
    if (!control) return
    if (control.type === 'hello_ok') { const body = control.body as { ptt_available?: boolean }; this.update({ connected: true, pttAvailable: body.ptt_available === true }) }
    else if (control.type === 'stream_start') { const body = control.body as { channel_id: string; source_callsign: string }; try { const id = parseChannelId(body.channel_id); if (this.liveChannel) this.retire(this.liveChannel); this.liveChannel = id; this.worker?.postMessage({ type: 'reset-playout' }); this.update({ currentSource: body.source_callsign }); this.activity(`${body.source_callsign} started a transmission.`) } catch { this.socket.close() } }
    else if (control.type === 'stream_end') { const body = control.body as { channel_id: string; reason: string }; try { const id = parseChannelId(body.channel_id); this.retire(id); if (id === this.liveChannel) { this.liveChannel = undefined; this.update({ currentSource: undefined }); this.activity(`The transmission ended: ${body.reason}.`) } } catch { this.socket.close() } }
    else if (control.type === 'ptt_granted') { const body = control.body as { channel_id: string; tot_seconds: number }; try { const id = parseChannelId(body.channel_id); if (this.cancelPending) { this.socket.send('ptt_stop', { channel_id: body.channel_id }); this.update({ ptt: 'stopping' }) } else { void this.startMicrophone(id); this.startTOT(body.tot_seconds) } } catch { this.update({ ptt: 'idle', error: 'The server returned an invalid channel ID.' }); this.socket.close() } }
    else if (control.type === 'ptt_busy') { this.update({ ptt: 'idle', busy: true, error: 'The reflector floor is busy.' }); setTimeout(() => this.update({ busy: false }), 3000) }
    else if (control.type === 'ptt_ended') { clearInterval(this.totTimer); this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.pttChannel = undefined; this.cancelPending = false; this.update({ ptt: 'idle', remaining: undefined }) }
    else if (control.type === 'discontinuity') { const body = control.body as { old_channel_id: string; new_channel_id: string }; try { const oldID = parseChannelId(body.old_channel_id); const newID = parseChannelId(body.new_channel_id); this.retire(oldID); if (oldID === this.liveChannel) this.liveChannel = newID; this.worker?.postMessage({ type: 'reset-playout' }); this.audioNode?.port.postMessage({ type: 'clear' }); this.activity('Live audio restarted after a network discontinuity.') } catch { this.socket.close() } }
    else if (control.type === 'playback_opened') { const body = control.body as { channel_id: string; recording_id: string; duration_ms: number }; try { parseChannelId(body.channel_id); this.update({ playback: { channelId: body.channel_id, recordingId: body.recording_id, durationMs: body.duration_ms, state: 'playing', elapsedMs: 0 } }) } catch { this.socket.close() } }
    else if (control.type === 'playback_state') { const body = control.body as { channel_id: string; state: 'playing' | 'paused'; elapsed_ms: number }; if (this.state.playback?.channelId === body.channel_id) this.update({ playback: { ...this.state.playback, state: body.state, elapsedMs: body.elapsed_ms } }) }
    else if (control.type === 'error') this.update({ error: (control.body as { text?: string }).text ?? 'The live request failed.' })
  }

  private startTOT(seconds: number) {
    clearInterval(this.totTimer)
    const deadline = performance.now() + seconds * 1000
    this.update({ remaining: seconds })
    this.totTimer = window.setInterval(() => { const remaining = Math.max(0, Math.ceil((deadline - performance.now()) / 1000)); this.update({ remaining }); if (remaining === 0) { clearInterval(this.totTimer); this.stopPTT() } }, 250)
  }
}
