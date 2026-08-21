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

export interface WirePublicStatus {
  health: 'ok' | 'degraded' | 'unavailable'
  ready: boolean
  reflector: { id: string; display_name: string }
  client_count: number
  floor: { active: boolean; source_callsign?: string; started_at?: string; remaining_seconds?: number }
  recording: { available: boolean; quota_full: boolean }
  server_time: string
}

export function makeRequest<T>(type: string, requestId: string, body: T): string {
  if (!/^[\x20-\x7e]{1,64}$/.test(requestId)) throw new ProtocolError('The request ID is invalid.')
  return JSON.stringify({ api_version: 1, type, request_id: requestId, body })
}

const serverTypes = new Set(['hello_ok', 'status', 'stream_start', 'stream_end', 'ptt_granted', 'ptt_busy', 'ptt_ended', 'playback_opened', 'playback_state', 'discontinuity', 'error'])

function isRecord(value: unknown): value is Record<string, unknown> { return Boolean(value) && typeof value === 'object' && !Array.isArray(value) }
function hasOnly(record: Record<string, unknown>, required: string[], optional: string[] = []): boolean {
  const allowed = new Set([...required, ...optional])
  return required.every(key => key in record) && Object.keys(record).every(key => allowed.has(key))
}

export function parsePublicStatus(value: unknown): WirePublicStatus {
  if (!isRecord(value) || !hasOnly(value, ['health', 'ready', 'reflector', 'client_count', 'floor', 'recording', 'server_time'])) throw new ProtocolError('The status body is invalid.')
  const reflector = value.reflector; const floor = value.floor; const recording = value.recording
  if (!['ok', 'degraded', 'unavailable'].includes(String(value.health)) || typeof value.ready !== 'boolean' || !Number.isSafeInteger(value.client_count) || Number(value.client_count) < 0 || typeof value.server_time !== 'string') throw new ProtocolError('The status body is invalid.')
  if (!isRecord(reflector) || !hasOnly(reflector, ['id', 'display_name']) || typeof reflector.id !== 'string' || typeof reflector.display_name !== 'string') throw new ProtocolError('The status body is invalid.')
  if (!isRecord(floor) || !hasOnly(floor, ['active'], ['source_callsign', 'started_at', 'remaining_seconds']) || typeof floor.active !== 'boolean' || (floor.source_callsign !== undefined && typeof floor.source_callsign !== 'string') || (floor.started_at !== undefined && typeof floor.started_at !== 'string') || (floor.remaining_seconds !== undefined && (!Number.isFinite(floor.remaining_seconds) || Number(floor.remaining_seconds) < 0))) throw new ProtocolError('The status body is invalid.')
  if (!isRecord(recording) || !hasOnly(recording, ['available', 'quota_full']) || typeof recording.available !== 'boolean' || typeof recording.quota_full !== 'boolean') throw new ProtocolError('The status body is invalid.')
  return value as unknown as WirePublicStatus
}

export function parseControlMessage(input: string): ControlMessage {
  if (new TextEncoder().encode(input).byteLength > 16384) throw new ProtocolError('The control message is too large.')
  let value: unknown
  try { value = JSON.parse(input) } catch { throw new ProtocolError('The control message is not valid JSON.') }
  if (!isRecord(value)) throw new ProtocolError('The control envelope is invalid.')
  const record = value as Record<string, unknown>
  const keys = Object.keys(record)
  if (keys.some(key => !['api_version', 'type', 'request_id', 'body'].includes(key)) || record.api_version !== 1 || typeof record.type !== 'string' || !serverTypes.has(record.type) || !isRecord(record.body)) throw new ProtocolError('The control envelope is invalid.')
  if (record.request_id !== undefined && (typeof record.request_id !== 'string' || !/^[\x20-\x7e]{1,64}$/.test(record.request_id))) throw new ProtocolError('The response request ID is invalid.')
  if (record.type === 'status') parsePublicStatus(record.body)
  if (record.type === 'hello_ok' && 'status' in record.body) parsePublicStatus(record.body.status)
  if (record.type === 'error' && (!hasOnly(record.body, ['code', 'text']) || typeof record.body.code !== 'string' || typeof record.body.text !== 'string')) throw new ProtocolError('The error body is invalid.')
  return record as unknown as ControlMessage
}
