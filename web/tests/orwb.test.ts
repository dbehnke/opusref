import { describe, expect, it } from 'vitest'
import { decodePacket, encodePacket, MediaKind, parseControlMessage, ProtocolError } from '../src/lib/orwb'

describe('ORWB media packets', () => {
  it('preserves one complete payload and network-order metadata', () => {
    const encoded = encodePacket({ kind: MediaKind.Live, channelId: 0x0102030405060708n, sequence: 0xaabbccdd, timestamp: 960, payload: new Uint8Array([1, 2, 3]) })
    expect(new Uint8Array(encoded).slice(0, 8)).toEqual(new Uint8Array([79, 82, 87, 66, 1, 1, 0, 0]))
    expect(decodePacket(encoded)).toEqual({ kind: MediaKind.Live, channelId: 0x0102030405060708n, sequence: 0xaabbccdd, timestamp: 960, payload: new Uint8Array([1, 2, 3]) })
  })
  it.each([0, 1169])('rejects a payload with %i bytes', size => expect(() => encodePacket({ kind: MediaKind.Transmit, channelId: 1n, sequence: 0, timestamp: 0, payload: new Uint8Array(size) })).toThrow(ProtocolError))
  it('rejects malformed reserved bytes and lengths', () => {
    const encoded = encodePacket({ kind: MediaKind.Playback, channelId: 2n, sequence: 0, timestamp: 0, payload: new Uint8Array([1]) })
    new Uint8Array(encoded)[26] = 1
    expect(() => decodePacket(encoded)).toThrow('Reserved')
  })
})

describe('control messages', () => {
  it('accepts a version-one server envelope', () => { expect(parseControlMessage('{"api_version":1,"type":"ptt_busy","body":{}}').type).toBe('ptt_busy') })
  it.each(['{"api_version":1,"type":"unknown","body":{}}', '{"api_version":1,"type":"status","body":{},"extra":1}', '{"api_version":2,"type":"status","body":{}}'])('rejects an invalid server envelope', input => expect(() => parseControlMessage(input)).toThrow(ProtocolError))
  it('rejects more than 16 KiB', () => expect(() => parseControlMessage('{"api_version":1,"type":"error","body":{"text":"' + 'x'.repeat(16384) + '"}}')).toThrow('too large'))
})
