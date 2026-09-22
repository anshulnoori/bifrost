import { test, expect } from '../../core/fixtures/base.fixture'
import report from '../../../../integrations/headroom/testdata/dashboard.json'

test('overview automatically loads estimates, filters chart, and separates configuration', async ({ page }) => {
  await page.route('**/api/headroom/events', route => route.fulfill({ json: report }))
  await page.goto('/workspace/headroom')
  await expect(page.getByTestId('headroom-overview-tab')).toHaveAttribute('data-state', 'active')
  await expect(page.getByTestId('headroom-token-savings')).toHaveText('5,127 fewer tokens (97.2%)')
  await expect(page.getByTestId('headroom-token-chart')).toBeVisible()
  await expect(page.getByTestId('headroom-token')).not.toBeVisible()
  await page.getByTestId('headroom-filter').fill('codex')
  await expect(page.getByTestId('headroom-token-savings')).toHaveText('No token estimates yet')
  await expect(page.getByTestId('headroom-token-chart')).not.toBeVisible()
  await page.getByTestId('headroom-configuration-tab').click()
  await expect(page.getByTestId('headroom-token-savings')).not.toBeVisible()
  await page.getByTestId('headroom-overview-tab').click()
  await page.route('**/api/headroom/events', route => route.fulfill({ status: 401, body: 'unauthorized' }))
  await page.getByTestId('headroom-refresh').click()
  await expect(page.getByTestId('headroom-error')).toBeVisible()
  await expect(page.getByTestId('headroom-token')).toBeVisible()
  await expect(page.getByTestId('headroom-token-chart')).not.toBeVisible()
})
