export const ORWB_HEADER_LENGTH = 32
export const ORWB_MAX_PAYLOAD = 1168
export const ORWB_MAX_MESSAGE = 1200

export enum MediaKind { Live = 1, Transmit = 2, Playback = 3 }

export interface MediaPacket {
  kind: MediaKind
  channelId: bigint
  sequence: number
  timestamp: number
  payload: Uint8Array
}

export class ProtocolError extends Error {}

export function encodePacket(packet: MediaPacket): ArrayBuffer {
  if (packet.channelId === 0n) throw new ProtocolError('The channel ID must not be zero.')
  if (packet.payload.byteLength < 1 || packet.payload.byteLength > ORWB_MAX_PAYLOAD) throw new ProtocolError('The Opus packet size is invalid.')
  const result = new ArrayBuffer(ORWB_HEADER_LENGTH + packet.payload.byteLength)
  const bytes = new Uint8Array(result)
  bytes.set([0x4f, 0x52, 0x57, 0x42], 0)
  const view = new DataView(result)
  view.setUint8(4, 1)
  view.setUint8(5, packet.kind)
  view.setBigUint64(8, packet.channelId)
  view.setUint32(16, packet.sequence)
  view.setUint32(20, packet.timestamp)
  view.setUint16(24, packet.payload.byteLength)
  bytes.set(packet.payload, ORWB_HEADER_LENGTH)
  return result
}

export function decodePacket(input: ArrayBuffer): MediaPacket {
  if (input.byteLength < ORWB_HEADER_LENGTH || input.byteLength > ORWB_MAX_MESSAGE) throw new ProtocolError('The media message size is invalid.')
  const bytes = new Uint8Array(input)
  if (bytes[0] !== 0x4f || bytes[1] !== 0x52 || bytes[2] !== 0x57 || bytes[3] !== 0x42) throw new ProtocolError('The media message magic is invalid.')
  const view = new DataView(input)
  if (view.getUint8(4) !== 1) throw new ProtocolError('The media version is not supported.')
  const kind = view.getUint8(5)
  if (![MediaKind.Live, MediaKind.Transmit, MediaKind.Playback].includes(kind)) throw new ProtocolError('The media kind is invalid.')
  if (view.getUint16(6) !== 0 || bytes.slice(26, 32).some(value => value !== 0)) throw new ProtocolError('Reserved media fields must be zero.')
  const channelId = view.getBigUint64(8)
  if (channelId === 0n) throw new ProtocolError('The channel ID must not be zero.')
  const length = view.getUint16(24)
  if (length < 1 || length > ORWB_MAX_PAYLOAD || input.byteLength !== ORWB_HEADER_LENGTH + length) throw new ProtocolError('The payload length is invalid.')
  return { kind, channelId, sequence: view.getUint32(16), timestamp: view.getUint32(20), payload: bytes.slice(32) }
}

export interface ControlMessage<T = unknown> { api_version: 1; type: string; request_id?: string; body: T }

export function makeRequest<T>(type: string, requestId: string, body: T): string {
  if (!/^[\x20-\x7e]{1,64}$/.test(requestId)) throw new ProtocolError('The request ID is invalid.')
  return JSON.stringify({ api_version: 1, type, request_id: requestId, body })
}
