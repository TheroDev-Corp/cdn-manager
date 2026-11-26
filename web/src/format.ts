export function formatBytes(bytes: number) {
  if (bytes < 1024) return bytes + ' B'
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  const index = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)) - 1)
  return new Intl.NumberFormat(undefined, {maximumFractionDigits: 1}).format(bytes / (1024 ** (index + 1))) + ' ' + units[index]
}

export function formatDate(date?: string) {
  if (!date) return '—'
  return new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'}).format(new Date(date))
}

export function isText(type: string) {
  return /^(text\/|application\/(?:json|[\w.-]+\+json|xml|javascript)(?:;|$))/i.test(type)
}
