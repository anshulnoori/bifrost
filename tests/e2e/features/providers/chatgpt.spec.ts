import { test, expect } from '../../core/fixtures/base.fixture'

for (const width of [320, 390, 1440]) {
  test(`ChatGPT uses Codex account controls and stays readable at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 })
    await page.addInitScript(() => localStorage.setItem('theme', 'dark'))
    const email = 'personal.subscription.account@example.test'
    const account = { id: 'personal', name: 'Personal', models: ['*'], blacklisted_models: ['gpt-excluded'], weight: 2, enabled: true }
    const provider = { name: 'chatgpt', keys: [account], network_config: {}, concurrency_and_buffer_size: {}, provider_status: 'active' }
    let checked = false
    await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
    await page.route('**/api/providers/chatgpt', route => route.fulfill({ json: provider }))
    await page.route('**/api/providers/chatgpt/keys', route => route.fulfill({ json: { keys: [account], total: 1 } }))
    await page.route('**/api/providers/chatgpt/keys/personal', route => {
      expect(route.request().method()).toBe('PUT')
      Object.assign(account, route.request().postDataJSON())
      return route.fulfill({ json: account })
    })
    await page.route('**/api/providers/chatgpt/keys/personal/refresh-models', route => {
      expect(route.request().method()).toBe('POST')
      checked = true
      return route.fulfill({ json: { ...account, status: 'success' } })
    })
    await page.route('**/api/chatgpt/connections**', route => {
      expect(route.request().headers()['x-bf-chatgpt-key']).toBe('personal')
      return route.fulfill({ json: { state: 'connected', email } })
    })
    await page.route('**/api/codex/**', () => { throw new Error('ChatGPT must not use Codex credentials or usage') })
    await page.goto('/workspace/providers')
    const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
    if (await closeSetup.isVisible()) await closeSetup.click()
    const closeWidget = page.getByTestId('onboarding-widget-close')
    if (await closeWidget.isVisible()) await closeWidget.click()
    const row = page.getByTestId('chatgpt-account-personal')
    const trigger = row.getByRole('button', { name: `Personal (${email}) subscription details` })
    await expect(trigger).toHaveAttribute('aria-expanded', 'false')
    await trigger.click()
    await expect(trigger).toHaveAttribute('aria-expanded', 'true')
    await expect(row.getByRole('button', { name: 'Check access', exact: true })).toBeVisible()
    await expect(row).toContainText('Excludes gpt-excluded')
    await expect(row).toContainText('Routing weight: 2')
    await expect(row.getByRole('link', { name: 'ChatGPT usage' })).toHaveAttribute('href', 'https://chatgpt.com/settings/usage')
    await expect(row.getByRole('progressbar')).toHaveCount(0)
    await expect(row.getByText('Routing reserve', { exact: true })).toHaveCount(0)
    expect(await row.evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
    if (width === 1440) {
      await expect(page.getByTestId('provider-item-chatgpt').locator('svg path')).toHaveAttribute('fill', 'white')
      await row.getByRole('button', { name: 'Check access', exact: true }).click()
      await expect.poll(() => checked).toBe(true)
      await row.getByRole('button', { name: 'Deactivate', exact: true }).click()
      await expect(row.getByRole('button', { name: 'Activate', exact: true })).toBeVisible()
      await expect(row.getByRole('button', { name: 'Check access', exact: true })).toBeDisabled()
      await expect(trigger).toContainText('Inactive')
      await row.getByRole('button', { name: 'Activate', exact: true }).click()
      await expect(row.getByRole('button', { name: 'Check access', exact: true })).toBeEnabled()
    }
    if (await closeWidget.isVisible()) await closeWidget.click()
    await expect(closeWidget).toBeHidden()
    if (process.env.CHATGPT_SCREENSHOT_DIR) await page.screenshot({ animations: 'disabled', path: `${process.env.CHATGPT_SCREENSHOT_DIR}/chatgpt-${width}-expanded.png` })
  })
}

test('ChatGPT accounts use keyless plan authorization and keep callback codes out of URLs', async ({ page }) => {
  const account = { id: 'personal', name: 'Personal', models: ['*'], weight: 1, enabled: true }
  const provider = { name: 'chatgpt', keys: [account], network_config: {}, concurrency_and_buffer_size: {}, status: 'active' }
  let state = 'disconnected'
  let completed = false
  await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
  await page.route('**/api/providers/chatgpt', route => route.fulfill({ json: provider }))
  await page.route('**/api/providers/chatgpt/keys', route => route.fulfill({ json: { keys: [account], total: 1 } }))
  await page.route('**/api/chatgpt/connections**', async route => {
    expect(route.request().headers()['x-bf-chatgpt-key']).toBe('personal')
    expect(route.request().headers()['x-bf-vk']).toBeUndefined()
    const path = new URL(route.request().url()).pathname
    if (path.endsWith('/complete')) {
      expect(route.request().method()).toBe('POST')
      expect(route.request().postDataJSON()).toEqual({ callback_url: 'http://127.0.0.1:1455/auth/callback?code=test-code&state=test-state' })
      expect(route.request().url()).not.toContain('test-code')
      state = 'connected'
      completed = true
    } else if (route.request().method() === 'DELETE') state = 'disconnected'
    else if (route.request().method() === 'POST') state = 'pending'
    await route.fulfill({ json: {
      id: 'connection-personal', state,
      ...(state === 'connected' ? { email: 'personal@example.test' } : {}),
      ...(state === 'pending' ? { authorization_url: 'https://auth.openai.com/api/accounts/authorize?client_id=dynamic_agent_client&state=test-state' } : {}),
    } })
  })
  await page.goto('/workspace/providers')
  const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await closeSetup.isVisible()) await closeSetup.click()
  const row = page.getByTestId('chatgpt-account-personal')
  await expect(row).toBeVisible()
  const closeWidget = page.getByTestId('onboarding-widget-close')
  if (await closeWidget.isVisible()) await closeWidget.click()
  await row.getByRole('button', { name: 'Personal subscription details' }).click()
  await row.getByRole('button', { name: 'Edit connection' }).click()
  const form = page.getByTestId('key-form')
  await expect(form.getByLabel('API Key', { exact: true })).toHaveCount(0)
  await expect(form.getByLabel('Name (optional)', { exact: true })).toBeVisible()
  await page.getByTestId('chatgpt-connect').click()
  await expect(page.getByTestId('chatgpt-authorization-link')).toHaveAttribute('href', /auth\.openai\.com\/api\/accounts\/authorize\?client_id=/)
  if (process.env.CHATGPT_SCREENSHOT_DIR) await page.getByTestId('chatgpt-onboarding').screenshot({ animations: 'disabled', path: `${process.env.CHATGPT_SCREENSHOT_DIR}/chatgpt-sign-in.png` })
  const callback = page.getByTestId('chatgpt-callback-url')
  await callback.fill('https://attacker.invalid/auth/callback?code=test-code')
  await expect(page.getByTestId('chatgpt-complete')).toBeDisabled()
  await callback.fill('http://127.0.0.1:1455/auth/callback?code=test-code&state=test-state')
  await page.getByTestId('chatgpt-complete').click()
  await expect(page.getByTestId('chatgpt-status')).toHaveText('connected')
  await expect.poll(() => completed).toBe(true)
  await expect(form).toContainText('personal@example.test')
  await page.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(form).toHaveCount(0)
  if (await closeWidget.isVisible()) await closeWidget.click()
  await expect(row.getByTestId('chatgpt-account-email')).toHaveText('personal@example.test')
  if (process.env.CHATGPT_SCREENSHOT_DIR) await row.screenshot({ animations: 'disabled', path: `${process.env.CHATGPT_SCREENSHOT_DIR}/chatgpt-account.png` })
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await row.evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
  await row.getByRole('button', { name: 'Edit connection' }).click()
  await page.getByTestId('chatgpt-disconnect').click()
  await expect(page.getByTestId('chatgpt-status')).toHaveText('disconnected')
  await expect(page.getByTestId('chatgpt-disconnect')).toHaveCount(0)
  await expect(page.getByTestId('chatgpt-connect')).toBeEnabled()
})
