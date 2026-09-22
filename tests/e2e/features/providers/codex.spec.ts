import { test, expect } from '../../core/fixtures/base.fixture'

test('Codex accounts use provider table, isolated usage bars and dashboard onboarding', async ({ page }) => {
  const accounts = [
    { id: 'personal', name: 'Personal', models: ['*'], weight: 1, enabled: true },
    { id: 'work', name: 'Work', models: ['*'], weight: 1, enabled: true },
  ]
  const provider = { name: 'codex', keys: accounts, network_config: {}, concurrency_and_buffer_size: {}, provider_status: 'active' }
  const states: Record<string, string> = { personal: 'connected', work: 'connected' }
  let created = ''
  let polls = 0
  let fail = false
  let checked = false
  await page.route('**/api/providers/codex/keys/personal/refresh-models', route => {
    checked = true
    return route.fulfill({ json: { ...accounts[0], status: 'success' } })
  })
  await page.route('**/api/providers/codex/keys/personal', route => {
    expect(route.request().method()).toBe('PUT')
    accounts[0].enabled = route.request().postDataJSON().enabled
    return route.fulfill({ json: accounts[0] })
  })
  await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
  await page.route('**/api/providers/codex', route => route.fulfill({ json: provider }))
  await page.route('**/api/providers/codex/keys', route => {
    if (route.request().method() === 'POST') {
      const account = route.request().postDataJSON()
      expect(account.value).toBeUndefined()
      accounts.push(account); states[account.id] = 'disconnected'; created = account.id
      return route.fulfill({ json: account })
    }
    return route.fulfill({ json: { keys: accounts, total: accounts.length } })
  })
  await page.route('**/api/codex/connections**', async route => {
    const key = route.request().headers()['x-bf-codex-key']
    expect(route.request().headers()['x-bf-vk']).toBeUndefined()
    expect(accounts.some(account => account.id === key)).toBeTruthy()
    const path = new URL(route.request().url()).pathname
    if (path.endsWith('/usage')) {
      if (fail && key === 'work') return route.fulfill({ status: 502, json: { error: 'unavailable' } })
      return route.fulfill({ json: { plan_type: key === 'work' ? 'pro' : 'plus', checked_at: new Date().toISOString(), rate_limit: {
        allowed: key !== 'work', limit_reached: key === 'work',
        primary_window: { used_percent: key === 'work' ? 100 : 23, limit_window_seconds: 18000, reset_at: 1900000000 },
        secondary_window: { used_percent: key === 'work' ? 95 : 68, limit_window_seconds: 604800, reset_at: 1900100000 },
      } } })
    }
    if (route.request().method() === 'DELETE') states[key] = 'disconnected'
    else if (path.endsWith('/poll')) {
      polls++; states[key] = polls === 1 ? 'polling' : 'connected'
      if (polls === 1) return route.fulfill({ status: 409, json: { error: 'another replica owns polling' } })
    } else if (route.request().method() === 'POST') states[key] = 'pending'
    const state = states[key]
    return route.fulfill({ json: { state, ...(state === 'disconnected' ? {} : { id: `connection-${key}` }),
      ...(state === 'pending' ? { user_code: 'TEST-ONLY', verification_url: 'https://auth.openai.com/codex/device', interval_seconds: 1 } : {}),
    } })
  })
  await page.goto('/workspace/providers')
  await expect(page.getByTestId('keys-table')).toBeVisible({ timeout: 15000 })
  const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await closeSetup.isVisible()) await closeSetup.click()
  const personal = page.getByTestId('codex-usage-personal')
  const work = page.getByTestId('codex-usage-work')
  await expect(personal.getByRole('progressbar', { name: 'Remaining allowance', exact: true })).toHaveAttribute('aria-valuenow', '32')
  await expect(work.getByRole('progressbar', { name: 'Remaining allowance', exact: true })).toHaveAttribute('aria-valuenow', '0')
  await expect(personal.getByRole('button', { name: 'Personal subscription details' })).toHaveAttribute('aria-expanded', 'false')
  await personal.getByRole('button', { name: 'Personal subscription details' }).click()
  await work.getByRole('button', { name: 'Work subscription details' }).click()
  await expect(personal.getByRole('button', { name: 'Check access' })).toBeVisible()
  await personal.getByRole('button', { name: 'Check access' }).click()
  await expect.poll(() => checked).toBe(true)
  await personal.getByRole('button', { name: 'Deactivate', exact: true }).click()
  await expect(personal.getByRole('button', { name: 'Activate', exact: true })).toBeVisible()
  await expect(personal.getByRole('button', { name: 'Check access' })).toBeDisabled()
  await expect(work.getByRole('button', { name: 'Deactivate', exact: true })).toBeVisible()
  await personal.getByRole('button', { name: 'Activate', exact: true }).click()
  await expect(personal.getByRole('button', { name: 'Check access' })).toBeEnabled()
  await expect(personal.getByRole('link', { name: 'ChatGPT usage' })).toHaveAttribute('href', 'https://chatgpt.com/codex/settings/usage')
  await expect(personal.getByRole('progressbar', { name: '5h remaining' })).toHaveAttribute('aria-valuenow', '77')
  await expect(personal.getByRole('progressbar', { name: '7d remaining' })).toHaveAttribute('aria-valuenow', '32')
  await expect(work.getByRole('progressbar', { name: '5h remaining' })).toHaveAttribute('aria-valuenow', '0')
  await expect(work.getByRole('progressbar', { name: '7d remaining' })).toHaveAttribute('aria-valuenow', '5')
  await expect(page.getByText(/not endorsed|permitted coding|Bifrost virtual key/i)).toHaveCount(0)
  if (process.env.CODEX_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.CODEX_SCREENSHOT_DIR}/codex-accounts.png` })
  await page.getByTestId('add-key-btn').click()
  const form = page.getByTestId('key-form')
  await form.getByLabel('Name', { exact: true }).fill('Travel')
  await expect(form.getByLabel('API Key', { exact: true })).toHaveCount(0)
  await page.getByTestId('key-save-btn').click()
  await expect(page.getByTestId('codex-onboarding')).toBeVisible()
  await page.getByTestId('codex-connect').click()
  await expect(page.getByTestId('codex-device-code')).toContainText('TEST-ONLY')
  await expect(page.getByRole('link', { name: 'Continue to OpenAI' })).toHaveAttribute('href', 'https://auth.openai.com/codex/device')
  if (process.env.CODEX_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.CODEX_SCREENSHOT_DIR}/codex-pending.png` })
  await expect(page.getByTestId('codex-connected')).toBeVisible({ timeout: 15000 })
  expect(polls).toBe(2)
  await page.getByTestId('codex-disconnect').click()
  await expect(page.getByTestId('codex-status')).toHaveText('disconnected')
  expect(states.personal).toBe('connected'); expect(states.work).toBe('connected')
  await page.getByTestId('key-cancel-btn').click()
  await expect(page.getByTestId(`codex-usage-${created}`)).toContainText('disconnected')
  fail = true
  await work.getByRole('button', { name: 'Refresh usage' }).click()
  await expect(work).toContainText('Usage unavailable')
  await expect(work.getByRole('progressbar')).toHaveCount(0)
  await expect(personal.getByRole('progressbar', { name: '5h remaining', exact: true })).toHaveAttribute('aria-valuenow', '77')
})
