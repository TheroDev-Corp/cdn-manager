import { expect, test } from '@playwright/test'

const settings = {
  url: 'https://configured-manager.example.test/mcp',
  clientId: 'test-public-client',
  issuer: 'https://identity.example.test/application/o/mcp/',
  metadataUrl: 'https://configured-manager.example.test/.well-known/oauth-protected-resource/mcp',
  scopes: ['read', 'write'],
}

test.beforeEach(async ({page}) => {
  await page.route('**/api/v1/session', route => route.fulfill({json: {subject: 'viewer', csrfToken: 'test-csrf'}}))
  await page.route('**/api/v1/sources', route => route.fulfill({json: {sources: []}}))
})

test('copies the configured MCP endpoint and browser SSO command at a narrow viewport', async ({page, context}) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await page.setViewportSize({width: 390, height: 700})
  await page.route('**/api/v1/mcp-settings', route => route.fulfill({json: settings}))
  await page.goto('/')

  const trigger = page.getByRole('button', {name: 'MCP Settings', exact: true})
  await trigger.focus()
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('dialog', {name: 'MCP Settings'})
  await expect(dialog.getByRole('button', {name: 'Close'})).toBeFocused()
  await expect(dialog.getByLabel('MCP endpoint')).toHaveValue(settings.url)
  await dialog.getByRole('button', {name: 'Copy endpoint'}).click()
  await expect(dialog.getByText('Copied to clipboard.')).toBeVisible()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(settings.url)

  const command = `npx @modelcontextprotocol/inspector@2.16.0 --cli '${settings.url}' --transport http --protocol-era modern --client-id '${settings.clientId}' --method tools/call --tool-name list_sources`
  await expect(dialog.getByLabel('Inspector command')).toHaveValue(command)
  await dialog.getByRole('button', {name: 'Copy command'}).click()
  await expect(dialog.getByText('Copied to clipboard.')).toHaveCount(2)
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(command)
  await expect(dialog.getByText(/Follow the browser sign-in prompt/)).toBeVisible()
  await dialog.getByText('OAuth discovery details').click()
  await expect(dialog.getByText(settings.issuer, {exact: true})).toBeVisible()
  await expect(dialog.getByText(settings.metadataUrl, {exact: true})).toBeVisible()
  expect(await dialog.evaluate(element => {
    const bounds = element.getBoundingClientRect()
    return bounds.left >= 0 && bounds.right <= window.innerWidth && element.scrollWidth <= element.clientWidth
  })).toBe(true)
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()
})

test('reports loading, supports retry, and leaves the endpoint selectable if copying fails', async ({page}) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {value: {writeText: async () => { throw new Error('Blocked') }}})
  })
  let completeRequest!: () => void
  const firstRequest = new Promise<void>(resolve => {completeRequest = resolve})
  let fail = true
  await page.route('**/api/v1/mcp-settings', async route => {
    await firstRequest
    if (fail) await route.fulfill({status: 403, json: {message: 'Could not load MCP connection settings.'}})
    else await route.fulfill({json: settings})
  })
  await page.goto('/')
  const trigger = page.getByRole('button', {name: 'MCP Settings', exact: true})
  await trigger.click()
  const dialog = page.getByRole('dialog', {name: 'MCP Settings'})
  await expect(dialog.getByText('Loading MCP connection settings…')).toBeVisible()
  completeRequest()
  await expect(dialog.getByRole('alert')).toContainText('Could not load MCP connection settings.')
  fail = false
  await dialog.getByRole('button', {name: 'Retry'}).click()
  await dialog.getByRole('button', {name: 'Copy endpoint'}).click()
  await expect(dialog.getByText(/Clipboard unavailable/)).toBeVisible()
  const endpoint = dialog.getByLabel('MCP endpoint')
  await endpoint.focus()
  expect(await endpoint.evaluate((field: HTMLTextAreaElement) => field.value.slice(field.selectionStart, field.selectionEnd))).toBe(settings.url)
  await dialog.getByRole('button', {name: 'Close'}).click()
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()
})
