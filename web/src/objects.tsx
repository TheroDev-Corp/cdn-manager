import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { endpoint, type Bucket, type Client, type ObjectRef, type Source } from './api'
import { CopyField, Failure, Modal, ObjectDetail } from './detail'
import { formatBytes, formatDate } from './format'

type Entry = {key: string; folder: boolean; size?: number; lastModified?: string}

export function Objects({source, bucket, client, csrfToken, enqueue}: {
  source: Source; bucket: Bucket; client: Client; csrfToken: string;
  enqueue: (entries: {file: File; object: ObjectRef}[]) => void
}) {
  const [prefix, setPrefix] = useState('')
  const [search, setSearch] = useState('')
  const [sort, setSort] = useState('name')
  const [selected, setSelected] = useState<string[]>([])
  const [object, setObject] = useState<ObjectRef>()
  const [link, setLink] = useState<{key: string; url: string}>()
  const [linkBusy, setLinkBusy] = useState('')
  const [policy, setPolicy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [failures, setFailures] = useState<{key: string; error?: string}[]>([])
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const folderInput = useRef<HTMLInputElement>(null)
  const objectHeading = useRef<HTMLHeadingElement>(null)
  const queryClient = useQueryClient()
  const query = useInfiniteQuery({
    queryKey: ['objects', bucket, prefix],
    initialPageParam: undefined as string | undefined,
    queryFn: ({pageParam, signal}) => client.objects(bucket, prefix, pageParam, signal),
    getNextPageParam: page => page.nextCursor || undefined,
  })
  const pages = query.data?.pages ?? []
  const entries: Entry[] = pages.flatMap(page => [
    ...(page.prefixes ?? []).map(key => ({key, folder: true})),
    ...(page.objects ?? []).map(item => ({...item, folder: false})),
  ])
  const visible = entries.filter(entry => entry.key.slice(prefix.length).toLocaleLowerCase().includes(search.toLocaleLowerCase())).sort((a, b) => {
    if (a.folder !== b.folder) return a.folder ? -1 : 1
    if (sort === 'size') return (b.size ?? 0) - (a.size ?? 0)
    if (sort === 'modified') return (b.lastModified ?? '').localeCompare(a.lastModified ?? '')
    return a.key.localeCompare(b.key)
  })
  const visibleObjects = visible.filter(entry => !entry.folder).map(entry => entry.key)
  const allSelected = visibleObjects.length > 0 && visibleObjects.every(key => selected.includes(key))
  const segments = prefix.split('/').slice(0, -1)
  const refresh = () => void queryClient.invalidateQueries({queryKey: ['objects', bucket]})

  function browse(next: string) {
    setPrefix(next)
    setSearch('')
    setSelected([])
    setFailures([])
    setError(undefined)
    objectHeading.current?.focus()
  }

  function addFiles(input: HTMLInputElement) {
    const files = Array.from(input.files ?? [])
    enqueue(files.map(file => ({file, object: {...bucket, key: prefix + (file.webkitRelativePath || file.name)}})))
    input.value = ''
  }

  return <>
    <header className="title object-title">
      <div>
        <h1 ref={objectHeading} tabIndex={-1}>Objects</h1>
        <nav className="breadcrumbs key" data-root={!prefix || undefined} aria-label="Object prefix">
          {prefix ? <button className="plain" onClick={() => browse('')}>{bucket.bucket}</button> :
            <span aria-current="location">{bucket.bucket}</span>}
          {segments.map((segment, index) => {
            const target = segments.slice(0, index + 1).join('/') + '/'
            const label = segment || '(empty)'
            return <span key={target}><span className="breadcrumb-divider" aria-hidden="true">/</span>
              {target === prefix ? <span aria-current="location">{label}</span> :
                <button className="plain" onClick={() => browse(target)}>{label}</button>}
            </span>
          })}
        </nav>
      </div>
      <div className="actions">
        {source.capabilities.policy && <button onClick={() => setPolicy(true)}>Bucket policy</button>}
        {source.capabilities.upload && <>
          <button onClick={() => folderInput.current?.click()}>Upload folder</button>
          <button className="primary" onClick={() => fileInput.current?.click()}>Upload files</button>
          <input hidden type="file" multiple ref={fileInput} onChange={event => addFiles(event.currentTarget)} />
          <input hidden type="file" multiple ref={node => {
            folderInput.current = node
            node?.setAttribute('webkitdirectory', '')
          }} onChange={event => addFiles(event.currentTarget)} />
        </>}
      </div>
    </header>
    <div className="table-toolbar">
      <div className="object-filter"><input type="search" aria-label="Filter loaded objects" placeholder="Filter loaded keys…" value={search} onChange={event => setSearch(event.target.value)} /></div>
      <div className="table-controls">
        <span className="object-count" aria-live="polite">{search ? `${visible.length} of ${entries.length} loaded` : `${entries.length} loaded`}</span>
        <label>Sort <select aria-label="Sort objects" value={sort} onChange={event => setSort(event.target.value)}>
          <option value="name">Name</option><option value="modified">Newest</option><option value="size">Largest</option>
        </select></label>
        <button onClick={() => void query.refetch()} disabled={query.isFetching}>Refresh</button>
      </div>
    </div>
    {selected.length > 0 && <div className="selection-bar">
      <strong>{selected.length} selected</strong>
      <form method="post" action={endpoint('/zip', bucket)} target="_blank" rel="noopener">
        <input name="csrfToken" value={csrfToken} type="hidden" />
        {selected.map(key => <input key={key} name="keys" value={key} type="hidden" />)}
        <button>Download ZIP</button>
      </form>
      {source.capabilities.delete && <button className="danger" disabled={busy} onClick={async () => {
        if (!confirm('Delete ' + selected.length + ' selected objects?')) return
        setBusy(true)
        setFailures([])
        setError(undefined)
        try {
          const result = await client.removeMany(bucket, selected)
          const failed = (result.results ?? []).filter(entry => entry.error)
          setFailures(failed)
          setSelected(failed.map(entry => entry.key))
          refresh()
        } catch (err) { setError(err) }
        finally { setBusy(false) }
      }}>Delete selected</button>}
      <button className="plain" onClick={() => setSelected([])}>Clear selection</button>
    </div>}
    {failures.length > 0 && <ul className="feedback error" role="alert">{failures.map(entry =>
      <li key={entry.key}><span className="key">{entry.key}</span> · {entry.error}</li>)}</ul>}
    {error !== undefined && <Failure error={error} />}
    {query.error && <Failure error={query.error} retry={() => void query.refetch()} />}
    <table className="objects">
      <thead><tr>
        <th className="select-col"><label className="object-check"><input type="checkbox" aria-label="Select all visible objects" checked={allSelected} disabled={visibleObjects.length === 0} onChange={event => {
          setSelected(event.target.checked ? [...new Set([...selected, ...visibleObjects])] : selected.filter(key => !visibleObjects.includes(key)))
        }} /></label></th>
        <th scope="col">Object key</th><th scope="col" className="numeric">Size</th><th scope="col" className="date-cell">Modified</th><th scope="col" className="row-actions">Link</th>
      </tr></thead>
      <tbody>{visible.map(entry => <tr key={(entry.folder ? 'prefix:' : 'object:') + entry.key}>
        <td className="select-col">{!entry.folder && <label className="object-check"><input type="checkbox" aria-label={'Select ' + entry.key} checked={selected.includes(entry.key)} onChange={event => {
          setSelected(event.target.checked ? [...selected, entry.key] : selected.filter(key => key !== entry.key))
        }} /></label>}</td>
        <td className="name-cell">
          <button onClick={() => entry.folder ? browse(entry.key) : setObject({...bucket, key: entry.key})} className="plain object-name" title={entry.key}>
            <span className={'object-icon ' + (entry.folder ? 'folder' : 'file')} aria-hidden="true" />
            <span className="key">{entry.key.slice(prefix.length) || entry.key}</span>
          </button>
          {!entry.folder && <small className="mobile-meta">
            <span className="mobile-size">{entry.size === undefined ? '—' : formatBytes(entry.size)} · </span>{formatDate(entry.lastModified)}
          </small>}
        </td>
        <td className="numeric">{entry.folder || entry.size === undefined ? '—' : formatBytes(entry.size)}</td>
        <td className="date-cell">{formatDate(entry.lastModified)}</td>
        <td className="row-actions">{!entry.folder && <button className="plain row-link" aria-disabled={linkBusy !== ''} aria-busy={linkBusy === entry.key} aria-label={'CDN link for ' + entry.key} onClick={async () => {
          if (linkBusy) return
          setLinkBusy(entry.key)
          setError(undefined)
          try {
            const response = await client.cdn({...bucket, key: entry.key})
            setLink({key: entry.key, url: response.url})
          } catch (err) { setError(err) }
          finally { setLinkBusy('') }
        }}>{linkBusy === entry.key ? 'Loading…' : 'CDN link'}</button>}</td>
      </tr>)}</tbody>
    </table>
    {query.isPending && <div className="empty" role="status">Loading objects…</div>}
    {query.isSuccess && visible.length === 0 && <div className="empty">
      <strong>{search ? 'No matching keys in the loaded pages' : 'This prefix is empty'}</strong>
      {search ? <button className="plain" onClick={() => setSearch('')}>Clear filter</button> :
        source.capabilities.upload && <button className="plain" onClick={() => fileInput.current?.click()}>Choose files to upload</button>}
    </div>}
    <footer className="table-foot">
      {query.hasNextPage && <span>Filter and sort cover only loaded pages.</span>}
      {query.hasNextPage && <button disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
        {query.isFetchingNextPage ? 'Loading…' : 'Load more'}
      </button>}
    </footer>
    {object && <ObjectDetail object={object} client={client} capabilities={source.capabilities} changed={refresh} close={() => setObject(undefined)} />}
    {link && <Modal title="Permanent CDN link" close={() => setLink(undefined)}>
      <p className="object-key key">{link.key}</p>
      <CopyField url={link.url} label="Permanent CDN link" />
      <a className="external" href={link.url} target="_blank" rel="noopener noreferrer">Open public URL ↗</a>
    </Modal>}
    {policy && <Policy {...{bucket, client}} close={() => setPolicy(false)} />}
  </>
}

function Policy({bucket, client, close}: {bucket: Bucket; client: Client; close: () => void}) {
  const query = useQuery({queryKey: ['policy', bucket], queryFn: () => client.policy(bucket)})
  const [draft, setDraft] = useState<string>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  useEffect(() => { setDraft(undefined) }, [bucket])
  return <Modal title="Bucket policy" close={close}>
    {query.isPending && <p role="status">Loading bucket policy…</p>}
    {query.error && <Failure error={query.error} retry={() => void query.refetch()} />}
    {query.data && <form onSubmit={async event => {
      event.preventDefault()
      if (draft === undefined || busy) return
      setError(undefined)
      setBusy(true)
      try { JSON.parse(draft); await client.savePolicy(bucket, draft); await query.refetch(); setDraft(undefined) }
      catch (err) { setError(err) }
      finally { setBusy(false) }
    }}>
      <label className="editor-label" htmlFor="policy">Policy JSON</label>
      <textarea id="policy" value={draft ?? query.data.policy} readOnly={busy} spellCheck={false} onChange={event => setDraft(event.target.value)} />
      <button className="primary" disabled={busy || draft === undefined}>Save policy</button>
    </form>}
    {error !== undefined && <Failure error={error} />}
  </Modal>
}
