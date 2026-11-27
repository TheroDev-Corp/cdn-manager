import { expect, test } from '@playwright/test'

test('copies an exact CDN link and preserves a conflicted text draft', async ({page, context}) => {
  const key = 'сообщение #50%.css'
  const url = 'https://cdn.example.test/' + encodeURIComponent(key)
  const seen: string[] = []
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const parsed = new URL(request.url())
    const endpoint = parsed.pathname.slice('/api/v1'.length)
    if (parsed.searchParams.has('key')) seen.push(parsed.searchParams.get('key')!)
    switch (endpoint) {
      case '/session': return route.fulfill({json: {subject: 'owner', csrfToken: 'test-csrf'}})
      case '/sources': return route.fulfill({json: {sources: [{id: 'assets', label: 'Assets', region: 'test-1', capabilities: {editText: true}}]}})
      case '/buckets': return route.fulfill({json: {buckets: [{name: 'files'}]}})
      case '/objects': return route.fulfill({json: {prefixes: ['styles/'], objects: [{key, size: 13}]}})
      case '/object': return route.fulfill({json: {key, size: 13, contentType: 'text/css', etag: '"old"'}})
      case '/cdn-url': return route.fulfill({json: {url}})
      case '/cdn-status': return route.fulfill({json: {available: false, status: 404}})
      case '/text': {
        if (request.method() === 'PUT') {
          expect(request.headers()['x-csrf-token']).toBe('test-csrf')
          expect(request.postDataJSON()).toMatchObject({text: 'a { color: blue }', etag: '"old"'})
          return route.fulfill({status: 412, json: {message: 'Object changed'}})
        }
        return route.fulfill({json: {text: 'a { color: red }', etag: '"old"', contentType: 'text/css', maxBytes: 1024}})
      }
      default: return route.fulfill({status: 404})
    }
  })
  await page.goto('/')
  await page.getByRole('button', {name: 'CDN link for ' + key}).click()
  await expect(page.getByRole('textbox', {name: 'Permanent CDN link'})).toHaveValue(url)
  await page.getByRole('button', {name: 'Copy link'}).click()
  await expect(page.getByRole('button', {name: 'Copied'})).toBeVisible()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(url)
  await page.getByRole('button', {name: 'Close'}).click()
  await page.getByRole('button', {name: key, exact: true}).click()
  await page.getByRole('button', {name: 'Check availability'}).click()
  await expect(page.getByRole('dialog').getByRole('status')).toHaveText('Checked from the manager · Unavailable · HTTP 404')
  await page.getByRole('button', {name: 'Edit text'}).click()
  await page.getByLabel('File contents').fill('a { color: blue }')
  await page.getByRole('button', {name: 'Save changes'}).click()
  await expect(page.getByRole('alert')).toContainText('Your draft is still in the editor.')
  await expect(page.getByLabel('File contents')).toHaveValue('a { color: blue }')
  await page.getByRole('button', {name: 'Details', exact: true}).click()
  await page.getByRole('button', {name: 'Edit text'}).click()
  await expect(page.getByLabel('File contents')).toHaveValue('a { color: blue }')
  page.once('dialog', prompt => prompt.dismiss())
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog', {name: 'Object details'})).toBeVisible()
  await expect(page.getByLabel('File contents')).toHaveValue('a { color: blue }')
  page.once('dialog', prompt => prompt.accept())
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog', {name: 'Object details'})).toHaveCount(0)
  await expect(page.getByRole('button', {name: key, exact: true})).toBeFocused()
  expect(seen.length).toBeGreaterThan(3)
  expect(seen.every(value => value === key)).toBe(true)
})

test('resumes a multipart upload after the browser reloads and skips confirmed parts', async ({page}) => {
  const calls: number[] = []
  const received: {number: number; size: number}[] = []
  let interrupted = false
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const parsed = new URL(request.url())
    const endpoint = parsed.pathname.slice('/api/v1'.length)
    switch (endpoint) {
      case '/session': return route.fulfill({json: {subject: 'editor', csrfToken: 'test-csrf'}})
      case '/sources': return route.fulfill({json: {sources: [{id: 'media', capabilities: {upload: true, multipart: true}}]}})
      case '/buckets': return route.fulfill({json: {buckets: [{name: 'videos'}]}})
      case '/objects': return route.fulfill({json: {objects: []}})
      case '/uploads':
      case '/uploads/id': return route.fulfill({json: {id: 'id', partSize: 4, parts: received, status: 'active'}})
      case '/uploads/id/parts/1':
      case '/uploads/id/parts/2': {
        const number = Number(endpoint.at(-1))
        calls.push(number)
        if (number === 2 && !interrupted) {
          interrupted = true
          return route.abort('failed')
        }
        received.push({number, size: request.postDataBuffer()!.length})
        return route.fulfill({status: 204})
      }
      case '/uploads/id/complete': return route.fulfill({status: 204})
      default: return route.fulfill({status: 404})
    }
  })
  const pickOriginal = async (selector: string) => page.locator(selector).evaluate(element => {
    const input = element as HTMLInputElement
    const files = new DataTransfer()
    files.items.add(new File(['12345678'], 'clip.mp4', {type: 'video/mp4', lastModified: 100}))
    input.files = files.files
    input.dispatchEvent(new Event('change', {bubbles: true}))
  })
  await page.goto('/')
  await expect(page.getByRole('button', {name: 'Upload files'})).toBeVisible()
  await pickOriginal('input[type=file]:not([webkitdirectory])')
  await expect(page.getByRole('button', {name: 'Resume', exact: true})).toBeVisible()
  await page.reload()
  await page.getByRole('button', {name: 'Transfers'}).click()
  await expect(page.getByText('Reselect file')).toBeVisible()
  await pickOriginal('.pick-file input')
  await expect(page.getByText('complete · 8 B / 8 B')).toBeVisible()
  expect(calls).toEqual([1, 2, 2])
})

test('uploads to a source with no multipart support when browser storage is denied', async ({page}) => {
  await page.addInitScript(() => {
    Storage.prototype.setItem = () => { throw new DOMException('Storage permission denied', 'SecurityError') }
  })
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    switch (url.pathname) {
      case '/api/v1/session': return route.fulfill({json: {subject: 'editor', csrfToken: 'test-csrf'}})
      case '/api/v1/sources': return route.fulfill({json: {sources: [{id: 'assets', capabilities: {upload: true}}]}})
      case '/api/v1/buckets': return route.fulfill({json: {buckets: [{name: 'files'}]}})
      case '/api/v1/objects': return route.fulfill({json: {objects: []}})
      case '/api/v1/object':
        expect(request.method()).toBe('PUT')
        expect(url.searchParams.get('key')).toBe('notes.txt')
        expect(request.headers()['x-csrf-token']).toBe('test-csrf')
        expect(request.postData()).toBe('notes')
        return route.fulfill({status: 204})
      default: return route.fulfill({status: 404})
    }
  })
  await page.goto('/')
  await page.locator('input[type=file]:not([webkitdirectory])').setInputFiles({
    name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('notes'),
  })
  await expect(page.getByText('complete · 5 B / 5 B')).toBeVisible()
  await expect(page.getByRole('alert')).toContainText('Browser storage is unavailable.')
})
