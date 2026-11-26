import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { endpoint, HTTPError, type Capabilities, type Client, type ObjectRef, type TextObject } from './api'
import { formatBytes, formatDate, isText } from './format'

export function Failure({error, retry}: {error: unknown; retry?: () => void}) {
  return <div className="feedback error" role="alert">
    <span>{error instanceof Error ? error.message : 'Request failed'}</span>
    {retry && <button onClick={retry}>Retry</button>}
    {error instanceof HTTPError && error.status === 401 && <a href="/auth/login">Sign in again</a>}
  </div>
}

export function Modal({title, close, children}: {title: string; close: () => void; children: ReactNode}) {
  return <dialog className="dialog" ref={node => { if (node && !node.open) node.showModal() }} onCancel={event => {
    event.preventDefault()
    close()
  }} aria-label={title}>
    <header><h2>{title}</h2><button aria-label="Close" onClick={close}>×</button></header>
    <div className="dialog-body">{children}</div>
  </dialog>
}

export function CopyField({url, label}: {url: string; label: string}) {
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState<unknown>()
  return <>
    <div className="copy-field">
      <input readOnly value={url} aria-label={label} onFocus={event => event.target.select()} />
      <button onClick={async () => {
        try {
          await navigator.clipboard.writeText(url)
          setCopied(true)
          setError(undefined)
        } catch (err) { setError(err) }
      }}>{copied ? 'Copied' : 'Copy link'}</button>
    </div>
    {error !== undefined && <p className="error" role="alert">Clipboard unavailable. Select and copy the link from the field.</p>}
  </>
}

export function ObjectDetail({object, capabilities, client, changed, close}: {
  object: ObjectRef; capabilities: Capabilities; client: Client; changed: () => void; close: () => void
}) {
  const query = useQuery({queryKey: ['object', object], queryFn: () => client.stat(object)})
  const cdn = useQuery({queryKey: ['cdn', object], queryFn: () => client.cdn(object)})
  const cdnStatus = useMutation({mutationFn: () => client.cdnStatus(object)})
  const [mode, setMode] = useState<'details' | 'text' | 'versions'>('details')
  const [editorOpened, setEditorOpened] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [ttl, setTtl] = useState(900)
  const [signed, setSigned] = useState('')
  const [failure, setFailure] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [showPreview, setShowPreview] = useState(false)
  const details = query.data
  const type = details?.contentType || ''
  const safeImage = /^image\/(?:png|jpeg|gif|webp|avif|bmp)(?:;|$)/i.test(type)
  const playable = /^(?:video|audio)\//i.test(type)
  return <Modal title="Object details" close={() => {
    if (!dirty || confirm('Discard your unsaved draft?')) close()
  }}>
    <p className="object-key key">{object.key}</p>
    <div className="detail-nav">
      <button aria-pressed={mode === 'details'} onClick={() => setMode('details')}>Details</button>
      {isText(type) && <button aria-pressed={mode === 'text'} onClick={() => {setEditorOpened(true); setMode('text')}}>{capabilities.editText ? 'Edit text' : 'Read text'}</button>}
      {capabilities.versions && <button aria-pressed={mode === 'versions'} onClick={() => setMode('versions')}>Versions</button>}
    </div>
    <div hidden={mode !== 'details'}>
      {query.isPending && <p role="status">Loading object…</p>}
      {query.error && <Failure error={query.error} retry={() => void query.refetch()} />}
      {details && <>
        <dl className="facts">
          <dt>Size</dt><dd>{formatBytes(details.size)}</dd>
          <dt>Modified</dt><dd>{formatDate(details.lastModified)}</dd>
          <dt>Content type</dt><dd>{type || '—'}</dd>
          <dt>ETag</dt><dd className="key">{details.etag ?? '—'}</dd>
          {Object.entries(details.metadata ?? {}).map(([key, value]) => <div key={key} className="fact"><dt>{key}</dt><dd>{value}</dd></div>)}
        </dl>
        <div className="actions">
          <a className="button" href={endpoint('/download', object)} download>Download</a>
          {(safeImage || playable) && <button onClick={() => setShowPreview(!showPreview)}>{showPreview ? 'Hide preview' : 'Preview'}</button>}
          {capabilities.delete && <button disabled={busy} className="danger" onClick={async () => {
            if (!confirm('Delete ' + object.key + '?')) return
            setBusy(true)
            try { await client.remove(object); changed(); close() }
            catch (error) { setFailure(error) }
            finally { setBusy(false) }
          }}>Delete object</button>}
        </div>
        {showPreview && (safeImage ? <img className="media-preview" src={endpoint('/preview', object)} alt={object.key} /> :
          /^video\//i.test(type) ? <video className="media-preview" src={endpoint('/preview', object)} controls preload="metadata" /> :
            <audio src={endpoint('/preview', object)} controls preload="metadata" />)}
      </>}
      <section className="detail-section">
        <h3>Permanent CDN link</h3>
        {cdn.isPending && <p role="status">Loading public URL…</p>}
        {cdn.error && <Failure error={cdn.error} retry={() => void cdn.refetch()} />}
        {cdn.data && <><CopyField url={cdn.data.url} label="Permanent CDN link" />
          <div className="actions cdn-actions">
            <a href={cdn.data.url} target="_blank" rel="noopener noreferrer">Open public URL ↗</a>
            <button disabled={cdnStatus.isPending} onClick={() => cdnStatus.mutate()}>
              {cdnStatus.isPending ? 'Checking availability…' : 'Check availability'}
            </button>
          </div>
          {cdnStatus.error && <Failure error={cdnStatus.error} />}
          {cdnStatus.data && <p role="status" className={cdnStatus.data.available ? 'muted' : 'error'}>
            Checked from the manager · {cdnStatus.data.available ? 'Available' : 'Unavailable'}
            {cdnStatus.data.status && ' · HTTP ' + cdnStatus.data.status}
          </p>}
        </>}
      </section>
      {capabilities.presign && <section className="detail-section">
        <h3>Temporary S3 link</h3>
        <p className="muted">The recipient needs network access to the S3 endpoint.</p>
        <form className="actions" onSubmit={async event => {
          event.preventDefault()
          setBusy(true)
          setSigned('')
          setFailure(undefined)
          try { setSigned((await client.presign(object, ttl)).url) }
          catch (error) { setFailure(error) }
          finally { setBusy(false) }
        }}>
          <label>Lifetime <select value={ttl} onChange={event => setTtl(Number(event.target.value))}>
            <option value={300}>5 minutes</option><option value={900}>15 minutes</option><option value={3600}>1 hour</option>
          </select></label>
          <button disabled={busy}>Generate link</button>
        </form>
        {signed && <CopyField key={signed} url={signed} label="Temporary S3 link" />}
      </section>}
      {failure !== undefined && <Failure error={failure} />}
    </div>
    {editorOpened && <div hidden={mode !== 'text'}><TextEditor {...{object, client, changed, setDirty}} writable={!!capabilities.editText} /></div>}
    {mode === 'versions' && <Versions {...{object, client}} />}
  </Modal>
}

function TextEditor({object, client, changed, writable, setDirty}: {
  object: ObjectRef; client: Client; changed: () => void; writable: boolean; setDirty: (value: boolean) => void
}) {
  const queryClient = useQueryClient()
  const queryKey = ['text', object] as const
  const query = useQuery({queryKey, queryFn: () => client.readText(object)})
  const [draft, setDraft] = useState<TextObject>()
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [latest, setLatest] = useState<TextObject>()
  const [readError, setReadError] = useState<unknown>()
  if (query.isPending) return <p role="status">Loading text…</p>
  if (query.error && !draft) return <Failure error={query.error} retry={() => void query.refetch()} />
  const text = draft ?? query.data
  if (!text) return null
  const size = new TextEncoder().encode(text.text).length
  const max = text.maxBytes ?? 1024 * 1024
  const conflict = error instanceof HTTPError && [409, 412].includes(error.status)
  return <form onSubmit={async event => {
    event.preventDefault()
    if (!draft || busy || !writable || size > max || conflict) return
    setBusy(true)
    setError(undefined)
    try {
      await client.saveText(object, draft)
      setSaved(true)
      setDraft(undefined)
      setDirty(false)
      changed()
      await queryClient.invalidateQueries({queryKey})
      await queryClient.invalidateQueries({queryKey: ['object', object]})
    } catch (err) { setError(err) }
    finally { setBusy(false) }
  }}>
    <label className="editor-label" htmlFor="object-text">File contents</label>
    <textarea id="object-text" value={text.text} readOnly={!writable || busy} spellCheck={false} onChange={event => {
      setDraft({...text, text: event.target.value})
      setSaved(false)
      setDirty(true)
    }} />
    <div className="actions">
      {writable && <button className="primary" disabled={!draft || busy || size > max || conflict}>{busy ? 'Saving…' : 'Save changes'}</button>}
      <span className={size > max ? 'error' : 'muted'}>{formatBytes(size)} / {formatBytes(max)}</span>
      {saved && <span role="status">Saved</span>}
    </div>
    {error !== undefined && (conflict ? <div className="feedback error" role="alert">
      <span>The object changed since you opened it. Your draft is still in the editor.</span>
      <button type="button" onClick={async () => {
        try { setReadError(undefined); setLatest(await client.readText(object)) }
        catch (err) { setReadError(err) }
      }}>View current contents</button>
    </div> : <Failure error={error} />)}
    {readError !== undefined && <Failure error={readError} />}
    {latest && <div className="detail-section">
      <label className="editor-label" htmlFor="latest-text">Current contents on the server</label>
      <textarea id="latest-text" readOnly value={latest.text} />
      <button type="button" onClick={() => {
        setDraft({...latest, text: text.text})
        setError(undefined)
        setLatest(undefined)
      }}>Keep draft and review for saving</button>
    </div>}
  </form>
}

function Versions({object, client}: {object: ObjectRef; client: Client}) {
  const query = useInfiniteQuery({
    queryKey: ['versions', object],
    initialPageParam: undefined as string | undefined,
    queryFn: ({pageParam, signal}) => client.versions(object, pageParam, signal),
    getNextPageParam: page => page.nextCursor || undefined,
  })
  if (query.isPending) return <p role="status">Loading versions…</p>
  if (query.error && !query.data) return <Failure error={query.error} retry={() => void query.refetch()} />
  const versions = query.data?.pages.flatMap(page => page.versions ?? []) ?? []
  return <>
    {versions.length ? <ul className="version-list">
      {versions.map(version => <li key={version.id}>
        <div><strong className="key">{version.id}</strong>
          <small>{formatDate(version.lastModified)} · {version.deleteMarker ? 'Delete marker' : formatBytes(version.size)}{version.isLatest ? ' · Latest' : ''}</small></div>
        {!version.deleteMarker && <a className="button" download href={endpoint('/download', {...object, versionId: version.id})}>Download</a>}
      </li>)}
    </ul> : <p>No versions returned.</p>}
    {query.isFetchNextPageError && <Failure error={query.error} />}
    {query.hasNextPage && <button className="version-next" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
      {query.isFetchingNextPage ? 'Loading versions…' : 'Load more versions'}
    </button>}
  </>
}
