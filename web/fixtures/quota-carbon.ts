import { createApp, h, reactive, type VNode } from 'vue'
import type { QuotaEstimateReport } from '../src/types'
import QuotaPoolBars from '../src/components/usage/QuotaPoolBars.vue'
import '../src/style.css'

// Synthetic members only. This fixture must be served without the production Vite API proxy.
const balance = (limit: number, remaining: number) => ({
  limit,
  remaining,
  used: limit - remaining,
})
function report(model: QuotaEstimateReport['model']): QuotaEstimateReport {
  const isCarbon = model === 'gemini-4-carbon'
  return {
    channel: 'playground',
    model,
    period_from: '2026-11-10T08:00:00Z',
    reset_at: '2026-11-11T08:00:00Z',
    updated_at: '2026-11-11T02:00:00Z',
    tokens: balance(180000000, isCarbon ? 150000000 : 72373000),
    requests: balance(1200, isCarbon ? 960 : 385),
    accounts: Array.from({ length: 6 }, (_, index) => ({
      id: `synthetic-${index}`,
      label: `合成账号 ${String.fromCharCode(65 + index)}`,
      tokens: balance(30000000, isCarbon ? 25000000 : 12062166 + (index < 4 ? 1 : 0)),
      requests: balance(200, isCarbon ? 160 : index === 0 ? 65 : 64),
    })),
  }
}

const props = reactive({
  report: report('gemini-4-argon'),
  error: '',
  carbonReport: report('gemini-4-carbon'),
  carbonError: '',
})
function setScenario(scenario: string): void {
  props.report = report('gemini-4-argon')
  props.carbonReport = report('gemini-4-carbon')
  props.carbonError = ''
  if (scenario === 'no-ultra') {
    props.report.tokens = balance(45000000, 30000000)
    props.report.requests = balance(150, 100)
    props.report.accounts = Array.from({ length: 3 }, (_, index) => ({
      id: `synthetic-pro-${index}`,
      label: `合成 Pro ${index + 1}`,
      tokens: balance(15000000, 10000000),
      requests: balance(50, index === 0 ? 34 : 33),
    }))
    props.carbonReport.accounts = []
  } else if (scenario === 'zero') {
    props.carbonReport.tokens = balance(180000000, 0)
    props.carbonReport.requests = balance(1200, 0)
    for (const account of props.carbonReport.accounts ?? []) {
      account.tokens = balance(30000000, 0)
      account.requests = balance(200, 0)
    }
  } else if (scenario === 'unknown') {
    props.carbonReport.tokens.remaining = null
    props.carbonReport.requests.remaining = null
  } else if (scenario === 'error') {
    props.carbonError = '读取失败'
  }
}

const query = new URLSearchParams(window.location.search)
setScenario(query.get('scenario') ?? 'normal')
Object.assign(window, { __quotaFixture: { setScenario } })
let renderView: () => VNode
if (query.get('view') === 'panel') {
  const { default: UsagePanel } = await import('../src/components/UsagePanel.vue')
  renderView = () => h(UsagePanel, { models: [] })
} else {
  renderView = () => h(QuotaPoolBars, props)
}
createApp({
  render: () => h('main', { class: 'p-4', 'data-quota-fixture': '' }, renderView()),
}).mount('#app')
