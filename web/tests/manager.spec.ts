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
        if (request.method() === 'GET') return route.fulfill({status: 404})
        expect(request.method()).toBe('PUT')
        expect(url.searchParams.get('key')).toBe('notes.txt')
        expect(url.searchParams.get('createOnly')).toBe('true')
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

test('restores a source and its bucket after reload and falls back when access changes', async ({page, context}) => {
  let mediaAvailable = true
  let subject = 'owner'
  await context.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    switch (url.pathname) {
      case '/api/v1/session': return route.fulfill({json: {subject, csrfToken: 'fixture'}})
      case '/api/v1/sources': return route.fulfill({json: {sources: ['cdn', ...(mediaAvailable ? ['media'] : [])].map(id => ({id, capabilities: {}}))}})
      case '/api/v1/buckets': return route.fulfill({json: {buckets: (url.searchParams.get('source') === 'media' ? ['photos', 'videos'] : ['assets']).map(name => ({name}))}})
      case '/api/v1/objects': return route.fulfill({json: {objects: []}})
      default: return route.fulfill({status: 404})
    }
  })
  await page.goto('/')
  await page.getByLabel('Storage source').selectOption('media')
  await page.getByLabel('Bucket', {exact: true}).selectOption('videos')
  await page.reload()
  await expect(page.getByLabel('Storage source')).toHaveValue('media')
  await expect(page.getByLabel('Bucket', {exact: true})).toHaveValue('videos')
  const other = await context.newPage()
  await other.goto('/')
  await expect(other.getByLabel('Bucket', {exact: true})).toHaveValue('videos')
  subject = 'viewer'
  await other.reload()
  await expect(other.getByLabel('Storage source')).toHaveValue('cdn')
  subject = 'owner'
  mediaAvailable = false
  await page.reload()
  await expect(page.getByLabel('Storage source')).toHaveValue('cdn')
  await expect(page.getByLabel('Bucket', {exact: true})).toHaveValue('assets')
})

test('reviews a batch before writing and resolves a destination claimed after the check', async ({page}) => {
  const files = new Map([['rename.txt', 'first'], ['rename (1).txt', 'occupied'], ['replace.txt', 'second'], ['skip.txt', 'third']])
  const writes: string[] = []
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const key = url.searchParams.get('key') ?? ''
    switch (url.pathname) {
      case '/api/v1/session': return route.fulfill({json: {subject: 'editor', csrfToken: 'fixture'}})
      case '/api/v1/sources': return route.fulfill({json: {sources: [{id: 'cdn', capabilities: {upload: true}}]}})
      case '/api/v1/buckets': return route.fulfill({json: {buckets: [{name: 'assets'}]}})
      case '/api/v1/objects': return route.fulfill({json: {objects: [], nextCursor: 'page2'}})
      case '/api/v1/object':
        if (request.method() === 'GET') return files.has(key) ? route.fulfill({json: {key, size: 5}}) : route.fulfill({status: 404})
        if (key === 'late.txt' && !files.has(key)) files.set(key, 'concurrent')
        if (url.searchParams.get('createOnly') === 'true' && files.has(key)) {
          return route.fulfill({status: 409, json: {code: 'object_exists', message: 'An object already exists at this destination.'}})
        }
        writes.push(key)
        files.set(key, request.postData()!)
        return route.fulfill({status: 201})
      default: return route.fulfill({status: 404})
    }
  })
  await page.goto('/')
  await page.locator('input[type=file]:not([webkitdirectory])').setInputFiles(
    ['rename.txt', 'replace.txt', 'skip.txt', 'late.txt'].map(name => ({name, mimeType: 'text/plain', buffer: Buffer.from('new')})),
  )
  const review = page.getByRole('dialog', {name: 'Upload conflicts'})
  await expect(review.getByRole('group', {name: 'rename.txt'}).getByLabel('New file name')).toHaveValue('rename (2).txt')
  await review.getByRole('group', {name: 'replace.txt'}).getByLabel('Action').selectOption('replace')
  await review.getByRole('group', {name: 'skip.txt'}).getByLabel('Action').selectOption('skip')
  expect(writes).toEqual([])
  await review.getByRole('button', {name: 'Confirm replacement'}).click()
  await page.getByRole('button', {name: 'Review conflict'}).click()
  await expect(review.getByLabel('New file name')).toHaveValue('late (1).txt')
  await review.getByRole('button', {name: 'Continue upload'}).click()
  await expect(page.getByText('complete · 3 B / 3 B')).toHaveCount(3)
  expect(writes).toEqual(['rename (2).txt', 'replace.txt', 'late (1).txt'])
  expect(files.get('rename.txt')).toBe('first')
  expect(files.get('replace.txt')).toBe('new')
  expect(files.get('skip.txt')).toBe('third')
  expect(files.get('late.txt')).toBe('concurrent')
})

test('prompts for sign-in when a transfer encounters an expired session', async ({page}) => {
  let signedIn = true
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    switch (new URL(request.url()).pathname) {
      case '/api/v1/session': return signedIn ? route.fulfill({json: {subject: 'owner', csrfToken: 'fixture'}}) : route.fulfill({status: 401})
      case '/api/v1/sources': return route.fulfill({json: {sources: [{id: 'cdn', capabilities: {upload: true}}]}})
      case '/api/v1/buckets': return route.fulfill({json: {buckets: [{name: 'assets'}]}})
      case '/api/v1/objects': return route.fulfill({json: {objects: []}})
      case '/api/v1/object':
        if (request.method() === 'GET') return route.fulfill({status: 404})
        signedIn = false
        return route.fulfill({status: 401})
      default: return route.fulfill({status: 404})
    }
  })
  await page.goto('/')
  await page.locator('input[type=file]:not([webkitdirectory])').setInputFiles({name: 'paused.txt', mimeType: 'text/plain', buffer: Buffer.from('draft')})
  await expect(page.getByText('Your session ended. Sign in again to resume your uploads.')).toBeVisible()
  signedIn = true
  await page.reload()
  await page.getByRole('button', {name: 'Transfers, 1 unfinished'}).click()
  await expect(page.getByText('paused.txt', {exact: true})).toBeVisible()
  await expect(page.getByText('Reselect file')).toBeVisible()
})
