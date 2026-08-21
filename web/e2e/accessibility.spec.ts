import { expect, test } from '@playwright/test'

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/session', route => route.fulfill({ json: { api_version: 1, data: { authenticated: false, passkey_available: false } } }))
  await page.route('**/api/v1/public/status', route => route.fulfill({ json: { api_version: 1, data: { health: 'ok', ready: true, reflector: { id: 'TEST', display_name: 'Test Reflector' }, client_count: 2, floor: { active: false }, recording: { available: true, quota_full: false }, server_time: new Date().toISOString() } } }))
})

test('public dashboard uses user-initiated audio', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Live channel' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Listen live' })).toBeVisible()
  await expect(page.getByText('Channel idle')).toBeVisible()
})

test('login has visible labels and a bounded error', async ({ page }) => {
  await page.route('**/api/v1/auth/login', route => route.fulfill({ status: 401, json: { code: 'invalid_credentials', message: 'invalid' } }))
  await page.goto('/login')
  await page.getByLabel('Username').fill('unknown')
  await page.getByLabel('Password').fill('wrong')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toHaveText('Sign-in failed. Check your credentials and try again.')
})
