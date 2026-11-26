import { QueryClient, QueryClientProvider, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/jetbrains-mono/latin-400.css'
import '@fontsource/jetbrains-mono/cyrillic-400.css'
import '@fontsource/unbounded/latin-600.css'
import { api, HTTPError, type Session } from './api'
import { Failure, Modal } from './detail'
import { MCPSettings } from './mcp-settings'
import { Objects } from './objects'
import { Transfers, useTransfers } from './transfers'
import './styles.css'

const queryClient = new QueryClient({defaultOptions: {queries: {
  retry: (count, error) => (!(error instanceof HTTPError) || error.status >= 500) && count < 1,
}}})

function Brand() {
  return <div className="brand"><span className="brand-mark" aria-hidden="true"><i /><i /><i /></span><strong>CDN Manager</strong></div>
}

function App() {
  const session = useQuery({queryKey: ['session'], queryFn: () => api().session(), staleTime: 60_000})
  if (session.data) return <Workspace key={session.data.subject} session={session.data} />
  const needsLogin = session.error instanceof HTTPError && session.error.status === 401
  return <main className="entry">
    <Brand />
    <div className="entry-card">
      {session.isPending ? <p role="status">Checking session…</p> : needsLogin ? <>
        <h1>Sign in to CDN Manager</h1>
        <p>Access your storage sources and share public CDN links.</p>
        <a className="button primary" href="/auth/login">Continue with single sign-on</a>
      </> : <Failure error={session.error} retry={() => void session.refetch()} />}
    </div>
  </main>
}

function Workspace({session}: {session: Session}) {
  const client = api(session.csrfToken)
  const cache = useQueryClient()
  const sources = useQuery({queryKey: ['sources'], queryFn: client.sources, refetchInterval: 30_000})
  const [sourceID, selectSource] = useState('')
  const [bucketName, selectBucket] = useState('')
  const [tab, setTab] = useState<'objects' | 'transfers'>('objects')
  const [mcpSettings, setMcpSettings] = useState(false)
  const transferTab = useRef<HTMLButtonElement>(null)
  const [create, setCreate] = useState(false)
  const [newBucket, setNewBucket] = useState('')
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const source = (sources.data?.sources ?? []).find(value => value.id === sourceID) ?? sources.data?.sources?.[0]
  const buckets = useQuery({
    queryKey: ['buckets', source?.id],
    queryFn: () => client.buckets(source!.id),
    enabled: !!source,
  })
  const activeBucket = (buckets.data?.buckets ?? []).find(value => value.name === bucketName) ?? buckets.data?.buckets?.[0]
  const transfers = useTransfers(client, session.subject, object => {
    void cache.invalidateQueries({queryKey: ['objects', {source: object.source, bucket: object.bucket}]})
  })
  const unfinished = transfers.rows.filter(row => row.status !== 'complete').length
  const sourceHealth = source?.health === 'healthy' ? 'Available' : source?.health === 'unavailable' ? 'Unavailable' : ''
  const reloadBuckets = async () => { await cache.invalidateQueries({queryKey: ['buckets', source?.id]}) }
  return <div className="app-frame">
    <a className="skip-link" href="#workspace">Skip to workspace</a>
    <header className="topbar">
      <Brand />
      <nav className="tabs" aria-label="Workspace views">
        <button aria-current={tab === 'objects' ? 'page' : undefined} onClick={() => setTab('objects')}>Objects</button>
        <button ref={transferTab} aria-current={tab === 'transfers' ? 'page' : undefined}
          aria-label={unfinished ? `Transfers, ${unfinished} unfinished` : undefined} onClick={() => setTab('transfers')}>
          Transfers{unfinished > 0 && <span className="tab-count" aria-hidden="true">{unfinished}</span>}
        </button>
      </nav>
      <div className="account">
        <span className="account-name" title={session.name || session.subject}>{session.name || session.subject}</span>
        <button onClick={async () => {
          if (unfinished && !confirm('Sign out? Active transfers will pause and can be resumed when you sign back in.')) return
          try {
            await client.logout()
            window.location.assign('/')
          } catch (err) { setError(err) }
        }}>Sign out</button>
        <button className="mcp-trigger" aria-label="MCP Settings" aria-haspopup="dialog" onClick={() => setMcpSettings(true)}>
          MCP<span className="mcp-label"> Settings</span>
        </button>
      </div>
    </header>
    <aside className="sources" aria-label="Storage scope">
      <div className="scope-group">
        <label htmlFor="source-select">Storage source</label>
        <select id="source-select" value={source?.id ?? ''} disabled={!source} onChange={event => {
          selectSource(event.target.value)
          selectBucket('')
          setError(undefined)
          setTab('objects')
        }}>
          {!source && <option value="">{sources.isPending ? 'Loading sources…' : 'No sources'}</option>}
          {(sources.data?.sources ?? []).map(item => <option value={item.id} key={item.id}>
            {item.label || item.id}{item.health === 'unavailable' ? ' (unavailable)' : ''}
          </option>)}
        </select>
        {source && (source.region || sourceHealth) && <div className="source-meta">
          {source.region && <span className="key" title="Region">{source.region}</span>}
          {sourceHealth && <span className="source-health" data-health={source.health}>{sourceHealth}</span>}
        </div>}
      </div>
      <div className="scope-group">
        <label htmlFor="bucket-select">Bucket</label>
        <select id="bucket-select" value={activeBucket?.name ?? ''} disabled={!activeBucket} onChange={event => {
          selectBucket(event.target.value)
          setError(undefined)
          setTab('objects')
        }}>
          {!activeBucket && <option value="">{source && buckets.isPending ? 'Loading buckets…' : 'No buckets'}</option>}
          {(buckets.data?.buckets ?? []).map(item => <option value={item.name} key={item.name}>{item.name}</option>)}
        </select>
        <div className="scope-actions">
          {source?.capabilities.createBucket && <button className="plain" onClick={() => {setNewBucket(''); setError(undefined); setCreate(true)}}>Create bucket</button>}
          {source?.capabilities.deleteBucket && activeBucket && <button className="plain danger" disabled={busy} onClick={async () => {
            if (!confirm('Delete the empty bucket ' + activeBucket.name + '?')) return
            setBusy(true)
            setError(undefined)
            try { await client.deleteBucket({source: source.id, bucket: activeBucket.name}); await reloadBuckets() }
            catch (err) { setError(err) }
            finally { setBusy(false) }
          }}>Delete empty bucket</button>}
        </div>
      </div>
    </aside>
    <main className="sheet" id="workspace" tabIndex={-1}>
      {!create && error !== undefined && <Failure error={error} />}
      {tab === 'transfers' && <>
        <header className="title"><h1>Transfers</h1></header>
        <Transfers transfers={transfers} />
      </>}
      <section hidden={tab !== 'objects'} aria-label="Objects">
        {(!source || !activeBucket) && <header className="title"><h1>Objects</h1></header>}
        {source?.health === 'unavailable' && <p className="feedback error" role="status">
          {source.label || source.id} is unavailable. You can switch to another source.
        </p>}
        {sources.error && <Failure error={sources.error} retry={() => void sources.refetch()} />}
        {source && buckets.error && <Failure error={buckets.error} retry={() => void buckets.refetch()} />}
        {(sources.isPending || (source && buckets.isPending)) && <div className="empty" role="status">Loading storage…</div>}
        {sources.isSuccess && !source && <div className="empty">No storage sources are available for this account.</div>}
        {source && buckets.isSuccess && !activeBucket && <div className="empty">No buckets are available in {source.label || source.id}.</div>}
        {source && activeBucket && <Objects
          key={JSON.stringify([source.id, activeBucket.name])} source={source} bucket={{source: source.id, bucket: activeBucket.name}}
          client={client} csrfToken={session.csrfToken} enqueue={entries => {
            if (entries.length) {
              transfers.enqueue(entries, !source.capabilities.multipart)
              setTab('transfers')
              transferTab.current?.focus()
            }
          }}
        />}
      </section>
    </main>
    {mcpSettings && <MCPSettings close={() => setMcpSettings(false)} />}
    {create && source && <Modal title="Create bucket" close={() => {setError(undefined); setCreate(false)}}>
      <form className="bucket-form" onSubmit={async event => {
        event.preventDefault()
        setBusy(true)
        setError(undefined)
        try {
          await client.createBucket({source: source.id, bucket: newBucket})
          await reloadBuckets()
          selectBucket(newBucket)
          setCreate(false)
          setTab('objects')
        } catch (err) { setError(err) }
        finally { setBusy(false) }
      }}>
        <label htmlFor="new-bucket">Bucket name</label>
        <input id="new-bucket" value={newBucket} onChange={event => setNewBucket(event.target.value)} required />
        <button className="primary" disabled={busy}>Create bucket</button>
      </form>
      {error !== undefined && <Failure error={error} />}
    </Modal>}
  </div>
}

createRoot(document.getElementById('root')!).render(<QueryClientProvider client={queryClient}><App /></QueryClientProvider>)
