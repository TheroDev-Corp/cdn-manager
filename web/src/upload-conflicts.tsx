import { useState } from 'react'
import { HTTPError, type Client, type ObjectRef } from './api'
import { Failure, Modal } from './detail'

export type UploadTarget = {file: File; object: ObjectRef; replace?: boolean}
export type UploadConflict = {index: number; object: ObjectRef; suggestion: string; canReplace: boolean}
type Choice = {action: 'rename' | 'skip' | 'replace'; name: string}
type Resolution = {index: number; object: ObjectRef; replace: boolean}

async function objectExists(client: Client, object: ObjectRef, signal?: AbortSignal) {
  try {
    await client.stat(object, signal)
    return true
  } catch (error) {
    if (error instanceof HTTPError && error.status === 404) return false
    throw error
  }
}

const parentKey = (key: string) => key.slice(0, key.lastIndexOf('/') + 1)
const keySize = (key: string) => new TextEncoder().encode(key).length

export async function suggestUploadName(client: Client, object: ObjectRef, reserved: Set<string>, signal?: AbortSignal) {
  const parent = parentKey(object.key)
  const name = object.key.slice(parent.length)
  const dot = name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const extension = dot > 0 ? name.slice(dot) : ''
  for (let number = 1; number <= 100; number++) {
    const suggestion = `${stem} (${number})${extension}`
    const key = parent + suggestion
    if (keySize(key) > 1024) return ''
    if (!reserved.has(key) && !await objectExists(client, {...object, key}, signal)) return suggestion
  }
  return ''
}

export async function inspectUploads(client: Client, targets: UploadTarget[], signal: AbortSignal) {
  const checked = new Map<string, boolean>()
  const reserved = new Set(targets.map(target => target.object.key))
  const keys = [...reserved]
  for (let offset = 0; offset < keys.length; offset += 8) {
    await Promise.all(keys.slice(offset, offset + 8).map(async key => {
      checked.set(key, await objectExists(client, {...targets[0].object, key}, signal))
    }))
  }

  const seen = new Set<string>()
  const conflicts: UploadConflict[] = []
  for (const [index, {object}] of targets.entries()) {
    if (checked.get(object.key) || seen.has(object.key)) {
      const suggestion = await suggestUploadName(client, object, reserved, signal)
      if (suggestion) reserved.add(parentKey(object.key) + suggestion)
      conflicts.push({index, object, suggestion, canReplace: !!checked.get(object.key) && !seen.has(object.key)})
    }
    seen.add(object.key)
  }
  return conflicts
}

export function UploadConflicts({client, conflicts, occupiedKeys, confirm, close}: {
  client: Client; conflicts: UploadConflict[]; occupiedKeys: string[];
  confirm: (resolutions: Resolution[]) => void | Promise<void>; close: () => void;
}) {
  const [choices, setChoices] = useState<Choice[]>(() => conflicts.map(row => ({action: 'rename', name: row.suggestion})))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const update = (index: number, choice: Choice) => setChoices(current => current.map((value, i) => i === index ? choice : value))
  const replacing = choices.some(choice => choice.action === 'replace')
  const {source, bucket} = conflicts[0].object

  return <Modal title="Upload conflicts" close={close} canClose={() => !busy}>
    <p className="object-key key">{source} / {bucket}</p>
    <p>These destination keys are already in use. Choose how to continue.</p>
    <form className="upload-conflicts" onSubmit={async event => {
      event.preventDefault()
      if (busy) return
      setBusy(true)
      setError(undefined)
      try {
        const reserved = new Set(occupiedKeys)
        const resolutions: Resolution[] = []
        for (const [i, {index, object}] of conflicts.entries()) {
          const {action, name} = choices[i]
          if (action === 'skip') continue
          const cleanName = name.trim()
          if (action === 'rename' && (!cleanName || cleanName === '.' || cleanName === '..' || /[/\\\x00-\x1f\x7f]/.test(cleanName))) {
            throw new Error('Enter a file name without slashes or control characters.')
          }
          const key = action === 'rename' ? parentKey(object.key) + cleanName : object.key
          if (keySize(key) > 1024) throw new Error('The destination key is too long.')
          if (reserved.has(key) || (action === 'rename' && await objectExists(client, {...object, key}))) {
            throw new Error(`“${key}” is already in use. Choose another name.`)
          }
          reserved.add(key)
          resolutions.push({index, object: {...object, key}, replace: action === 'replace'})
        }
        await confirm(resolutions)
      } catch (error) { setError(error) }
      finally { setBusy(false) }
    }}>
      {conflicts.map(({index, object, canReplace}, i) => <fieldset key={index} disabled={busy}>
        <legend className="key">{object.key}</legend>
        <label>Action
          <select value={choices[i].action} onChange={event => update(i, {...choices[i], action: event.target.value as Choice['action']})}>
            <option value="rename">Save as…</option>
            <option value="skip">Skip</option>
            {canReplace && <option value="replace">Replace</option>}
          </select>
        </label>
        {choices[i].action === 'rename' && <label>New file name
          <input value={choices[i].name} onChange={event => update(i, {...choices[i], name: event.target.value})} required />
        </label>}
      </fieldset>)}
      {error !== undefined && <Failure error={error} />}
      {replacing && <p role="status">Selected files will replace the existing objects at those keys.</p>}
      <div className="actions">
        <button type="button" onClick={close} disabled={busy}>Cancel</button>
        <button className={replacing ? 'danger' : 'primary'} disabled={busy}>
          {busy ? 'Checking…' : replacing ? 'Confirm replacement' : 'Continue upload'}
        </button>
      </div>
    </form>
  </Modal>
}
