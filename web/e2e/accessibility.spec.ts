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
