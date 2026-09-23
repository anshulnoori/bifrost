import { test, expect } from '../../core/fixtures/base.fixture'
import { resolve } from 'node:path'

const artifacts = resolve(__dirname, '../../../../.amp/in/artifacts')
test.use({ skipAutoLogin: true })

test('Tailscale login keeps password recovery and uses same-origin POST', async ({ page }) => {
  await page.route('**/api/session/is-auth-enabled', route => route.fulfill({ json: { is_auth_enabled: true, has_valid_token: false, auth_type: 'password', oidc_enabled: true } }))
  await page.goto('/login')
  await expect(page.getByTestId('login-tailscale')).toBeVisible()
  await expect(page.getByLabel('Username', { exact: true })).not.toBeVisible()
  await page.screenshot({ path: resolve(artifacts, 'tailscale-login.png') })
  await page.getByTestId('login-recovery').click()
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).toHaveAttribute('type', 'password')
  await page.screenshot({ path: resolve(artifacts, 'tailscale-recovery.png') })
  await page.getByTestId('login-recovery').click()
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
