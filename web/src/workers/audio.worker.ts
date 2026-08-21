/// <reference lib="webworker" />
import { decodePacket, encodePacket, MediaKind, ORWB_MAX_PAYLOAD } from '../lib/orwb'
import { JitterBuffer } from '../lib/jitter'

declare const AudioEncoder: any
declare const AudioDecoder: any
declare const AudioData: any
declare const EncodedAudioChunk: any

const scope = self as DedicatedWorkerGlobalScope
let encoder: any
let decoder: any
let channelId = 0n
let sequence = 0
let timestamp = 0
let socketBufferedAmount = 0
const jitter = new JitterBuffer()

function fail(code: string, message: string) { scope.postMessage({ type: 'error', code, message }) }

async function capability() {
  try {
    const config = { codec: 'opus', sampleRate: 48000, numberOfChannels: 1 }
    const [enc, dec] = await Promise.all([AudioEncoder.isConfigSupported({ ...config, bitrate: 24000 }), AudioDecoder.isConfigSupported(config)])
    scope.postMessage({ type: 'capability', supported: Boolean(enc.supported && dec.supported) })
  } catch { scope.postMessage({ type: 'capability', supported: false }) }
}

function configureEncoder() {
  encoder = new AudioEncoder({
    error: () => fail('encode_failed', 'The browser could not encode audio.'),
    output: (chunk: any) => {
      if (chunk.byteLength < 1 || chunk.byteLength > ORWB_MAX_PAYLOAD) return fail('packet_size', 'The encoded audio packet is too large.')
      const payload = new Uint8Array(chunk.byteLength); chunk.copyTo(payload)
      const packet = encodePacket({ kind: MediaKind.Transmit, channelId, sequence, timestamp, payload })
      sequence = (sequence + 1) >>> 0; timestamp = (timestamp + 960) >>> 0
      scope.postMessage({ type: 'packet', packet }, [packet])
    },
  })
  encoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1, bitrate: 24000, bitrateMode: 'constant', latencyMode: 'realtime', opus: { frameDuration: 20000, complexity: 5, format: 'opus', packetlossperc: 0, useinbandfec: false, usedtx: false } })
}

function configureDecoder() {
  decoder = new AudioDecoder({
    error: () => fail('decode_failed', 'The browser could not decode audio.'),
    output: (audio: any) => {
      const frames = audio.numberOfFrames
      const timestamp = Math.round(audio.timestamp * 48000 / 1000000) >>> 0
      const pcm = new Float32Array(frames); audio.copyTo(pcm, { planeIndex: 0 }); audio.close()
      const result = jitter.push(timestamp, pcm)
      if (result !== 'queued') scope.postMessage({ type: 'jitter-reset', reason: result })
    },
  })
  decoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1 })
}

scope.onmessage = event => {
  const data = event.data
  if (data.type === 'capability') return void capability()
  if (data.type === 'start-transmit') { channelId = BigInt(data.channelId); sequence = 0; timestamp = 0; configureEncoder(); return }
  if (data.type === 'stop-transmit') { encoder?.close(); encoder = undefined; return }
  if (data.type === 'reset-playout') { decoder?.reset(); jitter.reset(); return }
  if (data.type === 'socket-buffer') { socketBufferedAmount = data.bytes; return }
  if (data.type === 'pcm') {
    if (!encoder || encoder.encodeQueueSize >= 4 || socketBufferedAmount >= 65536 || !(data.pcm instanceof Float32Array) || data.pcm.length !== 960) return fail('transmit_overload', 'PTT stopped because the transmit queue was full.')
    const audio = new AudioData({ format: 'f32-planar', sampleRate: 48000, numberOfFrames: 960, numberOfChannels: 1, timestamp: timestamp * 1000000 / 48000, data: data.pcm })
    encoder.encode(audio); audio.close(); scope.postMessage({ type: 'capture-ack' })
    return
  }
  if (data.type === 'media') {
    const packet = data.packet instanceof ArrayBuffer ? decodePacket(data.packet) : data.packet
    if (packet.kind === MediaKind.Transmit) return fail('invalid_direction', 'The server sent an invalid media kind.')
    if (!decoder) configureDecoder()
    if (decoder.decodeQueueSize >= 4) return fail('receive_overload', 'Audio restarted because the receive queue was full.')
    decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: packet.timestamp * 1000000 / 48000, data: packet.payload }))
  }
}

setInterval(() => {
  for (const pcm of jitter.take(performance.now())) scope.postMessage({ type: 'pcm', pcm }, [pcm.buffer])
}, 10)
