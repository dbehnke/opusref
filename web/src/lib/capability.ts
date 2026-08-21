export interface AudioCapability { supported: boolean; reason?: string }

type CodecSupport = { supported?: boolean }
type CodecClass = { isConfigSupported(config: AudioEncoderConfig | AudioDecoderConfig): Promise<CodecSupport> }

export async function checkAudioCapability(scope: typeof globalThis = globalThis): Promise<AudioCapability> {
  const encoder = (scope as unknown as { AudioEncoder?: CodecClass }).AudioEncoder
  const decoder = (scope as unknown as { AudioDecoder?: CodecClass }).AudioDecoder
  if (!encoder || !decoder || !scope.AudioContext || !scope.Worker || !scope.AudioWorkletNode) return { supported: false, reason: 'This browser does not provide the required audio features.' }
  const config = { codec: 'opus', sampleRate: 48000, numberOfChannels: 1 }
  try {
    const [encode, decode] = await Promise.all([encoder.isConfigSupported({ ...config, bitrate: 24000 }), decoder.isConfigSupported(config)])
    if (!encode.supported || !decode.supported) return { supported: false, reason: 'This browser does not support raw Opus audio.' }
    const context = new AudioContext({ sampleRate: 48000 })
    const rate = context.sampleRate
    await context.close()
    return rate === 48000 ? { supported: true } : { supported: false, reason: 'This device cannot use a 48 kHz audio context.' }
  } catch { return { supported: false, reason: 'The browser could not start its audio system.' } }
}
