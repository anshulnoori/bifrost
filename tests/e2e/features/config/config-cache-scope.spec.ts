import { expect, test } from '../../core/fixtures/base.fixture'

for (const scoped of [true, false]) {
  test(`cache edits preserve authenticated scope=${scoped}`, async ({ page }) => {
    let saved = 0
    const plugin = { name: 'semantic_cache', enabled: true, config: {
      dimension: 1, ttl: 300, threshold: 0.8, conversation_history_threshold: 3,
      cache_by_model: true, cache_by_provider: true, scope_by_virtual_key: scoped,
      default_cache_key: 'fixture',
    } }
    await page.route('**/api/**', route => route.fulfill({ json: {} }))
    await page.route('**/api/version', route => route.fulfill({ json: '1.4.9' }))
    await page.route('https://getbifrost.ai/latest-release', route => route.fulfill({ json: { version: '1.4.9' } }))
    await page.route('**/api/session/is-auth-enabled', route => route.fulfill({ json: { is_auth_enabled: false } }))
    await page.route('**/api/config**', route => route.fulfill({ json: { is_cache_connected: true, is_db_connected: true } }))
    await page.route('**/api/providers', route => route.fulfill({ json: { providers: [] } }))
    await page.route('**/api/plugins', route => route.fulfill({ json: { plugins: [plugin] } }))
    await page.route('**/api/plugins/semantic_cache', route => {
      expect(route.request().method()).toBe('PUT')
      const update = route.request().postDataJSON()
      expect(update.config.scope_by_virtual_key).toBe(scoped)
      expect(update.config.ttl).toBe(301)
      Object.assign(plugin, update)
      saved++
      return route.fulfill({ json: { plugin } })
    })
    const closeSetup = page.getByRole('button', { name: 'Close for now', exact: true })
    await page.addLocatorHandler(closeSetup, () => closeSetup.click())
    await page.goto('/workspace/config/caching')
    await expect(page.getByTestId('caching-ttl-input')).toHaveValue('300', { timeout: 20000 })
    await page.getByTestId('caching-ttl-input').fill('301')
    await page.getByTestId('caching-save-button').click()
    await expect.poll(() => saved).toBe(1)
    await expect(page.getByTestId('caching-save-button')).toBeDisabled()
  })
}
