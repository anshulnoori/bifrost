import { test, expect } from '../../core/fixtures/base.fixture'

test.beforeEach(async ({ page }) => {
  // Complete fixture login before mounting the workspace; otherwise its shell
  // can appear briefly before unauthenticated requests redirect to /login.
  await page.goto('/login')
  await expect(page.getByTestId('sidebar-collapse-btn')).toBeVisible({ timeout: 15000 })
})

test('dismissed sidebar release stays hidden across collapse, navigation and reload', async ({ page }) => {
  let release = 'v99.0.0'
  await page.route('https://getbifrost.ai/latest-release', route => route.fulfill({ json: { name: release } }))
  await page.goto('/workspace/providers')
  const closeSetup = page.getByTestId('onboarding-widget-close')
  if (await closeSetup.isVisible()) await closeSetup.click()
  const title = page.getByText('v99.0.0 is now available.', { exact: true })
  await expect(title).toBeVisible({ timeout: 15000 })
  await title.locator('..').getByRole('button', { name: 'Dismiss', exact: true }).click()
  await expect(title).toBeHidden()
  await expect.poll(async () => (await page.context().cookies()).find(cookie => cookie.name === 'bifrost_release_dismissed')?.value).toBe('v99.0.0')
  await page.getByTestId('sidebar-collapse-btn').click()
  await page.getByRole('button', { name: 'Expand sidebar', exact: true }).click()
  await expect(title).toBeHidden()
  await page.goto('/workspace/logs')
  await expect(title).toBeHidden()
  await page.reload()
  await expect(title).toBeHidden()
  release = 'v99.0.1'
  await page.reload()
  await expect(page.getByText('v99.0.1 is now available.', { exact: true })).toBeVisible()
})

test('setup widget close survives navigation and reload', async ({ page, baseURL }) => {
  await page.context().addCookies([{ name: 'bifrost_onboarding_hidden_until_nav', value: 'true', url: baseURL! }])
  await page.goto('/workspace/providers')
  await expect(page.getByTestId('sidebar-collapse-btn')).toBeVisible({ timeout: 15000 })
  await page.goto('/workspace/logs')
  await expect(page.getByTestId('sidebar-collapse-btn')).toBeVisible()
  await page.reload()
  await expect(page.getByTestId('sidebar-collapse-btn')).toBeVisible()
  expect((await page.context().cookies()).find(cookie => cookie.name === 'bifrost_onboarding_hidden_until_nav')?.value).toBe('true')
  await expect(page.getByTestId('onboarding-widget-close')).toBeHidden()
})

test('OSS sidebar hides enterprise destinations but retains working OSS features', async ({ page }) => {
  await page.goto('/workspace/providers')
  await expect(page.getByTestId('sidebar-collapse-btn')).toBeVisible({ timeout: 15000 })
  const sidebar = page.locator('[data-slot="sidebar-content"]')
  for (const title of ['Alerting', 'Guardrails', 'Edge Control', 'Cluster Config', 'Adaptive Routing']) {
    await expect(sidebar.getByText(title, { exact: true })).toHaveCount(0)
  }
  for (const title of ['Governance', 'Settings']) await sidebar.getByText(title, { exact: true }).click()
  for (const title of ['Circuit Breaker', 'Users', 'Business Units', 'User Provisioning', 'Roles & Permissions', 'Access Profiles', 'Projects', 'Audit Logs', 'API Keys']) {
    await expect(sidebar.getByText(title, { exact: true })).toHaveCount(0)
  }
  for (const title of ['Model Providers', 'Budgets & Limits', 'Complexity Router', 'Teams', 'Customers', 'Virtual Keys', 'Security', 'Caching']) {
    await expect(sidebar.getByText(title, { exact: true })).toBeVisible()
  }
  await sidebar.screenshot({ path: '../../.amp/in/artifacts/sidebar-oss.png' })
})
