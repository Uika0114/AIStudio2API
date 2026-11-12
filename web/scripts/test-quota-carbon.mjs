import assert from 'node:assert/strict'
import { after, test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'

// Only the app's language preference is required for this isolated SSR fixture.
globalThis.window = { localStorage: { getItem: () => 'zh-CN' } }
globalThis.document = { documentElement: { lang: '' } }
const server = await createServer({
  configFile: false,
  root: fileURLToPath(new URL('../', import.meta.url)),
  plugins: [vue()],
  resolve: { alias: { '@': fileURLToPath(new URL('../src', import.meta.url)) } },
  server: { middlewareMode: true, hmr: false, ws: false },
  optimizeDeps: { noDiscovery: true, include: [] },
  appType: 'custom',
})
after(() => server.close())
const { default: QuotaPoolBars } = await server.ssrLoadModule(
  '/src/components/usage/QuotaPoolBars.vue',
)
const { api } = await server.ssrLoadModule('/src/api.ts')
const balance = (limit, used) => ({ limit, used, remaining: limit - used })
const report = (model, members = ['fixture-ultra']) => ({
  channel: 'playground',
  model,
  tokens: balance(30000000, 1000000),
  requests: balance(200, 20),
  accounts: members.map((id) => ({
    id,
    label: '合成账号',
    tokens: balance(30000000, 1000000),
    requests: balance(200, 20),
  })),
})
const argon = report('gemini-4-argon')
const carbon = report('gemini-4-carbon')
const render = (carbonReport, carbonError = '') =>
  renderToString(
    createSSRApp(QuotaPoolBars, { report: argon, error: '', carbonReport, carbonError }),
  )
const text = (html) => html.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ')

test('four real component bars put each model after the icon and before the metric', async () => {
  const html = await render(carbon)
  assert.deepEqual(
    [...html.matchAll(/data-quota-model="([^"]+)"/g)].map((match) => match[1]),
    ['Argon', 'Carbon'],
  )
  assert.equal([...html.matchAll(/data-quota="/g)].length, 4)
  assert.match(text(html), /ArgonToken.*Argon请求数.*CarbonToken.*Carbon请求数/)
  assert.match(html, /<svg[\s\S]*?<\/svg>\s*<span[^>]*>Argon<\/span>\s*<span[^>]*>Token<\/span>/)
  assert.match(html, /aria-label="Carbon Token"/)
})

test('no enabled Ultra pool hides Carbon without hiding the legacy Argon bars', async () => {
  for (const value of [null, report('gemini-4-carbon', [])]) {
    const html = await render(value)
    assert.equal([...html.matchAll(/data-quota="/g)].length, 2)
    assert.doesNotMatch(html, /data-quota-model="Carbon"/)
  }
})

test('a valid Ultra member with exhausted quota still shows both zero Carbon bars', async () => {
  const value = report('gemini-4-carbon')
  value.tokens = balance(30000000, 30000000)
  value.requests = balance(200, 200)
  const html = await render(value)
  assert.match(text(html), /CarbonToken0.*0%.*Carbon请求数0/)
})

test('Carbon read failure keeps its known visibility but never displays stale balances', async () => {
  const html = await render(carbon, '读取失败')
  const carbonHTML = html.slice(html.indexOf('data-quota-model="Carbon"'))
  assert.match(carbonHTML, /quota-unknown/)
  assert.match(text(carbonHTML), /CarbonToken—.*Carbon请求数—/)
  assert.doesNotMatch(carbonHTML, /2900万|180/)
  assert.match(text(html), /ArgonToken2900万/)
})

test('unknown Carbon coverage stays unknown instead of showing a full pool', async () => {
  const value = report('gemini-4-carbon')
  value.tokens.remaining = null
  value.requests.remaining = null
  const html = await render(value)
  assert.match(text(html), /CarbonToken—.*Carbon请求数—/)
})

test('API client preserves the default Argon URL and explicitly scopes Carbon', async () => {
  const previous = globalThis.fetch
  const calls = []
  globalThis.fetch = async (url) => {
    calls.push(url)
    return new globalThis.Response(JSON.stringify(carbon), {
      headers: { 'Content-Type': 'application/json' },
    })
  }
  try {
    await api.quotaEstimate()
    await api.quotaEstimate(undefined, 'gemini-4-carbon')
    assert.deepEqual(calls, ['/api/usage/quota', '/api/usage/quota?model=gemini-4-carbon'])
  } finally {
    globalThis.fetch = previous
  }
})
