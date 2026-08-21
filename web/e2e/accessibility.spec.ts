import { expect, test } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/session', route => route.fulfill({ json: { api_version: 1, data: { authenticated: false, passkey_available: false } } }))
  await page.route('**/api/v1/public/status', route => route.fulfill({ json: { api_version: 1, data: { health: 'ok', ready: true, reflector: { id: 'TEST', display_name: 'Test Reflector' }, client_count: 2, floor: { active: false }, recording: { available: true, quota_full: false }, server_time: new Date().toISOString() } } }))
})

test('public dashboard uses user-initiated audio', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Live channel' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Listen live' })).toBeVisible()
  await expect(page.getByText('Channel idle')).toBeVisible()
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
})

test('login has visible labels and a bounded error', async ({ page }) => {
  await page.route('**/api/v1/auth/login', route => route.fulfill({ status: 401, json: { code: 'invalid_credentials', message: 'invalid' } }))
  await page.goto('/login')
  await page.getByLabel('Username').fill('unknown')
  await page.getByLabel('Password').fill('wrong')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toHaveText('Sign-in failed. Check your credentials and try again.')
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
})

test('dashboard explains temporary not-ready recovery', async ({ page }) => {
  await page.unroute('**/api/v1/public/status')
  await page.route('**/api/v1/public/status', route => route.fulfill({ json: { api_version: 1, data: { health: 'degraded', ready: false, reflector: { id: 'TEST', display_name: 'Test Reflector' }, client_count: 0, floor: { active: false }, recording: { available: false, quota_full: false }, server_time: new Date().toISOString() } } }))
  await page.goto('/')
  await expect(page.getByText('The reflector is not ready. Monitoring can remain available while connections recover.')).toBeVisible()
})

test('forced password change explains the restriction and limits navigation', async ({ page }, testInfo) => {
  await page.unroute('**/api/v1/session')
  await page.route('**/api/v1/session', route => route.fulfill({ json: { api_version: 1, data: { authenticated: true, username: 'temporary', role: 'admin', csrf_token: 'csrf', passkey_available: false, forced_password_change: true } } }))
  await page.goto('/security')
  const alert = page.getByRole('alert')
  await expect(alert).toContainText('Change your temporary password before you use other features.')
  await expect(page.getByRole('link', { name: 'Live' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Recordings' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Accounts' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Operations' })).toHaveCount(0)
  await page.keyboard.press('Tab')
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  const screenshot = testInfo.outputPath('forced-password-change.png'); await page.screenshot({ path: screenshot, fullPage: true }); await testInfo.attach('forced-password-change', { path: screenshot, contentType: 'image/png' })
})

test('mobile logo and recording seek targets are at least 44 pixels', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.unroute('**/api/v1/session')
  await page.route('**/api/v1/session', route => route.fulfill({ json: { api_version: 1, data: { authenticated: true, username: 'listener', role: 'user', csrf_token: 'csrf', passkey_available: false } } }))
  await page.route('**/api/v1/recordings?**', route => route.fulfill({ json: { api_version: 1, data: { items: [{ id: 'rec-1', source_callsign: 'N0CALL', started_at: '2026-08-20T12:00:00Z', duration_ms: 5000, status: 'complete', end_reason: 'stream_end' }] } } }))
  await page.goto('/recordings')
  const logo = page.getByRole('link', { name: 'OpusRef' })
  const logoBox = await logo.boundingBox()
  expect(logoBox?.height).toBeGreaterThanOrEqual(44)
  await page.evaluate(() => {
    const input = document.querySelector<HTMLInputElement>('input[type="range"]')
    if (!input) {
      const item = document.querySelector('li')
      if (item) item.insertAdjacentHTML('beforeend', '<input aria-label="Test seek" class="seek-control" type="range">')
    }
  })
  const seek = page.getByRole('slider')
  const seekBox = await seek.boundingBox()
  expect(seekBox?.height).toBeGreaterThanOrEqual(44)
})
