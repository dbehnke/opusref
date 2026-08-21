import { expect, test } from '@playwright/test'
import { WebSocketServer } from 'ws'

test('browser socket exchanges strict control and ORWB media without compression', async ({ page }) => {
  const server = new WebSocketServer({ port: 0, perMessageDeflate: false })
  await new Promise<void>(resolve => server.once('listening', resolve))
  const address = server.address()
  if (typeof address === 'string' || !address) throw new Error('WebSocket server did not bind')
  let hello: any
  let extensions = 'not-connected'
  server.on('connection', socket => {
    extensions = socket.extensions
    socket.once('message', data => {
      hello = JSON.parse(data.toString())
      socket.send(JSON.stringify({ api_version: 1, type: 'hello_ok', request_id: hello.request_id, body: { authenticated: false, ptt_available: false, passkey_available: false } }))
      const packet = Buffer.alloc(33); packet.write('ORWB', 0); packet.writeUInt8(1, 4); packet.writeUInt8(1, 5); packet.writeBigUInt64BE(1n, 8); packet.writeUInt16BE(1, 24); packet.writeUInt8(0xf8, 32)
      socket.send(packet)
    })
  })
  try {
    await page.goto('/')
    const received = await page.evaluate(async endpoint => {
      // @ts-expect-error Vite serves this source module during browser tests.
      const { OpusRefSocket } = await import('/src/lib/socket.ts')
      const socket = new OpusRefSocket(endpoint)
      const events: any[] = []
      socket.addEventListener('event', (event: Event) => {
        const detail = (event as CustomEvent).detail
        if (detail.control) events.push({ type: detail.control.type })
        if (detail.media) { events.push({ kind: detail.media.kind, channel: detail.media.channelId.toString(), payload: [...detail.media.payload] }); socket.close() }
        if (detail.closed) events.push({ closeCode: detail.closed.code, reason: detail.closed.reason })
      })
      socket.connect({ encoder: true, decoder: true, context_rate: 48000 })
      const start = performance.now(); while (events.length < 3) { if (performance.now() - start > 5000) throw new Error('socket timeout'); await new Promise(resolve => setTimeout(resolve, 10)) }
      socket.close()
      return events
    }, `http://127.0.0.1:${address.port}/api/v1/ws`)
    expect(hello).toMatchObject({ api_version: 1, type: 'hello', body: { audio: { encoder: true, decoder: true, context_rate: 48000 } } })
    expect(extensions).toBe('')
    expect(received.slice(0, 2)).toEqual([{ type: 'hello_ok' }, { kind: 1, channel: '1', payload: [0xf8] }])
    expect(received[2]).toEqual({ closeCode: 1000, reason: 'page_close' })
  } finally { await new Promise<void>(resolve => server.close(() => resolve())) }
})

test('browser socket preserves playback controls, partial status, and media', async ({ page }) => {
  const server = new WebSocketServer({ port: 0, perMessageDeflate: false })
  await new Promise<void>(resolve => server.once('listening', resolve))
  const address = server.address(); if (typeof address === 'string' || !address) throw new Error('WebSocket server did not bind')
  const requests: any[] = []
  server.on('connection', socket => socket.on('message', data => {
    const request = JSON.parse(data.toString()); requests.push(request)
    if (request.type === 'hello') socket.send(JSON.stringify({ api_version: 1, type: 'hello_ok', request_id: request.request_id, body: { authenticated: true, role: 'user', ptt_available: true, passkey_available: false } }))
    if (request.type === 'playback_open') {
      socket.send(JSON.stringify({ api_version: 1, type: 'playback_opened', request_id: request.request_id, body: { channel_id: '18446744073709551615', recording_id: 'recording-1', duration_ms: 4000, status: 'partial' } }))
      const packet = Buffer.alloc(33); packet.write('ORWB', 0); packet.writeUInt8(1, 4); packet.writeUInt8(3, 5); packet.writeBigUInt64BE(18446744073709551615n, 8); packet.writeUInt32BE(0, 16); packet.writeUInt32BE(0, 20); packet.writeUInt16BE(1, 24); packet.writeUInt8(0xf8, 32); socket.send(packet)
    }
    if (request.type === 'playback_seek') {
      socket.send(JSON.stringify({ api_version: 1, type: 'playback_state', request_id: request.request_id, body: { channel_id: request.body.channel_id, state: 'playing', elapsed_ms: request.body.elapsed_ms } }))
      const packet = Buffer.alloc(34); packet.write('ORWB', 0); packet.writeUInt8(1, 4); packet.writeUInt8(3, 5); packet.writeBigUInt64BE(18446744073709551615n, 8); packet.writeUInt32BE(0, 16); packet.writeUInt32BE(95040, 20); packet.writeUInt16BE(2, 24); packet.set([0xf8, 0xff], 32); socket.send(packet)
    }
  }))
  try {
    await page.goto('/')
    const result = await page.evaluate(async endpoint => {
      // @ts-expect-error Vite serves this source module during browser tests.
      const { OpusRefSocket } = await import('/src/lib/socket.ts')
      const socket = new OpusRefSocket(endpoint); const events: any[] = []
      socket.addEventListener('event', (event: Event) => { const detail = (event as CustomEvent).detail; if (detail.control) { events.push(detail.control); if (detail.control.type === 'hello_ok') socket.send('playback_open', { recording_id: 'recording-1' }); if (detail.control.type === 'playback_opened') socket.send('playback_seek', { channel_id: detail.control.body.channel_id, elapsed_ms: 2000 }) } if (detail.media) events.push({ type: 'media', kind: detail.media.kind, channel_id: detail.media.channelId.toString(), sequence: detail.media.sequence, timestamp: detail.media.timestamp, payload: [...detail.media.payload] }) })
      socket.connect({ encoder: true, decoder: true, context_rate: 48000 }, 'csrf')
      const start = performance.now(); while (!events.some(event => event.type === 'playback_state') || events.filter(event => event.type === 'media').length < 2) { if (performance.now() - start > 5000) throw new Error('playback timeout'); await new Promise(resolve => setTimeout(resolve, 10)) }
      socket.close(); return events.map(event => event.type === 'media' ? event : { type: event.type, body: event.body })
    }, `http://127.0.0.1:${address.port}/api/v1/ws`)
    expect(requests.map(request => request.type)).toEqual(['hello', 'playback_open', 'playback_seek'])
    expect(requests[2].body).toEqual({ channel_id: '18446744073709551615', elapsed_ms: 2000 })
    expect(result).toContainEqual({ type: 'playback_opened', body: { channel_id: '18446744073709551615', recording_id: 'recording-1', duration_ms: 4000, status: 'partial' } })
    expect(result).toContainEqual({ type: 'playback_state', body: { channel_id: '18446744073709551615', state: 'playing', elapsed_ms: 2000 } })
    expect(result.filter(event => event.type === 'media')).toEqual([
      { type: 'media', kind: 3, channel_id: '18446744073709551615', sequence: 0, timestamp: 0, payload: [0xf8] },
      { type: 'media', kind: 3, channel_id: '18446744073709551615', sequence: 0, timestamp: 95040, payload: [0xf8, 0xff] },
    ])
  } finally { await new Promise<void>(resolve => server.close(() => resolve())) }
})

test('open-access browser session downgrades immediately after revocation', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'The production audio capability path runs in Chromium here.')
  const server = new WebSocketServer({ port: 0, perMessageDeflate: false })
  await new Promise<void>(resolve => server.once('listening', resolve))
  const address = server.address(); if (typeof address === 'string' || !address) throw new Error('WebSocket server did not bind')
  server.on('connection', socket => socket.once('message', data => { const hello = JSON.parse(data.toString()); socket.send(JSON.stringify({ api_version: 1, type: 'hello_ok', request_id: hello.request_id, body: { authenticated: true, role: 'user', ptt_available: true, passkey_available: false } })); setTimeout(() => socket.send(JSON.stringify({ api_version: 1, type: 'error', body: { code: 'session_invalid', text: 'Your session ended. Live listening remains available.' } })), 50) }))
  try {
    await page.goto('/')
    const state = await page.evaluate(async endpoint => {
      // @ts-expect-error Vite serves these source modules during browser tests.
      const { OpusRefSocket } = await import('/src/lib/socket.ts')
      // @ts-expect-error Vite serves these source modules during browser tests.
      const { BrowserAudioSession } = await import('/src/lib/audio-session.ts')
      const audio = new BrowserAudioSession('csrf', new OpusRefSocket(endpoint)); let revoked = false
      audio.addEventListener('session-invalid', () => { revoked = true })
      if (!await audio.start()) throw new Error(audio.state.error)
      const start = performance.now(); while (!revoked) { if (performance.now() - start > 5000) throw new Error('revocation timeout'); await new Promise(resolve => setTimeout(resolve, 10)) }
      const result = { connected: audio.state.connected, listening: audio.state.listening, pttAvailable: audio.state.pttAvailable, revoked }
      await audio.close(); return result
    }, `http://127.0.0.1:${address.port}/api/v1/ws`)
    expect(state).toEqual({ connected: true, listening: true, pttAvailable: false, revoked: true })
  } finally { await new Promise<void>(resolve => server.close(() => resolve())) }
})

test('browser closes on an unmatched response and on control backpressure', async ({ page }) => {
  const server = new WebSocketServer({ port: 0, perMessageDeflate: false })
  await new Promise<void>(resolve => server.once('listening', resolve))
  const address = server.address(); if (typeof address === 'string' || !address) throw new Error('WebSocket server did not bind')
  let connection = 0
  server.on('connection', socket => {
    connection++
    if (connection === 1) socket.once('message', () => socket.send(JSON.stringify({ api_version: 1, type: 'hello_ok', request_id: 'not-pending', body: { authenticated: false, ptt_available: false } })))
  })
  try {
    await page.goto('/')
    const result = await page.evaluate(async endpoint => {
      // @ts-expect-error Vite serves this source module during browser tests.
      const { OpusRefSocket } = await import('/src/lib/socket.ts')
      async function connectAndClose(action?: (socket: InstanceType<typeof OpusRefSocket>) => void) {
        const socket = new OpusRefSocket(endpoint)
        const closed = new Promise<any>(resolve => socket.addEventListener('event', (event: Event) => { const detail = (event as CustomEvent).detail; if (detail.closed) resolve(detail.closed) }))
        socket.connect({ encoder: true, decoder: true, context_rate: 48000 })
        if (action) { await new Promise(resolve => setTimeout(resolve, 50)); action(socket) }
        return await closed
      }
      const unmatched = await connectAndClose()
      const overloaded = await connectAndClose(socket => { for (let index = 0; index < 32; index++) { try { socket.send('playback_open', { recording_id: String(index) }) } catch {} } })
      return { unmatched, overloaded }
    }, `http://127.0.0.1:${address.port}/api/v1/ws`)
    expect(result.unmatched.code).toBe(4400)
    expect(result.overloaded.code).toBe(4409)
  } finally { for (const client of server.clients) client.terminate(); await new Promise<void>(resolve => server.close(() => resolve())) }
})
