import { expect, test } from '@playwright/test'

test('production worker encodes exact packet metadata and decodes bounded PCM', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'The real WebCodecs qualification runs in Chromium here.')
  await page.goto('/')
  const result = await page.evaluate(async () => {
    const worker = new Worker('/src/workers/audio.worker.ts', { type: 'module' })
    const messages: any[] = []
    worker.addEventListener('message', event => messages.push(event.data))
    worker.postMessage({ type: 'capability' })
    const waitFor = async (predicate: () => boolean, timeout = 10000) => { const start = performance.now(); while (!predicate()) { if (performance.now() - start > timeout) throw new Error('worker timeout'); await new Promise(resolve => setTimeout(resolve, 10)) } }
    await waitFor(() => messages.some(message => message.type === 'capability'))
    const supported = messages.find(message => message.type === 'capability')?.supported === true
    if (!supported) { worker.terminate(); return { supported } }
    worker.postMessage({ type: 'start-transmit', channelId: '18446744073709551615' })
    for (let index = 0; index < 3; index++) { const pcm = new Float32Array(960); worker.postMessage({ type: 'pcm', pcm }, [pcm.buffer]) }
    await waitFor(() => messages.filter(message => message.type === 'packet').length === 3)
    const packets = messages.filter(message => message.type === 'packet').map(message => message.packet as ArrayBuffer)
    const metadata = packets.map(packet => { const view = new DataView(packet); return { channel: view.getBigUint64(8).toString(), sequence: view.getUint32(16), timestamp: view.getUint32(20), payloadLength: view.getUint16(24) } })
    messages.length = 0
    packets.forEach((packet, index) => { const bytes = new Uint8Array(packet); const length = new DataView(packet).getUint16(24); worker.postMessage({ type: 'media', packet: { kind: 1, channelId: 1n, sequence: index, timestamp: index * 960, payload: bytes.slice(32, 32 + length) } }) })
    await waitFor(() => messages.filter(message => message.type === 'pcm').reduce((sum, message) => sum + message.pcm.length, 0) >= 2880)
    const decodedFrames = messages.filter(message => message.type === 'pcm').reduce((sum, message) => sum + message.pcm.length, 0)
    worker.terminate()
    return { supported, metadata, decodedFrames }
  })
  expect(result.supported).toBe(true)
  expect(result.metadata).toEqual([
    { channel: '18446744073709551615', sequence: 0, timestamp: 0, payloadLength: expect.any(Number) },
    { channel: '18446744073709551615', sequence: 1, timestamp: 960, payloadLength: expect.any(Number) },
    { channel: '18446744073709551615', sequence: 2, timestamp: 1920, payloadLength: expect.any(Number) },
  ])
  expect(result.metadata!.every(packet => packet.payloadLength >= 1 && packet.payloadLength <= 1168)).toBe(true)
  expect(result.decodedFrames).toBeGreaterThanOrEqual(2880)
})
