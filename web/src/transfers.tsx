import { useRef, useState } from 'react'
import type { Client, ObjectRef, Upload } from './api'
import { HTTPError } from './api'
import { formatBytes } from './format'

type Transfer = {
  token: string
  object: ObjectRef
  name: string
  size: number
  lastModified: number
  singlePut?: boolean
  id?: string
  fingerprints?: Record<number, string>
  sent: number
  status: 'queued' | 'uploading' | 'paused' | 'complete' | 'failed'
  message?: string
}

async function digest(blob: Blob) {
  const hash = await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())
  return Array.from(new Uint8Array(hash), byte => byte.toString(16).padStart(2, '0')).join('')
}

export function useTransfers(client: Client, subject: string, refresh: (object: ObjectRef) => void) {
  const storageKey = 'cdn-manager:transfers:' + subject
  const [rows, setRows] = useState<Transfer[]>(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(storageKey) || '[]') as Transfer[]
      return saved.map(row => ({...row, status: 'paused'}))
    } catch {
      return []
    }
  })
  const latest = useRef(rows)
  const files = useRef(new Map<string, File>())
  const running = useRef<{ token: string; controller: AbortController } | null>(null)
  const storageWarning = useRef(false)

  function store(next: Transfer[]) {
    latest.current = next
    try {
      localStorage.setItem(storageKey, JSON.stringify(next.filter(row => row.status !== 'complete' && row.id)))
    } catch {
      storageWarning.current = true
    }
    setRows(next)
  }

  function update(token: string, changes: Partial<Transfer>) {
    store(latest.current.map(row => row.token === token ? {...row, ...changes} : row))
  }

  async function pump() {
    if (running.current) return
    const row = latest.current.find(row => row.status === 'queued')
    if (!row) return
    const file = files.current.get(row.token)
    if (!file) return
    const controller = new AbortController()
    running.current = {token: row.token, controller}
    const signal = controller.signal
    const set = (changes: Partial<Transfer>) => update(row.token, changes)
    set({status: 'uploading', message: undefined})
    try {
      if (row.singlePut || file.size === 0) {
        await client.putObject(row.object, file, signal)
      } else {
        const session = row.id ? await client.upload(row.id, signal) : await client.start(row.object, file, signal)
        set({id: session.id})
        if (session.status !== 'complete' && session.status !== 'completed') {
          await transferParts(row, file, session, signal, set)
        }
      }
      set({status: 'complete', sent: file.size})
      files.current.delete(row.token)
      refresh(row.object)
    } catch (error) {
      const failed = error instanceof HTTPError && [400, 403, 404, 410].includes(error.status)
      set({
        status: failed ? 'failed' : 'paused',
        message: signal.aborted ? undefined : error instanceof TypeError ? 'Connection interrupted. Resume to continue.' :
          error instanceof Error ? error.message : 'Transfer interrupted',
      })
    } finally {
      running.current = null
      void pump()
    }
  }

  async function transferParts(
    row: Transfer, file: File, session: Upload, signal: AbortSignal, set: (changes: Partial<Transfer>) => void,
  ) {
    const {partSize} = session
    if (!Number.isSafeInteger(partSize) || partSize < 1) throw new Error('Invalid part size from the server')
    const confirmed = new Map((session.parts ?? []).map(part => [part.number, part.size]))
    const fingerprints = {...row.fingerprints}
    let sent = 0
    for (const [number, size] of confirmed) {
      signal.throwIfAborted()
      const offset = (number - 1) * partSize
      const blob = file.slice(offset, offset + partSize)
      if (offset < 0 || offset >= file.size || size !== blob.size || fingerprints[number] !== await digest(blob)) {
        throw new Error('Selected file differs from the original upload. Select the original file to resume.')
      }
      sent += size
    }
    set({sent})
    for (let offset = 0, number = 1; offset < file.size; offset += partSize, number++) {
      signal.throwIfAborted()
      if (confirmed.has(number)) continue
      const blob = file.slice(offset, offset + partSize)
      fingerprints[number] = await digest(blob)
      set({fingerprints})
      await client.part(session.id, number, blob, signal)
      sent += blob.size
      set({sent})
    }
    await client.complete(session.id, signal)
  }

  function enqueue(objects: {file: File; object: ObjectRef}[], singlePut: boolean) {
    const queued = objects.map(({file, object}): Transfer => {
      const token = crypto.randomUUID()
      const row = {token, object, name: file.name, size: file.size, lastModified: file.lastModified, sent: 0, singlePut}
      const maxSinglePutBytes = 64 * 1024 * 1024
      if (singlePut && file.size > maxSinglePutBytes) {
        return {...row, status: 'failed', message: 'This source accepts files up to ' + formatBytes(maxSinglePutBytes) + '.'}
      }
      files.current.set(token, file)
      return {...row, status: 'queued'}
    })
    store([...latest.current, ...queued])
    void pump()
  }

  function resume(row: Transfer, selected?: File) {
    const file = selected ?? files.current.get(row.token)
    if (!file) return
    if (file.name !== row.name || file.size !== row.size || file.lastModified !== row.lastModified) {
      update(row.token, {message: 'Select the original file with the same name, size, and modification time.'})
      return
    }
    files.current.set(row.token, file)
    update(row.token, {status: 'queued', message: undefined})
    void pump()
  }

  async function discard(row: Transfer) {
    if (row.id && row.status !== 'complete') {
      try {
        await client.abort(row.id)
      } catch (error) {
        if (!(error instanceof HTTPError) || ![404, 410].includes(error.status)) {
          update(row.token, {message: error instanceof Error ? error.message : 'Could not abort transfer'})
          return
        }
      }
    }
    files.current.delete(row.token)
    store(latest.current.filter(entry => entry.token !== row.token))
  }

  return {rows, enqueue, resume, discard, storageWarning: storageWarning.current, hasFile: (token: string) => files.current.has(token),
    pause: (token: string) => {
      if (running.current?.token === token) running.current.controller.abort()
      else update(token, {status: 'paused'})
    },
  }
}

export function Transfers({transfers}: {transfers: ReturnType<typeof useTransfers>}) {
  return <section aria-label="Transfers">
    {transfers.storageWarning && <p className="error" role="alert">Browser storage is unavailable. Keep this page open to resume transfers.</p>}
    {transfers.rows.length === 0 && <div className="empty">No uploads in this browser.</div>}
    <ul className="transfer-list">
      {transfers.rows.map(row => <li key={row.token}>
        <div className="transfer-target">
          <strong className="key">{row.object.key}</strong>
          <span>{row.object.source} / {row.object.bucket}</span>
        </div>
        <div className="transfer-progress">
          <progress value={row.sent || (row.status === 'complete' ? 1 : 0)} max={row.size || 1} aria-label={'Transfer progress for ' + row.object.key} />
          <span>{row.status} · {formatBytes(row.sent)} / {formatBytes(row.size)}</span>
          {row.message && <span className="error" role="alert">{row.message}</span>}
        </div>
        <div className="transfer-actions">
          {['queued', 'uploading'].includes(row.status) ?
            <button onClick={() => transfers.pause(row.token)}>Pause</button> :
            <>
              {row.status === 'paused' && (transfers.hasFile(row.token) ?
                <button onClick={() => transfers.resume(row)}>Resume</button> :
                <label className="button pick-file">Reselect file<input type="file" onChange={event => {
                  const file = event.currentTarget.files?.[0]
                  event.currentTarget.value = ''
                  if (file) transfers.resume(row, file)
                }} /></label>)}
              <button onClick={() => {
                if (row.status === 'complete' || !row.id || confirm('Abort this upload and discard its uploaded parts?')) void transfers.discard(row)
              }}>{row.status === 'complete' || !row.id ? 'Dismiss' : 'Abort'}</button>
            </>}
        </div>
      </li>)}
    </ul>
  </section>
}
