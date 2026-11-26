// Loopback demo issuer for Compose and browser tests. The identities are fixtures.
import { createHash, generateKeyPairSync, randomUUID, sign, timingSafeEqual } from 'node:crypto'

const issuer = 'http://127.0.0.1:5556'
const manager = 'http://127.0.0.1:8080'
const inspectorCallback = 'http://127.0.0.1:6276/oauth/callback'
const clients = {
  'demo-web': { redirects: [manager + '/auth/callback'], secret: 'only-for-local-demo' },
  'demo-mcp': { redirects: [issuer + '/mcp-callback', inspectorCallback] },
  'demo-other-mcp': { redirects: [issuer + '/mcp-callback'] },
}
const users = Object.freeze({
  owner: { name: 'Demo Owner', groups: ['demo-admins'] },
  editor: { name: 'Demo Editor', groups: ['demo-editors'] },
  viewer: { name: 'Demo Viewer', groups: ['demo-viewers'] },
})
const { publicKey, privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 })
const key = { ...publicKey.export({ format: 'jwk' }), alg: 'RS256', use: 'sig', kid: randomUUID() }
const flows = new Map()
const codes = new Map()
const scopes = ['openid', 'profile', 'email', 'groups', 'read', 'write', 'upload', 'delete', 'presign', 'policy']
setInterval(() => {
  for (const entries of [flows, codes]) {
    for (const [id, entry] of entries) if (entry.deadline < Date.now()) entries.delete(id)
  }
}, 60_000).unref()

function issue(typ, claims) {
  const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url')
  const now = Math.floor(Date.now() / 1000)
  const input = encode({ alg: 'RS256', typ, kid: key.kid }) + '.' +
    encode({ iss: issuer, iat: now, exp: now + 900, ...claims })
  return input + '.' + sign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url')
}

function createAuthorization(params) {
  const client = params.get('client_id')
  const settings = Object.hasOwn(clients, client) && clients[client]
  const redirect = params.get('redirect_uri')
  const scope = params.get('scope') ?? ''
  const challenge = params.get('code_challenge') ?? ''
  if (!settings || !settings.redirects.includes(redirect) || params.get('response_type') !== 'code' ||
      params.get('code_challenge_method') !== 'S256' || !/^[A-Za-z0-9_-]{43}$/.test(challenge) ||
      !scope.split(/ +/).every(value => scopes.includes(value))) {
    return new Response('Invalid demo authorization request', { status: 400 })
  }
  const id = randomUUID()
  flows.set(id, { client, redirect, scope, challenge, state: params.get('state'), nonce: params.get('nonce'), deadline: Date.now() + 300_000 })
  const buttons = Object.entries(users).map(([value, user]) =>
    '<button name="subject" value="' + value + '">' + user.name + '</button>').join('')
  return new Response(
    '<!doctype html><html lang="en"><meta charset="utf-8"><title>Demo sign-in</title>' +
      '<main><h1>Choose a demo account</h1><form method="post">' +
      '<input type="hidden" name="flow" value="' + id + '">' + buttons + '</form></main></html>',
    { headers: { 'Content-Type': 'text/html; charset=utf-8', 'Content-Security-Policy': `default-src 'none'; form-action 'self' ${manager} ${inspectorCallback}` } },
  )
}

async function authorize(request) {
  const form = await request.formData()
  const id = form.get('flow')
  const sub = form.get('subject')
  const flow = flows.get(id)
  flows.delete(id)
  if (!flow || flow.deadline < Date.now() || !Object.hasOwn(users, sub)) {
    return new Response('Demo login expired', { status: 400 })
  }
  const code = randomUUID()
  codes.set(code, { ...flow, sub, deadline: Date.now() + 60_000 })
  const redirect = new URL(flow.redirect)
  redirect.searchParams.set('code', code)
  if (flow.state) redirect.searchParams.set('state', flow.state)
  return Response.redirect(redirect, 303)
}

async function exchange(request) {
  const form = await request.formData()
  const code = form.get('code')
  const flow = codes.get(code)
  if (!flow || flow.deadline < Date.now()) return Response.json({ error: 'invalid_grant' }, { status: 400 })
  let id = form.get('client_id')
  let secret = form.get('client_secret')
  const auth = request.headers.get('Authorization')
  if (auth?.startsWith('Basic ')) {
    ;[id, secret] = Buffer.from(auth.slice(6), 'base64').toString().split(':').map(decodeURIComponent)
  }
  if (id !== flow.client || (clients[id].secret && secret !== clients[id].secret)) {
    return Response.json({ error: 'invalid_client' }, { status: 401 })
  }
  const verifier = form.get('code_verifier')
  if (typeof verifier !== 'string') return Response.json({ error: 'invalid_grant' }, { status: 400 })
  const challenge = createHash('sha256').update(verifier).digest('base64url')
  if (form.get('grant_type') !== 'authorization_code' || flow.redirect !== form.get('redirect_uri') ||
      !timingSafeEqual(Buffer.from(flow.challenge), Buffer.from(challenge))) {
    return Response.json({ error: 'invalid_grant' }, { status: 400 })
  }
  codes.delete(code)
  const identity = { sub: flow.sub, aud: id, ...users[flow.sub] }
  return Response.json({
    token_type: 'Bearer',
    expires_in: 900,
    scope: flow.scope,
    access_token: issue('at+jwt', { ...identity, scope: flow.scope, client_id: id }),
    id_token: issue('JWT', { ...identity, nonce: flow.nonce }),
  }, { headers: { 'Cache-Control': 'no-store' } })
}

Bun.serve({
  hostname: process.env.DEMO_ISSUER_HOST || '127.0.0.1',
  port: 5556,
  async fetch(request) {
    const url = new URL(request.url)
    switch (url.pathname) {
      case '/health': return new Response('OK')
      case '/.well-known/oauth-authorization-server':
      case '/.well-known/openid-configuration':
        return Response.json({
          issuer,
          authorization_endpoint: issuer + '/authorize',
          token_endpoint: issuer + '/token',
          jwks_uri: issuer + '/jwks',
          response_types_supported: ['code'],
          grant_types_supported: ['authorization_code'],
          subject_types_supported: ['public'],
          id_token_signing_alg_values_supported: ['RS256'],
          token_endpoint_auth_methods_supported: ['client_secret_basic', 'none'],
          code_challenge_methods_supported: ['S256'],
          scopes_supported: scopes,
        })
      case '/jwks': return Response.json({ keys: [key] })
      case '/authorize':
        if (request.method === 'GET') return createAuthorization(url.searchParams)
        if (request.method === 'POST') return authorize(request)
        break
      case '/token':
        if (request.method === 'POST') return exchange(request)
        break
      case '/mcp-callback': return new Response('Demo MCP authorization complete')
    }
    return new Response('Not Found', { status: 404 })
  },
})
