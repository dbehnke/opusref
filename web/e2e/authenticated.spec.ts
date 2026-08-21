import { expect, test } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

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

test('operations shows bounded operator alerts with refresh, empty, and accessible states', async ({ page }, testInfo) => {
  await page.route('**/api/v1/admin/clients?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  await page.route('**/api/v1/admin/audit?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  let eventLoads = 0
  await page.route('**/api/v1/admin/events', route => { const load = eventLoads++; if (load === 2) return route.fulfill({ status: 503, json: { code: 'unavailable', message: 'unavailable' } }); return route.fulfill({ json: envelope({ items: load === 0 ? [{ id: 7, time: '2026-08-21T12:00:00Z', kind: 'archive_quota', severity: 'warning', message: 'Archive quota is full.' }] : [] }) }) })
  await page.goto('/admin/operations')
  await expect(page.getByText('No clients are connected.')).toBeVisible()
  await expect(page.getByText('No audit events are available.')).toBeVisible()
  await expect(page.getByText('Archive quota is full.')).toBeVisible()
  await expect(page.getByText('warning', { exact: true })).toBeVisible()
  await expect(page.getByText('Available', { exact: true }).first()).toBeVisible()
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  const operationsScreenshot = testInfo.outputPath('operations-alerts-desktop.png'); await page.screenshot({ path: operationsScreenshot, fullPage: true }); await testInfo.attach('operations-alerts-desktop', { path: operationsScreenshot, contentType: 'image/png' })
  await page.getByRole('button', { name: 'Refresh' }).click()
  await expect(page.getByText('No operator alerts are available.')).toBeVisible()
  await page.getByRole('button', { name: 'Refresh' }).click()
  await expect(page.getByText('Operator alerts are unavailable. Select Refresh to try again.')).toBeVisible()
})

test('mobile account cards keep every action reachable by touch and keyboard', async ({ page }, testInfo) => {
  await page.route('**/api/v1/admin/accounts?**', route => route.fulfill({ json: envelope({ items: [{ id: 'user-1', username: 'operator', role: 'user', source_callsign: 'N0CALL', disabled: false, forced_password_change: false }] }) }))
  for (const width of [320, 390]) {
    await page.setViewportSize({ width, height: 900 })
    await page.goto('/admin/accounts')
    await page.getByLabel('Your current password').fill('a secure administrator password')
    const account = page.getByRole('article', { name: 'Account operator' })
    await expect(account).toBeVisible()
    const names = ['Edit', 'Disable', 'Revoke sessions', 'Clear passkeys', 'Delete']
    for (const name of names) {
      const control = account.getByRole('button', { name })
      await control.scrollIntoViewIfNeeded()
      await expect(control).toBeInViewport()
      expect(await control.evaluate(element => element.tabIndex)).toBe(0)
      await control.focus()
      await expect(control).toBeFocused()
      const box = await control.boundingBox()
      expect(box?.height).toBeGreaterThanOrEqual(44)
      expect(box?.x).toBeGreaterThanOrEqual(0)
      expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(width)
    }
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
    await page.evaluate(() => { (document.activeElement as HTMLElement | null)?.blur(); window.scrollTo(0, 0) })
    const accountScreenshot = testInfo.outputPath(`account-actions-${width}px.png`); await page.screenshot({ path: accountScreenshot, fullPage: true }); await testInfo.attach(`account-actions-${width}px`, { path: accountScreenshot, contentType: 'image/png' })
  }
})

test('passkey removal traps focus on the safe action and restores focus', async ({ page }, testInfo) => {
  await page.route('**/api/v1/me/sessions?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  await page.route('**/api/v1/me/passkeys?**', route => route.fulfill({ json: envelope({ items: [{ id: 'key-1', name: 'Travel key', created_at: '2026-08-20T12:00:00Z' }] }) }))
  await page.route('**/api/v1/me/reauth/password', route => route.fulfill({ json: envelope({ reauth_token: 'proof' }) }))
  let removed = false
  await page.route('**/api/v1/me/passkeys/key-1', route => { removed = route.request().method() === 'DELETE'; return route.fulfill({ json: envelope({}) }) })
  for (const [width, cancelWithEscape] of [[1440, true], [390, false]] as const) {
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 })
    await page.goto('/security')
    await page.getByLabel('Current password').fill('a secure password')
    const origin = page.getByRole('button', { name: 'Remove Travel key' })
    await origin.click()
    const dialog = page.getByRole('alertdialog', { name: 'Remove Travel key?' })
    const cancel = dialog.getByRole('button', { name: 'Cancel' })
    const confirm = dialog.getByRole('button', { name: 'Remove passkey' })
    await expect(dialog).toContainText('This passkey cannot be used again.')
    await expect(cancel).toBeFocused()
    await expect(page.locator('#app')).toHaveJSProperty('inert', true)
    await expect(page.locator('.skip-link')).toHaveJSProperty('inert', true)
    await page.keyboard.press('Shift+Tab'); await expect(confirm).toBeFocused()
    await page.keyboard.press('Tab'); await expect(cancel).toBeFocused()
    await page.evaluate(() => (document.querySelector('#app button') as HTMLButtonElement | null)?.focus())
    await expect(cancel).toBeFocused()
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
    const screenshot = testInfo.outputPath(`passkey-dialog-${width}px.png`); await page.screenshot({ path: screenshot }); await testInfo.attach(`passkey-dialog-${width}px`, { path: screenshot, contentType: 'image/png' })
    if (cancelWithEscape) await page.keyboard.press('Escape'); else await cancel.click()
    expect(removed).toBe(false)
    await expect(origin).toBeFocused()
    await expect(page.locator('#app')).toHaveJSProperty('inert', false)
    await expect(page.locator('.skip-link')).toHaveJSProperty('inert', false)
  }
  await page.getByRole('button', { name: 'Remove Travel key' }).click()
  const dialog = page.getByRole('alertdialog', { name: 'Remove Travel key?' })
  await dialog.getByRole('button', { name: 'Remove passkey' }).click()
  await expect.poll(() => removed).toBe(true)
  await expect(page.getByRole('button', { name: 'Add passkey' })).toBeFocused()
})

test('submitted passkey removal ignores repeated dismissal until failure or success', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.route('**/api/v1/me/sessions?**', route => route.fulfill({ json: envelope({ items: [] }) }))
  await page.route('**/api/v1/me/passkeys?**', route => route.fulfill({ json: envelope({ items: [{ id: 'key-1', name: 'Travel key', created_at: '2026-08-20T12:00:00Z' }] }) }))
  await page.route('**/api/v1/me/reauth/password', route => route.fulfill({ json: envelope({ reauth_token: 'proof' }) }))
  let finishDelete: ((success: boolean) => void) | undefined
  let attempts = 0
  await page.route('**/api/v1/me/passkeys/key-1', async route => {
    attempts++
    const success = await new Promise<boolean>(resolve => { finishDelete = resolve })
    await route.fulfill(success ? { json: envelope({}) } : { status: 503, json: { code: 'unavailable', message: 'unavailable' } })
  })
  await page.goto('/security')
  await page.getByLabel('Current password').fill('a secure password')
  const submit = async () => {
    await page.getByRole('button', { name: 'Remove Travel key' }).click()
    await page.getByRole('button', { name: 'Remove passkey' }).click()
    await expect(page.getByRole('status')).toHaveText('Removing passkey…')
    await expect.poll(() => attempts).toBeGreaterThan(0)
  }
  const tryDismiss = async () => {
    await page.keyboard.press('Escape'); await page.keyboard.press('Escape')
    await page.evaluate(() => { const dialog = document.querySelector('dialog'); dialog?.dispatchEvent(new Event('cancel', { cancelable: true })); dialog?.dispatchEvent(new Event('cancel', { cancelable: true })) })
    await expect(page.getByRole('alertdialog')).toBeVisible()
    await expect(page.locator('#app')).toHaveJSProperty('inert', true)
  }
  await submit(); await tryDismiss()
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  const busyScreenshot = testInfo.outputPath('passkey-removal-busy-390px.png')
  await page.screenshot({ path: busyScreenshot, fullPage: true })
  await testInfo.attach('passkey-removal-busy-390px', { path: busyScreenshot, contentType: 'image/png' })
  finishDelete?.(false)
  await expect(page.getByRole('alert')).toHaveText('The passkey was not removed. Confirm your identity and try again.')
  await expect(page.getByRole('button', { name: 'Cancel' })).toBeFocused()
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  const failureScreenshot = testInfo.outputPath('passkey-removal-failure-390px.png')
  await page.screenshot({ path: failureScreenshot, fullPage: true })
  await testInfo.attach('passkey-removal-failure-390px', { path: failureScreenshot, contentType: 'image/png' })
  const nextAttempt = attempts + 1
  await page.getByRole('button', { name: 'Remove passkey' }).click()
  await expect(page.getByRole('status')).toHaveText('Removing passkey…')
  await expect.poll(() => attempts).toBe(nextAttempt)
  await tryDismiss(); finishDelete?.(true)
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add passkey' })).toBeFocused()
})
