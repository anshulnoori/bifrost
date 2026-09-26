import { test, expect } from '../../core/fixtures/base.fixture'
import report from '../../../../integrations/headroom/testdata/dashboard.json'

test('embedding events do not prevent compression metrics from loading', async ({ page }) => {
  const mixed = { ...report, events: [...report.events, {
    ...report.events[1], started: '2026-09-22T12:02:00Z', status: 'embedded', reason: 'embedding',
    provider: '', model: '', thread_hash: '', principal_hash: '', project: '',
  }] }
  await page.route('**/api/headroom/events', route => route.fulfill({ json: mixed }))
  await page.goto('/workspace/headroom')
  await expect(page.getByTestId('headroom-error')).not.toBeVisible()
  await expect(page.getByTestId('headroom-token-savings')).toHaveText('5,127 fewer tokens (97.2%)')
  await expect(page.getByRole('cell', { name: 'embedded embedding', exact: true })).toBeVisible()
  await page.getByTestId('headroom-refresh').click()
  await expect(page.getByRole('cell', { name: 'embedded embedding', exact: true })).toBeVisible()
  const closeSetup = page.getByTestId('onboarding-widget-close')
  if (await closeSetup.isVisible()) await closeSetup.click()
  await page.getByRole('table').screenshot({ path: '../../.amp/in/artifacts/headroom-mixed-events.png' })
})

test('overview automatically loads estimates, filters chart, and separates configuration', async ({ page }) => {
  const chartReport = { ...report, events: [...report.events, { ...report.events[0], started: '2026-09-22T12:01:00Z' }] }
  await page.route('**/api/headroom/events', route => route.fulfill({ json: chartReport }))
  await page.goto('/workspace/headroom')
  await expect(page.getByTestId('headroom-overview-tab')).toHaveAttribute('data-state', 'active')
  await expect(page.getByTestId('headroom-token-savings')).toHaveText('10,254 fewer tokens (97.2%)')
  await expect(page.getByTestId('headroom-token-chart')).toBeVisible()
  const series = page.locator('[data-testid="headroom-token-chart"] .recharts-line')
  await expect(series).toHaveCount(2)
  for (const theme of ['light', 'dark']) {
    await page.evaluate(theme => { document.documentElement.classList.remove('light', 'dark'); document.documentElement.classList.add(theme) }, theme)
    for (const line of await series.all()) {
      const mark = line.locator('path.recharts-line-curve').first()
      // Horizontal SVG paths have zero-height boxes even when visibly stroked.
      await expect(mark).toBeAttached()
      await expect.poll(() => mark.evaluate(node => getComputedStyle(node).stroke)).not.toBe('none')
    }
  }
  await page.route('**/api/headroom/events', route => route.fulfill({ json: report }))
  await page.getByTestId('headroom-refresh').click()
  await expect(page.locator('[data-testid="headroom-token-chart"] circle.recharts-line-dot')).toHaveCount(2)
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
