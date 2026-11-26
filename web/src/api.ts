export type Capabilities = Partial<Record<'createBucket' | 'deleteBucket' | 'policy' | 'upload' | 'multipart' | 'delete' | 'presign' | 'editText' | 'versions' | 'metadata', boolean>>
export type Source = { id: string; label?: string; region?: string; health?: string; capabilities: Capabilities }
export type Session = { subject: string; name?: string; csrfToken: string }
export type MCPConnection = { url: string; clientId: string; issuer: string; metadataUrl: string; scopes: string[] }
export type Bucket = { source: string; bucket: string }
export type ObjectRef = Bucket & { key: string; versionId?: string }
export type ObjectInfo = { key: string; size: number; lastModified?: string; etag?: string; contentType?: string; metadata?: Record<string, string> }
export type Page = { objects?: ObjectInfo[]; prefixes?: string[]; nextCursor?: string }
export type TextObject = { text: string; etag: string; contentType: string; maxBytes?: number }
export type Version = { id: string; size: number; lastModified: string; isLatest?: boolean; deleteMarker?: boolean }
export type Upload = { id: string; partSize: number; parts?: { number: number; size: number }[]; status: string }
type Params = Record<string, string | undefined>

export class HTTPError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

export function endpoint(path: string, params?: Params) {
  const query = new URLSearchParams()
  for (const [name, value] of Object.entries(params ?? {})) {
    if (value !== undefined) query.set(name, value)
  }
  return '/api/v1' + path + (query.size ? '?' + query.toString() : '')
}

async function request(path: string, params?: Params, options: RequestInit = {}) {
  const response = await fetch(endpoint(path, params), options)
  if (!response.ok) {
    const error = await response.json().catch(() => null)
    const message = error?.message ?? error?.error?.message ?? error?.error
    throw new HTTPError(response.status, typeof message === 'string' ? message : response.statusText || String(response.status))
  }
  return response
}

async function json<T>(path: string, params?: Params, options?: RequestInit): Promise<T> {
  const response = await request(path, params, options)
  return response.status === 204 ? undefined as T : response.json()
}

export function api(csrfToken = '') {
  function mutation(method: string, body?: unknown): RequestInit {
    const headers: Record<string, string> = { 'X-CSRF-Token': csrfToken }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    return { method, headers, body: body === undefined ? undefined : JSON.stringify(body) }
  }
  return {
    session: () => json<Session>('/session'),
    mcpSettings: (signal: AbortSignal) => json<MCPConnection>('/mcp-settings', undefined, {signal}),
    sources: () => json<{sources: Source[]}>('/sources'),
    buckets: (source: string) => json<{buckets: {name: string}[]}>('/buckets', {source}),
    createBucket: (bucket: Bucket) => request('/buckets', bucket, mutation('POST')),
    deleteBucket: (bucket: Bucket) => request('/buckets', bucket, mutation('DELETE')),
    policy: (bucket: Bucket) => json<{policy: string}>('/bucket-policy', bucket),
    savePolicy: (bucket: Bucket, policy: string) => request('/bucket-policy', bucket, mutation('PUT', {policy})),
    objects: (bucket: Bucket, prefix: string, cursor: string | undefined, signal: AbortSignal) =>
      json<Page>('/objects', {...bucket, prefix, cursor}, {signal}),
    stat: (object: ObjectRef) => json<ObjectInfo>('/object', object),
    cdn: (object: ObjectRef) => json<{url: string}>('/cdn-url', object),
    cdnStatus: (object: ObjectRef) => json<{available: boolean; status?: number}>('/cdn-status', object),
    presign: (object: ObjectRef, ttlSeconds: number) => json<{url: string}>('/presign', object, mutation('POST', {ttlSeconds})),
    readText: (object: ObjectRef) => json<TextObject>('/text', object),
    saveText: (object: ObjectRef, text: TextObject) => request('/text', object, mutation('PUT', {text: text.text, etag: text.etag, contentType: text.contentType})),
    versions: (object: ObjectRef, cursor: string | undefined, signal: AbortSignal) =>
      json<{versions: Version[]; nextCursor?: string}>('/versions', {...object, cursor}, {signal}),
    remove: (object: ObjectRef) => request('/object', object, mutation('DELETE')),
    removeMany: (bucket: Bucket, keys: string[]) => json<{results: {key: string; error?: string}[]}>('/delete', bucket, mutation('POST', {keys})),
    zip: (bucket: Bucket, keys: string[]) => request('/zip', bucket, mutation('POST', {keys})),
    putObject: (object: ObjectRef, body: File, signal: AbortSignal) =>
      request('/object', object, {method: 'PUT', headers: {'X-CSRF-Token': csrfToken, 'Content-Type': body.type || 'application/octet-stream'}, body, signal}),
    start: (object: ObjectRef, file: File, signal: AbortSignal) =>
      json<Upload>('/uploads', object, {...mutation('POST', {size: file.size, contentType: file.type || 'application/octet-stream'}), signal}),
    upload: (id: string, signal: AbortSignal) => json<Upload>('/uploads/' + encodeURIComponent(id), undefined, {signal}),
    part: (id: string, number: number, body: Blob, signal: AbortSignal) =>
      request('/uploads/' + encodeURIComponent(id) + '/parts/' + number, undefined, {
        method: 'PUT', headers: {'X-CSRF-Token': csrfToken, 'Content-Type': 'application/octet-stream'}, body, signal,
      }),
    complete: (id: string, signal: AbortSignal) =>
      request('/uploads/' + encodeURIComponent(id) + '/complete', undefined, {...mutation('POST'), signal}),
    abort: (id: string) => request('/uploads/' + encodeURIComponent(id), undefined, mutation('DELETE')),
    logout: async () => {
      const response = await fetch('/auth/logout', mutation('POST'))
      if (!response.ok) throw new HTTPError(response.status, 'Sign out failed')
    },
  }
}
export type Client = ReturnType<typeof api>
