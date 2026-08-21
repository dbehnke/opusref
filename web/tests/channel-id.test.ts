import { describe, expect, it } from 'vitest'
import { parseChannelId } from '../src/lib/channel-id'
import { decodePacket, encodePacket, makeRequest, MediaKind } from '../src/lib/orwb'

describe('JSON channel IDs', () => {
  it('preserves values above the safe integer range', () => { expect(parseChannelId('18446744073709551615')).toBe(18446744073709551615n) })
  it('preserves one lossless ID across JSON control and ORWB media', () => { const text = makeRequest('ptt_stop', 'request-1', { channel_id: '18446744073709551615' }); expect(JSON.parse(text).body.channel_id).toBe('18446744073709551615'); const media = encodePacket({ kind: MediaKind.Transmit, channelId: parseChannelId(JSON.parse(text).body.channel_id), sequence: 0, timestamp: 0, payload: new Uint8Array([1]) }); expect(decodePacket(media).channelId).toBe(18446744073709551615n) })
  it.each(['', '0', '01', '-1', '+1', '1.0', '9007199254740993.0', '18446744073709551616', '1e3', 42])('rejects a noncanonical ID %p', value => { expect(() => parseChannelId(value)).toThrow() })
})
