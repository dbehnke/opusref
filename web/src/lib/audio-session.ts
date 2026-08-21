import { checkAudioCapability } from './capability'
import { MediaKind } from './orwb'
import { OpusRefSocket, type SocketEvent } from './socket'

export interface AudioSessionState {
  connected: boolean
  listening: boolean
  ptt: 'idle' | 'requesting' | 'transmitting' | 'stopping'
  busy: boolean
  remaining?: number
  error?: string
}

export class BrowserAudioSession extends EventTarget {
  readonly state: AudioSessionState = { connected: false, listening: false, ptt: 'idle', busy: false }
  private readonly socket = new OpusRefSocket()
  private worker?: Worker
  private context?: AudioContext
  private audioNode?: AudioWorkletNode
  private media?: MediaStream
  private pttChannel?: bigint

  constructor(private readonly csrf?: string) {
    super()
    this.socket.addEventListener('event', event => this.onSocket((event as CustomEvent<SocketEvent>).detail))
  }

  private update(change: Partial<AudioSessionState>) { Object.assign(this.state, change); this.dispatchEvent(new Event('state')) }

  async start(): Promise<boolean> {
    const capability = await checkAudioCapability()
    if (!capability.supported) { this.update({ error: capability.reason }); return false }
    this.context = new AudioContext({ sampleRate: 48000 })
    await this.context.audioWorklet.addModule('/opusref-audio-worklet.js')
    this.audioNode = new AudioWorkletNode(this.context, 'opusref-audio', { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] })
    this.audioNode.connect(this.context.destination)
    this.worker = new Worker(new URL('../workers/audio.worker.ts', import.meta.url), { type: 'module' })
    this.worker.addEventListener('message', event => this.onWorker(event.data))
    this.audioNode.port.addEventListener('message', event => { if (event.data.type === 'capture') this.worker?.postMessage({ type: 'pcm', pcm: event.data.pcm }, [event.data.pcm.buffer]) })
    this.audioNode.port.start()
    this.socket.connect({ encoder: true, decoder: true, context_rate: 48000 }, this.csrf)
    this.update({ listening: true })
    return true
  }

  async requestPTT(): Promise<void> {
    if (!this.state.connected) { this.update({ error: 'The live connection is not ready.' }); return }
    this.update({ ptt: 'requesting', error: undefined })
    this.socket.send('ptt_start', {})
  }

  stopPTT(): void {
    if (this.pttChannel) this.socket.send('ptt_stop', { channel_id: this.pttChannel.toString() })
    this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.update({ ptt: 'stopping' })
  }

  openPlayback(recordingId: string): void { this.socket.send('playback_open', { recording_id: recordingId }) }
  playback(type: 'playback_pause' | 'playback_resume' | 'playback_close', channelId: string): void { this.socket.send(type, { channel_id: channelId }) }
  seek(channelId: string, elapsedMs: number): void { this.socket.send('playback_seek', { channel_id: channelId, elapsed_ms: Math.max(0, Math.round(elapsedMs)) }) }

  async close(): Promise<void> {
    if (this.state.ptt !== 'idle') this.stopPTT()
    this.socket.close(); this.worker?.terminate(); this.audioNode?.disconnect(); this.stopMicrophone(); await this.context?.close()
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
    if (message.type === 'packet') {
      if (!this.socket.sendMedia(message.packet)) { this.update({ error: 'PTT stopped because the transmit queue was full.' }); this.stopPTT() }
    } else if (message.type === 'pcm') this.audioNode?.port.postMessage({ type: 'play', pcm: message.pcm }, [message.pcm.buffer])
    else if (message.type === 'error') { this.update({ error: message.message }); if (this.state.ptt !== 'idle') this.stopPTT() }
  }

  private onSocket(event: SocketEvent) {
    if (event.closed) { this.stopMicrophone(); this.update({ connected: false, ptt: 'idle', error: 'The live connection closed.' }); return }
    if (event.media && (event.media.kind === MediaKind.Live || event.media.kind === MediaKind.Playback) && this.state.ptt !== 'transmitting') this.worker?.postMessage({ type: 'media', packet: event.media })
    const control = event.control
    if (!control) return
    if (control.type === 'hello_ok') this.update({ connected: true })
    else if (control.type === 'ptt_granted') { const body = control.body as { channel_id: string; tot_seconds: number }; void this.startMicrophone(BigInt(body.channel_id)); this.update({ remaining: body.tot_seconds }) }
    else if (control.type === 'ptt_busy') this.update({ ptt: 'idle', busy: true, error: 'The reflector floor is busy.' })
    else if (control.type === 'ptt_ended') { this.worker?.postMessage({ type: 'stop-transmit' }); this.stopMicrophone(); this.pttChannel = undefined; this.update({ ptt: 'idle', remaining: undefined }) }
    else if (control.type === 'discontinuity') this.audioNode?.port.postMessage({ type: 'clear' })
    else if (control.type === 'error') this.update({ error: (control.body as { text?: string }).text ?? 'The live request failed.' })
  }
}
