import type { Model, QuotaEstimateReport } from '@/types'

export const fixedQuotaModels = ['gemini-4-argon', 'gemini-4-carbon', 'gemini-4-hydrogen'] as const

export function additionalGemini4QuotaModels(models: readonly Model[]): QuotaEstimateReport['model'][] {
  const ids = new Set<QuotaEstimateReport['model']>()
  for (const model of models) {
    if (!model.channels?.includes('playground') ||
        !model.methods.some((method) => method.toLowerCase() === 'generatecontent')) continue
    const id = (model.base_model || model.id).replace(/^models\//, '')
    if (id.length <= 96 && /^gemini-4(?:-[a-z0-9]+){1,8}$/.test(id) && !id.endsWith('-anti') &&
        !fixedQuotaModels.some((fixed) => fixed === id)) ids.add(id as QuotaEstimateReport['model'])
  }
  return [...ids].sort().slice(0, 64)
}

export function gemini4QuotaLabel(model: QuotaEstimateReport['model']): string {
  return model.slice('gemini-4-'.length).replace(/(^|-)([a-z])/g, (_, prefix: string, letter: string) => prefix + letter.toUpperCase())
}
