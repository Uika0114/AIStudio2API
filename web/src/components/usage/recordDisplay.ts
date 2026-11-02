import type { UsageRecord } from '@/types'

type ModelRecord = Pick<UsageRecord, 'model' | 'requested_model' | 'base_model' | 'anti_truncation'>

// Parse only known public selector namespaces, never an arbitrary occurrence of
// "anti" in an upstream model name. This changes presentation, not stored IDs.
function selector(value: string | undefined | null): { base: string; anti: boolean } | undefined {
  if (typeof value !== 'string') return undefined
  const id = value.startsWith('models/') ? value.slice(7) : value
  const match = /^(?:(?:playground|build|assistant)\/)(anti-truncation\/)?(.+)$/.exec(id)
  if (match) return { base: match[2]!, anti: match[1] !== undefined }
  return undefined
}

export function recordModelName(record: ModelRecord): string {
  const original = record.model || record.base_model || record.requested_model || '—'
  const requested = selector(record.requested_model)
  const stored = selector(record.model)
  const explicit = typeof record.anti_truncation === 'boolean' ? record.anti_truncation : undefined
  const selected = requested ?? stored
  if (explicit !== undefined && selected !== undefined && explicit !== selected.anti)
    return original
  const anti = explicit ?? selected?.anti
  if (anti === undefined) return original // legacy absence is not an inferred mode
  const rawBase = record.base_model || stored?.base || record.model || requested?.base
  const base = selector(rawBase)?.base || rawBase
  if (!base) return original
  return anti && !base.endsWith('/anti-truncation') ? base + '/anti-truncation' : base
}

// The server owns validity and calculation. Formatting must neither recompute
// TPS from total latency nor turn an absent/invalid value into a zero suffix.
export function tokenTotalWithTPS(total: string, value: unknown, locale: string): string {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return total
  const options: Intl.NumberFormatOptions =
    value < 0.1
      ? {
          maximumSignificantDigits: 2,
          notation: value < 0.001 ? 'scientific' : 'standard',
          useGrouping: false,
        }
      : { maximumFractionDigits: 1, useGrouping: false }
  return total + '/' + new Intl.NumberFormat(locale, options).format(value)
}
