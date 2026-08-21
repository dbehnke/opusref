import { expect, test } from '@playwright/test'

const systemURL = process.env.OPUSREF_SYSTEM_URL

test('nginx forwards the production console and same-origin WSS to the Go companion', async ({ browser, browserName }) => {
  test.skip(!systemURL, 'Set OPUSREF_SYSTEM_URL to run the nginx system gate.')
  test.skip(browserName !== 'chromium', 'The system gate runs once in Chromium.')
  const context = await browser.newContext({ ignoreHTTPSErrors: true })
  const page = await context.newPage()
  try {
    let response
    await expect(async () => {
      response = await page.goto(systemURL!, { waitUntil: 'domcontentloaded' })
      expect(response?.status()).toBe(200)
    }).toPass({ timeout: 20_000, intervals: [200, 500, 1000] })
    expect(response!.headers().server).toMatch(/^nginx\//)
    expect(response!.headers()['content-security-policy']).toContain("connect-src 'self'")
    await expect(page.getByText('System Proxy Reflector')).toBeVisible({ timeout: 10_000 })
    await page.getByRole('button', { name: 'Listen live' }).click()
    await expect(page.getByRole('button', { name: 'Stop listening' })).toBeVisible({ timeout: 10_000 })
    await page.getByRole('button', { name: 'Stop listening' }).click()
    await expect(page.getByRole('button', { name: 'Listen live' })).toBeVisible()
  } finally { await context.close() }
})
