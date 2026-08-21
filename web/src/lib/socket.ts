import { decodePacket, makeRequest, parseControlMessage, type ControlMessage, type MediaPacket } from './orwb'

export type SocketEvent = { control?: ControlMessage; media?: MediaPacket; closed?: { code: number; reason: string } }

export class OpusRefSocket extends EventTarget {
  private socket?: WebSocket
  private requestCounter = 0
  constructor(private readonly endpoint = '/api/v1/ws') { super() }

  connect(audio: { encoder: boolean; decoder: boolean; context_rate: number }, csrf?: string): void {
    const url = new URL(this.endpoint, window.location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    this.socket = new WebSocket(url)
    this.socket.binaryType = 'arraybuffer'
    this.socket.addEventListener('open', () => this.send('hello', { audio, ...(csrf ? { csrf_token: csrf } : {}) }))
    this.socket.addEventListener('message', event => {
      try {
        const detail: SocketEvent = typeof event.data === 'string' ? { control: parseControlMessage(event.data) } : { media: decodePacket(event.data) }
        this.dispatchEvent(new CustomEvent('event', { detail }))
      } catch { this.socket?.close(4400, 'invalid_message') }
    })
    this.socket.addEventListener('close', event => this.dispatchEvent(new CustomEvent('event', { detail: { closed: { code: event.code, reason: event.reason } } satisfies SocketEvent })))
  }

  send(type: string, body: unknown): string {
    const requestId = `${Date.now().toString(36)}-${++this.requestCounter}`
    this.socket?.send(makeRequest(type, requestId, body))
    return requestId
  }

  sendMedia(packet: ArrayBuffer): boolean {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN || this.socket.bufferedAmount >= 65536) return false
    this.socket.send(packet)
    return true
  }
  get bufferedAmount(): number { return this.socket?.bufferedAmount ?? 0 }
  close(): void { this.socket?.close(1000, 'page_close') }
}
