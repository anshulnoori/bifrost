import { test, expect } from '../../core/fixtures/base.fixture'
import { resolve } from 'node:path'

const artifacts = resolve(__dirname, '../../../../.amp/in/artifacts')
test.use({ skipAutoLogin: true })

test('Tailscale login has its brand icon, no password fallback, and uses same-origin POST', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' })
  await page.route('**/api/session/is-auth-enabled', route => route.fulfill({ json: { is_auth_enabled: true, has_valid_token: false, auth_type: 'password', oidc_enabled: true } }))
  await page.goto('/login')
  await expect(page.getByTestId('login-tailscale')).toBeVisible()
  const icon = page.getByTestId('login-tailscale').locator('img')
  await expect(icon).toHaveAttribute('src', '/tailscale-icon-light.svg')
  await expect.poll(() => icon.evaluate(img => (img as HTMLImageElement).naturalWidth)).toBeGreaterThan(0)
  await expect(icon).toHaveCSS('width', '20px')
  await expect(page.getByTestId('login-tailscale')).toHaveCSS('height', '40px')
  await expect(page.getByTestId('login-recovery')).toHaveCount(0)
  await expect(page.getByLabel('Username', { exact: true })).not.toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).not.toBeVisible()
  await page.screenshot({ path: resolve(artifacts, 'tailscale-login.png') })
  await page.route('**/api/session/oidc/login', route => {
    expect(route.request().method()).toBe('POST')
    expect(route.request().headers().origin).toBe(new URL(route.request().url()).origin)
    return route.fulfill({ status: 303, headers: { location: '/login?oidc_error=cancelled' } })
  })
  await page.getByTestId('login-tailscale').click()
  await expect(page.getByRole('alert')).toHaveText('Sign-in was cancelled. You can try again.')
  await page.screenshot({ path: resolve(artifacts, 'tailscale-cancelled.png') })
})

test('Unconfigured OIDC retains normal password login', async ({ page }) => {
  await page.route('**/api/session/is-auth-enabled', route => route.fulfill({ json: { is_auth_enabled: true, has_valid_token: false, auth_type: 'password', oidc_enabled: false } }))
  await page.goto('/login')
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).toBeVisible()
  await expect(page.getByTestId('login-tailscale')).not.toBeVisible()
  await page.screenshot({ path: resolve(artifacts, 'tailscale-disabled.png') })
})

test('Unavailable OIDC uses the official dark icon in dark mode and no password recovery', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.route('**/api/session/is-auth-enabled', route => route.fulfill({ json: { is_auth_enabled: true, has_valid_token: false, auth_type: 'password', oidc_enabled: true } }))
  await page.goto('/login?oidc_error=unavailable')
  const icon = page.getByTestId('login-tailscale').locator('img')
  await expect(icon).toHaveAttribute('src', '/tailscale-icon-dark.svg')
  await expect(icon).toHaveJSProperty('complete', true)
  await expect.poll(() => icon.evaluate(img => (img as HTMLImageElement).naturalWidth)).toBeGreaterThan(0)
  await expect(page.getByRole('alert')).toHaveText('Tailscale sign-in is unavailable. Please try again later.')
  await expect(page.getByTestId('login-recovery')).toHaveCount(0)
  await expect(page.getByLabel('Password', { exact: true })).not.toBeVisible()
  await page.screenshot({ path: resolve(artifacts, 'tailscale-unavailable.png') })
})

test('Pending auth status does not flash password login', async ({ page }) => {
  let release!: () => void
  let requests = 0
  const pending = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/session/is-auth-enabled', async route => {
    // Let the route loader complete, then hold the login component's query.
    if (++requests > 1) await pending
    await route.fulfill({ json: { is_auth_enabled: true, has_valid_token: false, auth_type: 'password', oidc_enabled: true } })
  })
  await page.goto('/login')
  await expect(page.getByRole('heading', { name: 'Welcome back' })).toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0)
  release()
  await expect(page.getByTestId('login-tailscale')).toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0)
})
