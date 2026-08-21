import { expect, test } from '@playwright/test'
import { createServer } from 'node:https'
import { readFileSync, statSync } from 'node:fs'
import { extname, join, normalize } from 'node:path'
import { fileURLToPath } from 'node:url'
import { generate } from 'selfsigned'
import { WebSocketServer } from 'ws'

test('production assets run live audio through same-origin HTTPS and WSS', async ({ browser, browserName }) => {
  test.skip(browserName !== 'chromium', 'The executable raw Opus production path runs in Chromium here.')
  const certificate = await generate([{ name: 'commonName', value: 'localhost' }], { algorithm: 'sha256', keySize: 2048, days: 1, extensions: [{ name: 'subjectAltName', altNames: [{ type: 2, value: 'localhost' }] }] })
  const dist = fileURLToPath(new URL('../dist', import.meta.url))
  const status = { health: 'degraded', ready: true, reflector: { id: 'TLS', display_name: 'TLS Test' }, client_count: 1, floor: { active: false }, recording: { available: false, quota_full: true }, server_time: new Date().toISOString() }
  const server = createServer({ key: certificate.private, cert: certificate.cert }, (request, response) => {
    response.setHeader('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; worker-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
    response.setHeader('X-Content-Type-Options', 'nosniff'); response.setHeader('Referrer-Policy', 'no-referrer'); response.setHeader('Cross-Origin-Opener-Policy', 'same-origin'); response.setHeader('Permissions-Policy', 'microphone=(self), camera=(), geolocation=()')
    if (request.url === '/api/v1/session') { response.setHeader('Content-Type', 'application/json'); response.end(JSON.stringify({ api_version: 1, data: { authenticated: false, passkey_available: false } })); return }
    if (request.url === '/api/v1/public/status') { response.setHeader('Content-Type', 'application/json'); response.end(JSON.stringify({ api_version: 1, data: status })); return }
    const relative = request.url === '/' ? 'index.html' : normalize((request.url ?? '/').split('?')[0]!).replace(/^\/+/, '')
    const file = join(dist, relative)
    try { if (!file.startsWith(dist) || !statSync(file).isFile()) throw new Error('not found'); response.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' } as Record<string, string>)[extname(file)] ?? 'application/octet-stream'); response.end(readFileSync(file)) } catch { response.statusCode = 404; response.end('not found') }
  })
  const wss = new WebSocketServer({ server, path: '/api/v1/ws', perMessageDeflate: false })
  let origin = ''; let extensions = ''
  wss.on('connection', (socket, request) => { origin = request.headers.origin ?? ''; extensions = socket.extensions; socket.once('message', data => { const hello = JSON.parse(data.toString()); socket.send(JSON.stringify({ api_version: 1, type: 'hello_ok', request_id: hello.request_id, body: { authenticated: false, ptt_available: false, passkey_available: false, limits: { media_bytes: 1200, control_bytes: 16384, live_queue_packets: 64 }, status } })); setTimeout(() => socket.close(1012, 'restart'), 250) }) })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const address = server.address(); if (!address || typeof address === 'string') throw new Error('HTTPS server did not bind')
  const context = await browser.newContext({ ignoreHTTPSErrors: true, permissions: ['microphone'] }); const page = await context.newPage()
  try {
    const response = await page.goto(`https://localhost:${address.port}/`)
    expect(response?.headers()['content-security-policy']).toContain("frame-ancestors 'none'")
    await page.getByRole('button', { name: 'Listen live' }).click()
    await expect(page.getByRole('button', { name: 'Stop listening' })).toBeVisible()
    await expect(page.getByText('Archive quota full. Live audio remains available. New recordings are stopped.')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Listen live' })).toBeVisible({ timeout: 5000 })
    await expect(page.getByText('The live connection closed. Select Listen live to reconnect.')).toBeVisible()
    expect(origin).toBe(`https://localhost:${address.port}`); expect(extensions).toBe('')
  } finally { await context.close(); await new Promise<void>(resolve => wss.close(() => server.close(() => resolve()))) }
})
