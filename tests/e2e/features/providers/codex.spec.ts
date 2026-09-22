import { test, expect } from '../../core/fixtures/base.fixture'

test('Codex device onboarding, replica polling, disconnect, and errors', async ({ page }) => {
  const provider = { name: 'codex', keys: [], network_config: {}, concurrency_and_buffer_size: {}, provider_status: 'active' }
  await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
  await page.route('**/api/providers/codex', route => route.fulfill({ json: provider }))
  let state = 'disconnected'
  let polls = 0
  let fail = false
  await page.route('**/api/codex/connections**', async route => {
    expect(route.request().headers()['x-bf-vk']).toBe('sk-bf-fixture-ui')
    expect(route.request().url()).not.toContain('sk-bf-fixture-ui')
    if (fail) { await route.fulfill({ status: 503, json: { error: 'fixture-only' } }); return }
    const path = new URL(route.request().url()).pathname
    if (route.request().method() === 'DELETE') state = 'disconnected'
    else if (path.endsWith('/poll')) { polls++; state = polls === 1 ? 'polling' : 'connected' }
    else if (route.request().method() === 'POST') state = 'pending'
    await route.fulfill({ json: {
      state, ...(state === 'disconnected' ? {} : { id: 'fixture-id' }),
      ...(state === 'pending' ? { user_code: 'TEST-ONLY', verification_url: 'https://auth.openai.com/codex/device', interval_seconds: 1, expires_at: new Date(Date.now() + 900000).toISOString() } : {}),
    } })
  })
  await page.goto('/workspace/providers')
  await expect(page.getByTestId('codex-onboarding')).toBeVisible()
  // The setup checklist is unrelated to this form and can cover its actions.
  const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await closeSetup.isVisible()) await closeSetup.click()
  await page.getByTestId('codex-gateway-key').fill('sk-bf-fixture-ui')
  await expect(page.getByTestId('codex-gateway-key')).toHaveAttribute('type', 'password')
  await page.getByTestId('codex-check-status').click()
  await page.getByTestId('codex-connect').click()
  await expect(page.getByTestId('codex-device-code')).toContainText('TEST-ONLY')
  await expect(page.getByRole('link', { name: 'Open OpenAI device authorization' })).toHaveAttribute('href', 'https://auth.openai.com/codex/device')
  if (process.env.CODEX_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.CODEX_SCREENSHOT_DIR}/codex-pending.png` })
  await expect(page.getByTestId('codex-connected')).toBeVisible({ timeout: 15000 })
  expect(polls).toBe(2)
  if (process.env.CODEX_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.CODEX_SCREENSHOT_DIR}/codex-connected.png` })
  await page.getByTestId('codex-disconnect').click()
  await expect(page.getByTestId('codex-status')).toHaveText('disconnected')
  fail = true
  await page.getByTestId('codex-check-status').click()
  await expect(page.getByTestId('codex-error')).toContainText('Configure encrypted database storage')
  if (process.env.CODEX_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.CODEX_SCREENSHOT_DIR}/codex-error.png` })
  expect(await page.evaluate(() => JSON.stringify(localStorage) + JSON.stringify(sessionStorage))).not.toContain('sk-bf-fixture-ui')
})
