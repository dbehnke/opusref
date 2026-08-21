const canonicalChannel = /^[1-9][0-9]{0,19}$/
const maximum = 18446744073709551615n

export function parseChannelId(value: unknown): bigint {
  if (typeof value !== 'string' || !canonicalChannel.test(value)) throw new Error('The channel ID is not canonical.')
  const parsed = BigInt(value)
  if (parsed > maximum) throw new Error('The channel ID is out of range.')
  return parsed
}
