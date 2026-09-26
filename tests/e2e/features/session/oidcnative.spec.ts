import { test, expect } from '../../core/fixtures/base.fixture'
import { resolve } from 'node:path'
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { execFileSync, spawn } from 'node:child_process'
import { createHash, generateKeyPairSync, randomBytes, sign } from 'node:crypto'
import { createServer as createHTTPServer, request as httpRequest } from 'node:http'
import { createServer as createHTTPSServer } from 'node:https'
import type { Server } from 'node:http'
import type { AddressInfo } from 'node:net'

const artifacts = resolve(__dirname, '../../../../.amp/in/artifacts')
test.use({ skipAutoLogin: true })

test('Exclusive native OIDC through real gateway and signed tokens, without password fallback', async ({ browser }) => {
  const binary = process.env.BIFROST_OIDC_TEST_BINARY
  test.skip(!binary, 'Set BIFROST_OIDC_TEST_BINARY to the built gateway for the full-path test')
  test.setTimeout(90000)
  const dir = mkdtempSync(resolve(tmpdir(), 'bifrost-oidc-browser-'))
  const listen = (server: Server) => new Promise<number>(accept => server.listen(0, '127.0.0.1', () => accept((server.address() as AddressInfo).port)))
  const certPath = resolve(dir, 'ca.pem')
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=OIDC test only', '-addext', 'subjectAltName=IP:127.0.0.1', '-keyout', resolve(dir, 'key.pem'), '-out', certPath], { stdio: 'ignore' })
  const tls = { key: readFileSync(resolve(dir, 'key.pem')), cert: readFileSync(certPath) }
  const keys = generateKeyPairSync('rsa', { modulusLength: 2048 })
  const jwk = { ...keys.publicKey.export({ format: 'jwk' }), kid: 'fixture-key', alg: 'RS256', use: 'sig' }
  const codes = new Map<string, { nonce: string; challenge: string; subject: string }>()
  let subject = '12345'
  let exchanges = 0
  let issuer = ''
  let base = ''
  const idp = createHTTPSServer(tls, async (req, res) => {
    const url = new URL(req.url!, issuer)
    res.setHeader('Content-Type', 'application/json')
    if (url.pathname === '/.well-known/openid-configuration') {
      res.end(JSON.stringify({ issuer, authorization_endpoint: issuer + '/authorize', token_endpoint: issuer + '/token', jwks_uri: issuer + '/jwks', id_token_signing_alg_values_supported: ['RS256'] }))
    } else if (url.pathname === '/jwks') {
      res.end(JSON.stringify({ keys: [jwk] }))
    } else if (url.pathname === '/authorize') {
      if (url.searchParams.get('client_id') !== 'browser-test' || url.searchParams.get('redirect_uri') !== base + '/api/session/oidc/callback' || url.searchParams.get('code_challenge_method') !== 'S256') {
        res.writeHead(400); res.end('{}'); return
      }
      if (!url.searchParams.has('approve')) {
        res.setHeader('Content-Type', 'text/html')
        const link = url.pathname + url.search + '&approve=1'
        res.end(`<h1>Local test identity provider</h1><a href="${link.replaceAll('&', '&amp;')}">Continue as test user</a>`)
        return
      }
      const code = randomBytes(24).toString('base64url')
      codes.set(code, { nonce: url.searchParams.get('nonce')!, challenge: url.searchParams.get('code_challenge')!, subject })
      const callback = new URL(base + '/api/session/oidc/callback')
      callback.searchParams.set('code', code)
      callback.searchParams.set('state', url.searchParams.get('state')!)
      res.writeHead(303, { location: callback.href }); res.end()
    } else if (url.pathname === '/token') {
      let body = ''
      for await (const chunk of req) body += chunk
      const form = new URLSearchParams(body)
      const pending = codes.get(form.get('code')!)
      codes.delete(form.get('code')!)
      if (!pending || req.headers.authorization !== 'Basic ' + Buffer.from('browser-test:fixture-secret').toString('base64') || form.get('redirect_uri') !== base + '/api/session/oidc/callback' || createHash('sha256').update(form.get('code_verifier') || '').digest('base64url') !== pending.challenge) {
        res.writeHead(400); res.end('{"error":"invalid_grant"}'); return
      }
      exchanges++
      const payload = { iss: issuer, aud: 'browser-test', sub: pending.subject, nonce: pending.nonce, iat: Math.floor(Date.now() / 1000), exp: Math.floor(Date.now() / 1000) + 300 }
      const unsigned = Buffer.from(JSON.stringify({ alg: 'RS256', kid: 'fixture-key' })).toString('base64url') + '.' + Buffer.from(JSON.stringify(payload)).toString('base64url')
      const idToken = unsigned + '.' + sign('RSA-SHA256', Buffer.from(unsigned), keys.privateKey).toString('base64url')
      res.end(JSON.stringify({ access_token: 'fixture-upstream-token', token_type: 'Bearer', id_token: idToken }))
    } else { res.writeHead(404); res.end('{}') }
  })
  issuer = 'https://127.0.0.1:' + await listen(idp)
  const reservation = createHTTPServer()
  const port = await listen(reservation)
  await new Promise<void>(done => reservation.close(() => done()))
  const frontend = createHTTPSServer(tls, (req, res) => {
    const upstream = httpRequest({ hostname: '127.0.0.1', port, path: req.url, method: req.method, headers: { ...req.headers, 'x-forwarded-proto': 'https' } }, response => {
      res.writeHead(response.statusCode!, response.headers)
      response.pipe(res)
    })
    upstream.on('error', () => { res.writeHead(502); res.end() })
    req.pipe(upstream)
  })
  base = 'https://127.0.0.1:' + await listen(frontend)
  writeFileSync(resolve(dir, 'pricing.json'), '{}', { mode: 0o600 })
  writeFileSync(resolve(dir, 'config.json'), JSON.stringify({
    encryption_key: 'disposable-browser-fixture-encryption-key',
    framework: { pricing: { pricing_url: 'file://' + resolve(dir, 'pricing.json'), model_parameters_url: 'file://' + resolve(dir, 'pricing.json'), mcp_library_sync_interval: 0 } },
    client: { enable_logging: false, enforce_auth_on_inference: true },
    plugins: [{ name: 'telemetry', enabled: false }],
    config_store: { enabled: true, type: 'sqlite', config: { path: resolve(dir, 'config.db') } },
    governance: { auth_config: { is_enabled: true, admin_username: 'fixture-admin', admin_password: 'fixture-recovery-password' } },
  }), { mode: 0o600 })
  const gateway = spawn(binary!, ['-app-dir', dir, '-host', '127.0.0.1', '-port', String(port)], {
    env: { PATH: process.env.PATH, HOME: dir, SSL_CERT_FILE: certPath, BIFROST_OIDC_ISSUER: issuer, BIFROST_OIDC_CLIENT_ID: 'browser-test', BIFROST_OIDC_CLIENT_SECRET: 'fixture-secret', BIFROST_OIDC_REDIRECT_URL: base + '/api/session/oidc/callback', BIFROST_OIDC_ALLOWED_SUBJECTS: '12345' },
    stdio: 'ignore',
  })
  // Certificate errors are ignored only in this disposable browser context. The Go
  // gateway still verifies IdP TLS against the explicitly installed fixture CA.
  const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 900 }, deviceScaleFactor: 2 })
  try {
    await expect.poll(async () => { try { return (await context.request.get(base + '/health')).status() } catch { return 0 } }, { timeout: 40000 }).toBe(200)
    expect((await context.request.get(base + '/api/providers')).status()).toBe(401)
    expect((await context.request.get(base + '/api/providers', { headers: { 'X-Forwarded-User': '12345', Authorization: 'Bearer fixture-upstream-token' } })).status()).toBe(401)
    let page = await context.newPage()
    let callback = ''
    page.on('request', req => { if (new URL(req.url()).pathname === '/api/session/oidc/callback') callback = req.url() })
    await page.goto(base + '/login')
    await expect(page.getByTestId('login-tailscale')).toBeVisible()
    await expect(page.getByTestId('login-recovery')).toHaveCount(0)
    await expect(page.getByLabel('Username', { exact: true })).toHaveCount(0)
    await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0)
    await page.getByTestId('login-tailscale').click()
    await expect(page.getByRole('heading', { name: 'Local test identity provider' })).toBeVisible()
    await page.getByRole('link', { name: 'Continue as test user' }).click()
    await expect(page).toHaveURL(/\/workspace(?:\/|$)/)
    expect((await context.request.get(base + '/api/providers')).status()).toBe(200)
    expect((await (await context.request.get(base + '/api/session/is-auth-enabled')).json()).has_valid_token).toBe(true)
    const session = (await context.cookies()).find(cookie => cookie.name === 'token')!
    expect(session.secure && session.httpOnly).toBe(true)
    expect(session.expires - Date.now() / 1000).toBeGreaterThan(12 * 3600 - 60)
    // Chromium adjusts Expires against the second-precision HTTP Date header.
    expect(session.expires - Date.now() / 1000).toBeLessThanOrEqual(12 * 3600 + 2)
    expect(exchanges).toBe(1)
    await page.goto(base + '/workspace/providers')
    await expect(page.getByRole('heading', { name: 'Add a provider to start routing requests' })).toBeVisible()
    await page.screenshot({ path: resolve(artifacts, 'oidc-real-gateway-authenticated.png') })
    const replay = await context.request.get(callback, { maxRedirects: 0 })
    expect(replay.headers().location).toBe('/login?oidc_error=invalid')
    expect(exchanges).toBe(1)
    expect((await context.request.post(base + '/api/session/logout', { headers: { Origin: base } })).status()).toBe(200)
    expect((await context.request.get(base + '/api/providers', { headers: { Cookie: 'token=' + session.value } })).status()).toBe(401)
    // Start a fresh tab after API logout; the old dashboard's background
    // requests can independently redirect that tab to login on their next 401.
    await page.close()
    page = await context.newPage()
    subject = 'not-allowed'
    await page.goto(base + '/login')
    await page.getByTestId('login-tailscale').click()
    await page.getByRole('link', { name: 'Continue as test user' }).click()
    await expect(page.getByRole('alert')).toContainText('not allowed to administer Bifrost')
    expect((await context.request.get(base + '/api/providers')).status()).toBe(401)
    await expect(page.getByTestId('login-recovery')).toHaveCount(0)
    await expect(page.getByLabel('Username', { exact: true })).toHaveCount(0)
    await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0)
    await page.screenshot({ path: resolve(artifacts, 'oidc-real-gateway-denied.png') })
    // Correct configured credentials must not provide an alternate path around
    // the denied subject. Exercise the real endpoint, not only hidden controls.
    const password = await context.request.post(base + '/api/session/login', {
      headers: { Origin: base },
      data: { username: 'fixture-admin', password: 'fixture-recovery-password' },
    })
    expect(password.status()).toBe(403)
    expect(await password.text()).toContain('Password authentication is disabled')
    expect(password.headers()['set-cookie']).toBeUndefined()
    expect((await context.request.get(base + '/api/providers')).status()).toBe(401)
    expect((await (await context.request.get(base + '/api/session/is-auth-enabled')).json()).has_valid_token).toBe(false)
  } finally {
    await context.close()
    gateway.kill('SIGTERM')
    if (gateway.exitCode === null) await new Promise<void>(done => gateway.once('exit', () => done()))
    frontend.closeAllConnections(); idp.closeAllConnections()
    await Promise.all([new Promise<void>(done => frontend.close(() => done())), new Promise<void>(done => idp.close(() => done()))])
    rmSync(dir, { recursive: true, force: true })
  }
})
