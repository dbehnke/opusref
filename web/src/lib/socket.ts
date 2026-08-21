import { decodePacket, makeRequest, parseControlMessage, ProtocolError, type ControlMessage, type MediaPacket } from './orwb'

export type SocketEvent = { control?: ControlMessage; media?: MediaPacket; closed?: { code: number; reason: string } }

export class OpusRefSocket extends EventTarget {
  private socket?: WebSocket
  private requestCounter = 0
  private readonly pending = new Set<string>()
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
        if (detail.control?.request_id && !this.pending.delete(detail.control.request_id)) throw new ProtocolError('The response request ID is not pending.')
        this.dispatchEvent(new CustomEvent('event', { detail }))
      } catch { this.socket?.close(4400, 'invalid_message') }
    })
    this.socket.addEventListener('close', event => { this.pending.clear(); this.dispatchEvent(new CustomEvent('event', { detail: { closed: { code: event.code, reason: event.reason } } satisfies SocketEvent })) })
  }

  send(type: string, body: unknown): string {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) throw new ProtocolError('The live connection is not ready.')
    if (this.pending.size >= 32 || this.socket.bufferedAmount >= 65536) { this.socket.close(4409, 'client_overload'); throw new ProtocolError('The control queue is full.') }
    const requestId = `${Date.now().toString(36)}-${++this.requestCounter}`
    const message = makeRequest(type, requestId, body)
    this.pending.add(requestId)
    this.socket.send(message)
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
