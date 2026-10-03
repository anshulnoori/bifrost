import { test, expect } from '../../core/fixtures/base.fixture'

for (const reserve of [25, 0]) {
  test(`remove a saved ${reserve}% subscription reserve without deleting the account`, async ({ page }) => {
    const accounts = [
      { id: 'personal', name: 'Personal', models: ['*'], weight: 1, enabled: true, codex_reserve_percent: reserve as number | null },
      { id: 'work', name: 'Work', models: ['*'], weight: 1, enabled: true, codex_reserve_percent: 40 },
    ]
    const provider = { name: 'codex', keys: accounts, network_config: {}, concurrency_and_buffer_size: {}, provider_status: 'active' }
    let fail = true
    await page.route('**/api/providers', route => route.fulfill({ json: { providers: [provider], total: 1 } }))
    await page.route('**/api/providers/codex/keys', route => route.fulfill({ json: { keys: accounts, total: accounts.length } }))
    await page.route('**/api/providers/codex/keys/personal', route => {
      expect(route.request().method()).toBe('PUT')
      const payload = route.request().postDataJSON()
      expect(payload.codex_reserve_percent).toBeNull()
      expect(payload.enabled).toBe(true)
      expect(payload.models).toEqual(['*'])
      if (fail) return route.fulfill({ status: 500, json: { error: { message: 'Save failed' } } })
      Object.assign(accounts[0], payload)
      return route.fulfill({ json: accounts[0] })
    })
    await page.goto('/workspace/model-limits?tab=subscriptions')
    const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
    if (await closeSetup.isVisible()) await closeSetup.click()
    const closeWidget = page.getByTestId('onboarding-widget-close')
    if (await closeWidget.isVisible()) await closeWidget.click()
    const row = page.getByTestId('subscription-limit-personal')
    const remove = row.getByRole('button', { name: 'Remove limit', exact: true })
    await expect(remove).toBeVisible()
    await remove.click()
    await expect(row.getByRole('alert')).toContainText('Save failed')
    await expect(row.getByRole('spinbutton')).toHaveValue(String(reserve))
    expect(accounts[0].codex_reserve_percent).toBe(reserve)
    fail = false
    // Removal clears the saved limit even when the field contains an invalid unsaved edit.
    await row.getByRole('spinbutton').fill('101')
    await remove.click()
    await expect.poll(() => accounts[0].codex_reserve_percent).toBeNull()
    await expect(row.getByRole('spinbutton')).toHaveValue('')
    await expect(remove).toHaveCount(0)
    await expect(row.getByRole('status')).toHaveText('Saved')
    expect(accounts[1].codex_reserve_percent).toBe(40)
    await page.reload()
    await expect(row.getByRole('spinbutton')).toHaveValue('')
    await expect(remove).toHaveCount(0)
    await expect(page.getByTestId('subscription-limit-work').getByRole('spinbutton')).toHaveValue('40')
    if (process.env.SUBSCRIPTION_SCREENSHOT_DIR && reserve === 25) {
      await page.getByRole('table').screenshot({ animations: 'disabled', path: `${process.env.SUBSCRIPTION_SCREENSHOT_DIR}/subscription-limit-removed.png` })
    }
  })
}

test('Claude accounts appear beside Codex and save their reserve to the Claude provider', async ({ page }) => {
  const codex = [{ id: 'personal', name: 'Personal', models: ['*'], weight: 1, enabled: true, codex_reserve_percent: 40 as number | null }]
  const claude = [{ id: 'claude-pro', name: 'Claude Pro', models: ['*'], weight: 1, enabled: true, codex_reserve_percent: null as number | null }]
  const providers = [
    { name: 'codex', keys: codex, network_config: {}, concurrency_and_buffer_size: {}, provider_status: 'active' },
    { name: 'claude', keys: claude, network_config: { max_retries: 0 }, concurrency_and_buffer_size: {}, provider_status: 'active' },
  ]
  await page.route('**/api/providers', route => route.fulfill({ json: { providers, total: providers.length } }))
  await page.route('**/api/providers/codex/keys', route => route.fulfill({ json: { keys: codex, total: codex.length } }))
  await page.route('**/api/providers/claude/keys', route => route.fulfill({ json: { keys: claude, total: claude.length } }))
  await page.route('**/api/providers/codex/keys/**', route => route.fulfill({ status: 500, json: { error: { message: 'wrong provider' } } }))
  await page.route('**/api/providers/claude/keys/claude-pro', route => {
    expect(route.request().method()).toBe('PUT')
    const payload = route.request().postDataJSON()
    expect(payload.codex_reserve_percent).toBe(30)
    Object.assign(claude[0], payload)
    return route.fulfill({ json: claude[0] })
  })
  await page.goto('/workspace/model-limits?tab=subscriptions')
  const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
  if (await closeSetup.isVisible()) await closeSetup.click()
  const claudeRow = page.getByTestId('subscription-limit-claude-pro')
  await expect(page.getByTestId('subscription-limit-personal')).toContainText('Codex')
  await expect(claudeRow).toContainText('Claude')
  await claudeRow.getByRole('spinbutton').fill('30')
  await claudeRow.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(claudeRow.getByRole('status')).toHaveText('Saved')
  await expect.poll(() => claude[0].codex_reserve_percent).toBe(30)
  expect(codex[0].codex_reserve_percent).toBe(40)
})
