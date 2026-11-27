import { useQuery } from '@tanstack/react-query'
import { useId, useState } from 'react'
import { api } from './api'
import { Failure, Modal } from './detail'
import './mcp-settings.css'

function shellArg(value: string) {
  return "'" + value.replaceAll("'", "'\"'\"'") + "'"
}

function CopyValue({label, value, action, multiline = false}: {
  label: string; value: string; action: string; multiline?: boolean
}) {
  const id = useId()
  const [feedback, setFeedback] = useState<'copied' | 'unavailable'>()

  return <div className="mcp-copy">
    <label htmlFor={id}>{label}</label>
    <div className="mcp-copy-control">
      <textarea id={id} className={multiline ? 'key mcp-command' : 'key'} readOnly rows={multiline ? 6 : 2}
        spellCheck={false} value={value} onFocus={event => event.currentTarget.select()} />
      <button type="button" onClick={async () => {
        try {
          await navigator.clipboard.writeText(value)
          setFeedback('copied')
        } catch {
          setFeedback('unavailable')
        }
      }}>{action}</button>
    </div>
    <p className="mcp-copy-feedback" role="status">
      {feedback === 'copied' && 'Copied to clipboard.'}
      {feedback === 'unavailable' && 'Clipboard unavailable. Select and copy the text from the field.'}
    </p>
  </div>
}

export function MCPSettings({close}: {close: () => void}) {
  const query = useQuery({
    queryKey: ['mcp-settings'],
    queryFn: ({signal}) => api().mcpSettings(signal),
    gcTime: 0,
  })
  const settings = query.data
  const inspectorCommand = settings && [
    'npx @modelcontextprotocol/inspector@2.16.0 --cli', shellArg(settings.url),
    '--transport http --protocol-era modern --client-id', shellArg(settings.clientId),
    '--method tools/call --tool-name list_sources',
  ].join(' ')

  return <Modal title="MCP Settings" close={close}>
    <div className="mcp-settings">
      <p>Connect to your storage sources using browser single sign-on.</p>
      {query.isPending && <p role="status" className="muted">Loading MCP connection settings…</p>}
      {query.isError && <Failure error={query.error} retry={() => void query.refetch()} />}
      {settings && inspectorCommand && <>
        <section className="mcp-connection" aria-label="MCP connection">
          <CopyValue label="MCP endpoint" value={settings.url} action="Copy endpoint" />
          <dl>
            <div><dt>Transport</dt><dd>Streamable HTTP</dd></div>
            <div><dt>OAuth client ID</dt><dd><code className="key">{settings.clientId}</code></dd></div>
          </dl>
        </section>
        <section className="mcp-inspector" aria-label="Connect with MCP Inspector">
          <h3>Connect with MCP Inspector</h3>
          <p>Run the command in Bash or zsh with a supported Node.js version, such as 24.22.1. Follow the browser sign-in prompt using your CDN Manager account. Inspector then lists the sources you can access.</p>
          <CopyValue label="Inspector command" value={inspectorCommand} action="Copy command" multiline />
        </section>
        <details className="mcp-discovery">
          <summary>OAuth discovery details</summary>
          <dl>
            <div><dt>Issuer</dt><dd className="key">{settings.issuer}</dd></div>
            <div><dt>Protected resource metadata</dt><dd className="key">{settings.metadataUrl}</dd></div>
            <div><dt>Supported scopes</dt><dd><code className="key">{settings.scopes.join(', ')}</code></dd></div>
          </dl>
        </details>
      </>}
    </div>
  </Modal>
}
