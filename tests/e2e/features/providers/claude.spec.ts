import { test, expect } from '../../core/fixtures/base.fixture'

for (const width of [390, 1440]) {
  test(`Claude subscription account stays readable at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 })
    const email = 'personal.subscription.account@example.test'
    let state = 'connected'
    let unavailable = false
    const account = { id: 'claude-account', name: 'Personal', models: ['*'], weight: 1, enabled: true }
    const provider = { name: 'claude', keys: [account], network_config: { max_retries: 0 }, concurrency_and_buffer_size: {}, provider_status: 'active' }
    await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
    await page.route('**/api/providers/claude', route => route.fulfill({ json: provider }))
    await page.route('**/api/providers/claude/keys', route => route.fulfill({ json: { keys: [account], total: 1 } }))
    await page.route('**/api/claude/connections**', route => {
      expect(route.request().headers()['x-bf-claude-key']).toBe(account.id)
      expect(route.request().headers()['x-bf-vk']).toBeUndefined()
      if (unavailable) return route.fulfill({ status: 502, json: { error: 'unavailable' } })
      // An account that reports no windows must not get a fabricated bar.
      return route.fulfill({ json: route.request().url().endsWith('/usage') ? {} : { state, email } })
    })
    await page.goto('/workspace/providers')
    const setup = page.getByRole('button', { name: 'Close for now', exact: true })
    if (await setup.isVisible()) await setup.click()
    const row = page.getByTestId('claude-account-claude-account')
    const trigger = row.getByRole('button', { name: `Personal (${email}) subscription details` })
    await expect(trigger).toBeVisible()
    const widgetClose = page.getByTestId('onboarding-widget-close')
    if (await widgetClose.isVisible()) await widgetClose.click()
    await trigger.click()
    await expect(trigger).toHaveAttribute('aria-expanded', 'true')
    await expect(row.getByTestId('claude-account-email')).toHaveText(email)
    expect(await row.evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
    const bounds = (await row.boundingBox())!
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(width)
    await expect(row.getByRole('progressbar')).toHaveCount(0)
    if (process.env.CLAUDE_SCREENSHOT_DIR) {
      await page.screenshot({ path: `${process.env.CLAUDE_SCREENSHOT_DIR}/claude-${width}-page.png` })
      await row.screenshot({ path: `${process.env.CLAUDE_SCREENSHOT_DIR}/claude-${width}-expanded.png` })
    }
    await row.getByRole('button', { name: 'Edit connection' }).click()
    await expect(page.getByTestId('claude-connected')).toContainText(email)
    await expect(page.getByTestId('key-form').getByText('API Key', { exact: true })).toHaveCount(0)
    if (width === 1440) {
      await page.keyboard.press('Escape')
      await page.clock.install()
      // Manual refresh resets the polling interval after the clock is installed.
      await Promise.all([
        page.waitForResponse(response => response.url().endsWith('/api/claude/connections/current')),
        page.getByRole('button', { name: 'Refresh account status' }).click(),
      ])
      state = 'reconnect_required'
      await page.clock.runFor(60000)
      await expect(row.getByText('reconnect required', { exact: true })).toBeVisible()
      unavailable = true
      await page.clock.runFor(60000)
      await expect(row.getByText('Status unavailable', { exact: true })).toBeVisible()
    }
  })
}

test('Claude subscription usage shows 5h and weekly allowance without inventing missing data', async ({ page }) => {
  const account = { id: 'claude-usage', name: 'Personal', models: ['*'], weight: 1, enabled: true }
  const provider = { name: 'claude', keys: [account], network_config: { max_retries: 0 }, concurrency_and_buffer_size: {}, provider_status: 'active' }
  let failUsage = false
  await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
  await page.route('**/api/providers/claude', route => route.fulfill({ json: provider }))
  await page.route('**/api/providers/claude/keys', route => route.fulfill({ json: { keys: [account], total: 1 } }))
  await page.route('**/api/claude/connections/usage', route => {
    expect(route.request().headers()['x-bf-claude-key']).toBe(account.id)
    return failUsage
      ? route.fulfill({ status: 502, json: { error: 'unavailable' } })
      : route.fulfill({ json: {
          five_hour: { utilization: 6, resets_at: new Date(Date.now() + 3 * 3600_000).toISOString() },
          seven_day: { utilization: 81, resets_at: new Date(Date.now() + 4 * 86400_000).toISOString() },
          models: [{ name: 'Fable', utilization: 40 }],
        } })
  })
  await page.route('**/api/claude/connections/current', route => route.fulfill({ json: { state: 'connected', email: 'owner@example.test' } }))
  await page.goto('/workspace/providers')
  const setup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await setup.isVisible()) await setup.click()
  const row = page.getByTestId('claude-account-claude-usage')
  // Headline is the tighter of the session and weekly windows: 100 - 81.
  await expect(row.getByRole('progressbar', { name: 'Remaining allowance' })).toHaveAttribute('aria-valuenow', '19')
  await row.getByRole('button', { name: /subscription details/ }).click()
  const usage = row.getByTestId('claude-usage')
  await expect(usage.getByRole('progressbar', { name: '5h session remaining' })).toHaveAttribute('aria-valuenow', '94')
  await expect(usage.getByRole('progressbar', { name: '7d all models remaining' })).toHaveAttribute('aria-valuenow', '19')
  await expect(usage.getByRole('progressbar', { name: '7d Fable remaining' })).toHaveAttribute('aria-valuenow', '60')
  await expect(usage.getByText(/Resets in about 3 hours/)).toBeVisible()
  await expect(usage.getByRole('progressbar', { name: '7d Opus remaining' })).toHaveCount(0)
  failUsage = true
  await usage.getByRole('button', { name: 'Refresh usage' }).click()
  await expect(usage.getByText('Usage unavailable')).toBeVisible()
  await expect(row.getByRole('progressbar')).toHaveCount(0)
})

test('Claude creates an account before browser login and handles manual code, errors and cancellation', async ({ page }) => {
  const accounts: { id: string; name: string; models: string[]; weight: number; enabled: boolean }[] = []
  const provider = { name: 'claude', keys: accounts, network_config: { max_retries: 0 }, concurrency_and_buffer_size: {}, provider_status: 'active' }
  let state = 'disconnected', id = '', receivedCode = false
  const url = 'https://claude.com/cai/oauth/authorize?state=fixture-state&code_challenge=fixture&code_challenge_method=S256'
  await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
  await page.route('**/api/providers/claude', route => route.fulfill({ json: provider }))
  await page.route('**/api/providers/claude/keys', route => {
    if (route.request().method() === 'POST') {
      const account = route.request().postDataJSON()
      expect(account.value).toBeUndefined()
      expect(account.name).toMatch(/^Claude /)
      accounts.push(account); id = account.id
      return route.fulfill({ json: account })
    }
    return route.fulfill({ json: { keys: accounts, total: accounts.length } })
  })
  await page.route('**/api/claude/connections**', route => {
    expect(id).not.toBe('')
    expect(route.request().headers()['x-bf-claude-key']).toBe(id)
    const path = new URL(route.request().url()).pathname
    if (route.request().method() === 'DELETE') state = 'disconnected'
    else if (path.endsWith('/code')) {
      expect(route.request().postDataJSON()).toEqual({ id: 'login-fixture', code: 'bad#fixture-state' })
      receivedCode = true
      return route.fulfill({ status: 400, json: { error: 'redacted' } })
    } else if (route.request().method() === 'POST') state = 'pending'
    return route.fulfill({ json: { state, ...(state === 'pending' ? { id: 'login-fixture', authorization_url: url, interval_seconds: 1 } : {}) } })
  })
  await page.goto('/workspace/providers')
  const setup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await setup.isVisible()) await setup.click()
  await page.getByTestId('add-key-btn').click()
  await expect(page.getByTestId('claude-onboarding')).toHaveCount(0)
  await page.getByTestId('key-save-btn').click()
  await page.getByTestId('claude-connect').click()
  await expect(page.getByRole('link', { name: 'Open Claude sign-in' })).toHaveAttribute('href', url)
  if (process.env.CLAUDE_SCREENSHOT_DIR) await page.getByTestId('claude-onboarding').screenshot({ animations: 'disabled', path: `${process.env.CLAUDE_SCREENSHOT_DIR}/claude-sign-in.png` })
  await page.getByTestId('claude-authorization-code').fill('bad#fixture-state')
  await page.getByTestId('claude-submit-code').click()
  await expect(page.getByTestId('claude-error')).toContainText('complete code#state')
  expect(receivedCode).toBe(true)
  // A subsequent silent status poll must not erase a manual-code error.
  await page.waitForResponse(response => response.url().endsWith('/api/claude/connections/current'))
  await expect(page.getByTestId('claude-error')).toBeVisible()
  await page.getByTestId('claude-disconnect').click()
  await expect(page.getByTestId('claude-status')).toHaveText('disconnected')
  await expect(page.getByTestId('claude-authorization-code')).toHaveCount(0)
})
