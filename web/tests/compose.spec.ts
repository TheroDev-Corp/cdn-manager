import { execFileSync } from 'node:child_process'
import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { createReadStream } from 'node:fs'
import { appendFile, mkdir, stat, truncate, writeFile } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

const baseURL = process.env.CDN_TEST_BASE_URL
const issuer = 'http://127.0.0.1:5556'
const bytesInMiB = 1024 * 1024
const videoBytes = 72 * bytesInMiB

test.skip(!baseURL, 'Start Docker Compose and set CDN_TEST_BASE_URL to run the integrated acceptance.')

function docker(...args: string[]) {
  const sudo = process.env.CDN_TEST_DOCKER_USE_SUDO === '1'
  const project = process.env.COMPOSE_PROJECT_NAME
  const dockerArgs = args[0] === 'compose' && project ? ['compose', '--project-name', project, ...args.slice(1)] : args
  const options = {cwd: resolve(process.env.CDN_TEST_COMPOSE_DIR ?? '..'), encoding: 'utf8' as const}
  return execFileSync(sudo ? 'sudo' : 'docker', sudo ? ['-n', 'docker', ...dockerArgs] : dockerArgs, options).trim()
}

async function login(page: Page, account: 'Owner' | 'Editor' | 'Viewer') {
  await page.goto('/')
  await page.getByRole('link', {name: 'Continue with single sign-on'}).click()
  await page.getByRole('button', {name: 'Demo ' + account}).click()
  await expect(page.getByLabel('Storage source')).toBeEnabled()
}

async function ready() {
  await expect.poll(async () => {
    try { return (await fetch(baseURL!)).status }
    catch { return 0 }
  }, {timeout: 25_000}).toBe(200)
}

async function mp4(directory: string, name: string) {
  await mkdir(directory, {recursive: true})
  const path = join(directory, name)
  execFileSync('ffmpeg', ['-v', 'error', '-f', 'lavfi', '-i', 'testsrc2=size=128x72:rate=24',
    '-t', '2', '-c:v', 'libx264', '-movflags', '+faststart', path])
  const padding = Buffer.alloc(8)
  padding.writeUInt32BE(videoBytes - (await stat(path)).size)
  padding.write('free', 4)
  await appendFile(path, padding)
  await truncate(path, videoBytes)
  return path
}

async function digest(chunks: AsyncIterable<Uint8Array>) {
  const hash = createHash('sha256')
  for await (const chunk of chunks) hash.update(chunk)
  return hash.digest('hex')
}

async function copyLink(page: Page, key: string) {
  await page.getByRole('button', {name: 'CDN link for ' + key, exact: true}).click()
  const textbox = page.getByRole('textbox', {name: 'Permanent CDN link'})
  await expect(textbox).not.toHaveValue('')
  const url = await textbox.inputValue()
  await page.getByRole('button', {name: 'Copy link'}).click()
  await expect(page.getByRole('button', {name: 'Copied'})).toBeVisible()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(url)
  await page.getByRole('button', {name: 'Close'}).click()
  return url
}

async function shots(page: Page, state: string) {
  const output = process.env.CDN_TEST_SCREENSHOTS
  if (!output) return
  await mkdir(output, {recursive: true})
  for (const width of [1440, 768, 390]) {
    await page.setViewportSize({width, height: width === 1440 ? 900 : 850})
    await page.evaluate(() => document.fonts.ready)
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
    await page.screenshot({path: join(output, state + '-' + width + '.png'), fullPage: true})
  }
  await page.setViewportSize({width: 1440, height: 900})
}

async function token(page: Page, scope: string, client = 'demo-mcp') {
  const verifier = randomBytes(32).toString('base64url')
  const state = randomUUID()
  const redirect = issuer + '/mcp-callback'
  const params = new URLSearchParams({
    response_type: 'code', redirect_uri: redirect, client_id: client, scope, state,
    code_challenge: createHash('sha256').update(verifier).digest('base64url'), code_challenge_method: 'S256',
  })
  await page.goto(issuer + '/authorize?' + params.toString())
  await page.getByRole('button', {name: 'Demo Owner'}).click()
  await expect(page).toHaveURL(url => url.pathname === '/mcp-callback')
  const callback = new URL(page.url()).searchParams
  expect(callback.get('state')).toBe(state)
  const response = await page.request.post(issuer + '/token', {form: {
    grant_type: 'authorization_code', client_id: client, redirect_uri: redirect, code: callback.get('code')!, code_verifier: verifier,
  }})
  expect(response.status()).toBe(200)
  return (await response.json()).access_token as string
}

function mcpHeaders(accessToken?: string) {
  const headers = new Headers({Accept: 'application/json, text/event-stream', 'Content-Type': 'application/json'})
  if (accessToken) headers.set('Authorization', 'Bearer ' + accessToken)
  return headers
}

function rpc(headers: Headers, method: string, params: unknown, id?: number) {
  return fetch(baseURL + '/mcp', {
    method: 'POST', headers, body: JSON.stringify({jsonrpc: '2.0', id, method, params}),
    signal: AbortSignal.timeout(15_000),
  })
}

async function message(response: Response) {
  expect(response.status).toBe(200)
  if (!response.headers.get('content-type')?.includes('text/event-stream')) return response.json()
  const reader = response.body!.getReader()
  const decoder = new TextDecoder()
  let pending = ''
  try {
    while (true) {
      const next = await reader.read()
      if (next.done) break
      pending += decoder.decode(next.value, {stream: true})
      const lines = pending.split('\n')
      pending = lines.pop()!
      for (const line of lines) {
        if (line.startsWith('data: ') && line.slice(6).trim()) return JSON.parse(line.slice(6))
      }
    }
    throw new Error('MCP stream ended before the response message.')
  } finally {
    await reader.cancel()
  }
}

const initialize = {protocolVersion: '2025-11-25', capabilities: {}, clientInfo: {name: 'compose-acceptance', version: '1.0'}}

function toolOutput(result: {structuredContent?: unknown; content: {text: string}[]; isError?: boolean}) {
  expect(result.isError ?? false).toBe(false)
  return result.structuredContent ?? JSON.parse(result.content[0].text)
}

async function mcpClient(accessToken: string) {
  const headers = mcpHeaders(accessToken)
  const response = await rpc(headers, 'initialize', initialize, 1)
  const session = response.headers.get('Mcp-Session-Id')
  if (session) headers.set('Mcp-Session-Id', session)
  headers.set('MCP-Protocol-Version', (await message(response)).result.protocolVersion)
  expect((await rpc(headers, 'notifications/initialized', {})).status).toBe(202)
  return async (name: string, args: unknown) => {
    const {result} = await message(await rpc(headers, 'tools/call', {name, arguments: args}, 2))
    return result
  }
}

test('shows MCP connection values from the manager and protects the settings with SSO', async ({page, request}) => {
  expect((await request.get('/api/v1/mcp-settings')).status()).toBe(401)
  const deniedMcp = await request.get('/mcp')
  expect(deniedMcp.status()).toBe(401)
  const discoveryPath = '/.well-known/oauth-protected-resource/mcp'
  expect(deniedMcp.headers()['www-authenticate']).toContain(baseURL + discoveryPath)
  const discovery = await (await request.get(discoveryPath)).json()

  await login(page, 'Owner')
  const settingsRequest = page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/mcp-settings')
  await page.getByRole('button', {name: 'MCP Settings', exact: true}).click()
  const response = await settingsRequest
  expect(response.status()).toBe(200)
  expect(response.headers()['cache-control']).toBe('no-store')
  expect(await response.json()).toEqual({
    url: discovery.resource, clientId: 'demo-mcp', issuer: discovery.authorization_servers[0],
    metadataUrl: baseURL + discoveryPath, scopes: discovery.scopes_supported,
  })
  const dialog = page.getByRole('dialog', {name: 'MCP Settings'})
  await expect(dialog.getByLabel('MCP endpoint')).toHaveValue(discovery.resource)
  await expect(dialog.getByLabel('Inspector command')).toHaveValue(/--client-id 'demo-mcp'.*list_sources/)
})

test('resumes a video across a manager restart and serves identical browser and MCP CDN links', async ({page, context, browser}, testInfo) => {
  test.setTimeout(160_000)
  await ready()
  const managerID = docker('compose', 'ps', '-q', 'manager')
  const memoryLimit = Number(docker('inspect', '--format', '{{.HostConfig.Memory}}', managerID))
  expect(memoryLimit).toBeGreaterThan(0)
  expect(videoBytes).toBeGreaterThan(memoryLimit)
  const label = randomUUID().slice(0, 8)
  const filename = 'Клип ' + label + ' #50%.mp4'
  const folder = testInfo.outputPath('media')
  const path = await mp4(folder, filename)
  const hash = await digest(createReadStream(path))
  await login(page, 'Owner')
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])

  const textFilename = 'Правила ' + label + ' #50%.css'
  const text = 'body { color: teal; }\n'
  const directPut = page.waitForResponse(response =>
    response.request().method() === 'PUT' && new URL(response.url()).pathname === '/api/v1/object')
  await page.locator('input[type=file]:not([webkitdirectory])').setInputFiles({
    name: textFilename, mimeType: 'text/css', buffer: Buffer.from(text),
  })
  expect((await directPut).ok()).toBe(true)
  await page.getByRole('button', {name: 'Objects', exact: true}).click()
  const textURL = await copyLink(page, textFilename)
  const publicText = await fetch(textURL)
  expect(publicText.status).toBe(200)
  expect(await publicText.text()).toBe(text)
  await shots(page, 'objects')
  await page.getByRole('button', {name: textFilename, exact: true}).click()
  await expect(page.getByRole('link', {name: 'Download'})).toBeVisible()
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height: 900})
    await page.getByRole('button', {name: 'Check availability'}).click()
    await expect(page.getByRole('dialog').getByRole('status')).toHaveText('Checked from the manager · Available · HTTP 200')
  }
  await shots(page, 'detail')
  await page.getByRole('button', {name: 'Close'}).click()

  await page.getByLabel('Storage source').selectOption('media')
  const attempts: number[] = []
  let interrupted = false
  await page.route('**/api/v1/uploads/*/parts/*', async route => {
    const part = Number(new URL(route.request().url()).pathname.split('/').at(-1))
    attempts.push(part)
    if (part === 2 && !interrupted) {
      interrupted = true
      await route.abort('failed')
    } else await route.continue()
  })
  const started = page.waitForResponse(response =>
    response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/v1/uploads')
  await page.locator('input[webkitdirectory]').setInputFiles(folder)
  const {id} = await (await started).json()
  const uploadPath = '/api/v1/uploads/' + id
  await expect(page.getByRole('button', {name: 'Resume', exact: true})).toBeVisible({timeout: 30_000})
  expect(attempts).toEqual([1, 2])
  await shots(page, 'transfer')
  const editor = await browser.newPage({baseURL})
  try {
    await login(editor, 'Editor')
    expect((await editor.request.get(uploadPath)).status()).toBe(404)
  } finally { await editor.close() }

  docker('compose', 'restart', 'manager')
  await ready()
  await login(page, 'Owner')
  const session = await (await page.request.get('/api/v1/session')).json()
  const durable = await (await page.request.get(uploadPath)).json()
  expect(durable.parts.map((part: {number: number}) => part.number)).toEqual([1])
  await page.getByRole('button', {name: /^Transfers/}).click()
  const completed = page.waitForResponse(response => new URL(response.url()).pathname === uploadPath + '/complete')
  await page.locator('.pick-file input').setInputFiles(path)
  await expect(page.getByText('complete · 72 MiB / 72 MiB')).toBeVisible({timeout: 60_000})
  expect(attempts.filter(number => number === 1)).toHaveLength(1)
  expect(attempts.filter(number => number === 2)).toHaveLength(2)
  expect((await completed).status()).toBe(200)
  const result = (await (await page.request.get(uploadPath)).json()).result
  expect(result).toMatchObject({size: videoBytes, key: 'media/' + filename})
  const retry = await page.request.post(uploadPath + '/complete', {headers: {'X-CSRF-Token': session.csrfToken}})
  expect(retry.status()).toBe(200)
  expect(await retry.json()).toEqual(result)

  await page.getByLabel('Storage source').selectOption('media')
  await page.getByRole('button', {name: 'media/', exact: true}).click()
  const key = 'media/' + filename
  const videoURL = await copyLink(page, key)
  expect(new URL(videoURL).pathname).toBe('/media/media/' + encodeURIComponent(filename))
  const rpcURL = new URL(baseURL + '/mcp')
  const rejected = await rpc(mcpHeaders(), 'initialize', initialize, 1)
  expect(rejected.status).toBe(401)
  expect(rejected.headers.get('www-authenticate')).toContain('resource_metadata')
  const discovery = await fetch(baseURL + '/.well-known/oauth-protected-resource/mcp')
  expect(await discovery.json()).toMatchObject({resource: rpcURL.toString(), authorization_servers: [issuer]})

  const mcpPage = await browser.newPage({baseURL})
  try {
    const scoped = await mcpClient(await token(mcpPage, 'read'))
    const listed = await scoped('list_sources', {})
    const listedSources = toolOutput(listed)
    const webSources = await (await page.request.get('/api/v1/sources')).json()
    expect(listedSources.sources.map((source: {id: string}) => source.id)).toEqual(
      webSources.sources.map((source: {id: string}) => source.id),
    )
    const link = await scoped('get_cdn_url', {source: 'media', bucket: 'videos', key})
    expect(toolOutput(link)).toMatchObject({url: videoURL})
    const payload = 'demo transfer over a part grant'
    const mcpKey = 'media/' + label + '-grant.txt'
    const uploadArgs = {
      source: 'media', bucket: 'videos', key: mcpKey, size: payload.length, contentType: 'text/plain',
    }
    expect((await scoped('start_upload', uploadArgs)).isError).toBe(true)
    const write = await mcpClient(await token(mcpPage, 'read write'))
    const upload = toolOutput(await write('start_upload', uploadArgs))
    const grant = toolOutput(await write('grant_upload_part', {id: upload.id, number: 1}))
    expect(grant.contentLength).toBe(payload.length)
    expect(new URL(grant.url).origin).toBe(baseURL)
    const uploaded = await fetch(grant.url, {
      method: grant.method, headers: {Authorization: grant.authorization}, body: payload,
    })
    expect(uploaded.status).toBe(200)
    expect(toolOutput(await write('complete_upload', {id: upload.id}))).toMatchObject({key: mcpKey})
    const wrongAudience = await token(mcpPage, 'read', 'demo-other-mcp')
    expect((await rpc(mcpHeaders(wrongAudience), 'initialize', initialize, 1)).status).toBe(401)
  } finally { await mcpPage.close() }

  const metrics = docker('compose', 'exec', '-T', 'issuer', 'bun', '-e', [
    'const response = await fetch("http://127.0.0.1:9090/metrics")',
    'if (!response.ok) process.exit(1)',
    'console.log(await response.text())',
  ].join('\n'))
  const uploadedBytes = metrics.match(/^cdn_manager_transfer_bytes_total\{direction="upload",source="media"\} (\S+)$/m)
  expect(Number(uploadedBytes?.[1])).toBeGreaterThan(0)
  expect(metrics).toMatch(/^cdn_manager_source_available\{source="media"\} 1$/m)
  expect(metrics).toMatch(/^cdn_manager_http_requests_total\{[^\n]*route="\/mcp"/m)
  expect(metrics).toMatch(/^cdn_manager_http_request_duration_seconds_bucket\{/m)
  expect(metrics).toMatch(/^cdn_manager_s3_retries_total\{/m)
  await testInfo.attach('prometheus-metrics', {body: metrics, contentType: 'text/plain'})
  if (process.env.CDN_TEST_SCREENSHOTS) {
    await writeFile(join(process.env.CDN_TEST_SCREENSHOTS, 'prometheus.txt'), metrics + '\n')
  }

  let originBytes = 0
  docker('compose', 'stop', 'manager')
  try {
    await expect(fetch(baseURL!)).rejects.toThrow()
    const ingress = () => Number(docker('compose', 'exec', '-T', 'media-gateway', 'cat', '/sys/class/net/eth0/statistics/rx_bytes'))
    const before = ingress()
    const firstSlice = await fetch(videoURL, {headers: {Range: 'bytes=0-4095'}})
    expect(firstSlice.status).toBe(206)
    expect(firstSlice.headers.get('content-range')).toBe('bytes 0-4095/' + videoBytes)
    const localPrefix = createReadStream(path, {start: 0, end: 4095})
    expect(await digest(firstSlice.body!)).toBe(await digest(localPrefix))
    originBytes = ingress() - before
    expect(originBytes).toBeGreaterThan(4095)
    expect(originBytes).toBeLessThan(2 * bytesInMiB)
    const unauthenticatedOrigin = docker('compose', 'exec', '-T', 'garage', 'sh', '-c',
      'wget -S -O /dev/null "$1" 2>&1 || true', 'wget', 'http://localhost:3900/videos/' + key.split('/').map(encodeURIComponent).join('/'))
    expect(unauthenticatedOrigin).toContain('403')
    const video = await fetch(videoURL)
    expect(video.status).toBe(200)
    expect(await digest(video.body!)).toBe(hash)
  } finally { docker('compose', 'start', 'manager') }

  await ready()
  const viewer = await browser.newPage({baseURL})
  try {
    await login(viewer, 'Viewer')
    await expect(viewer.getByLabel('Bucket')).toBeEnabled()
    await expect(viewer.getByRole('button', {name: 'Upload files'})).toHaveCount(0)
    expect((await viewer.request.get('/api/v1/buckets?source=media')).status()).toBe(404)
    const {csrfToken} = await (await viewer.request.get('/api/v1/session')).json()
    const mutate = await viewer.request.put('/api/v1/object?source=assets&bucket=assets&key=viewer-probe.txt', {
      data: 'denied', headers: {'X-CSRF-Token': csrfToken, 'Content-Type': 'text/plain'},
    })
    expect(mutate.status()).toBe(403)
  } finally { await viewer.close() }

  const evidence = JSON.stringify({
    videoBytes, memoryLimit, sha256: hash, attempts, contentRange: 'bytes 0-4095/' + videoBytes, gatewayIngressBytes: originBytes,
  }, null, 2)
  await testInfo.attach('transfer-and-range', {body: evidence, contentType: 'application/json'})
  if (process.env.CDN_TEST_SCREENSHOTS) await writeFile(join(process.env.CDN_TEST_SCREENSHOTS, 'acceptance.json'), evidence + '\n')
})
