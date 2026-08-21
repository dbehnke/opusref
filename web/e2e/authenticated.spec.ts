import { expect, test } from '@playwright/test'

const envelope = (data: unknown) => ({ api_version: 1, data })
const session = { authenticated: true, username: 'admin', role: 'admin', csrf_token: 'csrf', passkey_available: true, forced_password_change: false }

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/session', route => route.fulfill({ json: envelope(session) }))
  await page.route('**/api/v1/public/status', route => route.fulfill({ json: envelope({ health: 'degraded', ready: true, reflector: { id: 'TEST', display_name: 'Test Reflector' }, client_count: 2, floor: { active: false }, recording: { available: true, quota_full: false }, server_time: new Date().toISOString() }) }))
})

test('recording filters and administrator deletion call the locked APIs', async ({ page }) => {
  await page.route('**/api/v1/recordings?**', route => route.fulfill({ json: envelope({ items: [{ id: 'rec-1', source_callsign: 'N0CALL', started_at: '2026-08-20T12:00:00Z', duration_ms: 5000, status: 'partial', end_reason: 'timeout' }] }) }))
  await page.route('**/api/v1/me/reauth/password', route => route.fulfill({ json: envelope({ reauth_token: 'proof' }) }))
  let deleted = false
  await page.route('**/api/v1/admin/recordings/rec-1', route => { deleted = route.request().method() === 'DELETE' && route.request().headers()['x-reauth-token'] === 'proof'; return route.fulfill({ json: envelope({}) }) })
  page.on('dialog', dialog => dialog.accept())
  await page.goto('/recordings')
  await expect(page.getByText('N0CALL')).toBeVisible()
  await page.getByLabel('Administrator password').fill('a secure administrator password')
  await page.getByRole('button', { name: 'Delete' }).click()
  await expect.poll(() => deleted).toBe(true)
})

test('administrator account creation uses reauthentication and PATCH editing', async ({ page }) => {
  let accounts = { items: [{ id: 'user-1', username: 'operator', role: 'user', source_callsign: 'N0CALL', disabled: false, forced_password_change: false }] }
  await page.route('**/api/v1/admin/accounts?**', route => route.fulfill({ json: envelope(accounts) }))
  await page.route('**/api/v1/me/reauth/password', route => route.fulfill({ json: envelope({ reauth_token: 'proof' }) }))
  let created = false
  await page.route('**/api/v1/admin/accounts', route => { created = route.request().method() === 'POST' && route.request().headers()['x-reauth-token'] === 'proof'; return route.fulfill({ json: envelope({}) }) })
  await page.goto('/admin/accounts')
  await page.getByLabel('Your current password').fill('a secure administrator password')
  await page.getByLabel('Username').fill('new.user')
  await page.getByLabel('Source callsign').fill('N1NEW')
  await page.getByLabel('Initial password').fill('a long temporary password')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect.poll(() => created).toBe(true)
})

test('operations composes only the locked status, client, and audit routes', async ({ page }) => {
  await page.route('**/api/v1/admin/clients?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  await page.route('**/api/v1/admin/audit?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  await page.goto('/admin/operations')
  await expect(page.getByText('No clients are connected.')).toBeVisible()
  await expect(page.getByText('No audit events are available.')).toBeVisible()
  await expect(page.getByText('Available', { exact: true }).first()).toBeVisible()
})
