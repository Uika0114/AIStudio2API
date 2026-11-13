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
const hydrogen = report('gemini-4-hydrogen')
hydrogen.tokens = balance(30000000, 6000000)
hydrogen.requests = balance(200, 50)
const render = (overrides = {}) =>
  renderToString(
    createSSRApp(QuotaPoolBars, {
      report: argon,
      error: '',
      carbonReport: carbon,
      carbonError: '',
      hydrogenReport: hydrogen,
      hydrogenError: '',
      ...overrides,
    }),
  )
const text = (html) => html.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ')
const poolHTML = (html, label) =>
  html.slice(html.indexOf(`data-quota-model="${label}"`)).split('</section>')[0]
const models = ['Argon', 'Carbon', 'Hydrogen']

test('six real component bars put each model after the icon and before the metric', async () => {
  const html = await render()
  assert.deepEqual(
    [...html.matchAll(/data-quota-model="([^"]+)"/g)].map((match) => match[1]),
    models,
  )
  assert.equal([...html.matchAll(/data-quota="/g)].length, 6)
  assert.match(
    text(html),
    /ArgonToken.*Argon请求数.*CarbonToken.*Carbon请求数.*HydrogenToken.*Hydrogen请求数/,
  )
  for (const label of models) {
    assert.match(
      poolHTML(html, label),
      new RegExp(`<svg[\\s\\S]*?</svg>\\s*<span[^>]*>${label}</span>\\s*<span[^>]*>Token</span>`),
    )
    assert.match(html, new RegExp(`aria-label="${label} Token"`))
  }
  assert.match(text(poolHTML(html, 'Carbon')), /CarbonToken2900万.*Carbon请求数180/)
  assert.match(text(poolHTML(html, 'Hydrogen')), /HydrogenToken2400万.*Hydrogen请求数150/)
})

test('no enabled Ultra pool hides Carbon and Hydrogen without hiding Argon', async () => {
  for (const empty of [true, false]) {
    const html = await render({
      carbonReport: empty ? null : report('gemini-4-carbon', []),
      hydrogenReport: empty ? null : report('gemini-4-hydrogen', []),
    })
    assert.equal([...html.matchAll(/data-quota="/g)].length, 2)
    assert.doesNotMatch(html, /data-quota-model="(?:Carbon|Hydrogen)"/)
  }
})

for (const [label, key] of [
  ['Carbon', 'carbon'],
  ['Hydrogen', 'hydrogen'],
]) {
  test(`an absent ${label} pool does not hide the other model's bars`, async () => {
    const html = await render({ [`${key}Report`]: report(`gemini-4-${key}`, []) })
    assert.equal([...html.matchAll(/data-quota="/g)].length, 4)
    assert.doesNotMatch(html, new RegExp(`data-quota-model="${label}"`))
  })

  test(`a valid Ultra member with exhausted quota still shows both zero ${label} bars`, async () => {
    const value = report(`gemini-4-${key}`)
    value.tokens = balance(30000000, 30000000)
    value.requests = balance(200, 200)
    const html = await render({ [`${key}Report`]: value })
    assert.equal([...html.matchAll(/data-quota="/g)].length, 6)
    assert.match(text(poolHTML(html, label)), new RegExp(`${label}Token0.*0%.*${label}请求数0.*0%`))
  })

  test(`${label} read failure keeps visibility without stale balances or affecting other pools`, async () => {
    const html = await render({ [`${key}Error`]: '读取失败' })
    const failed = poolHTML(html, label)
    assert.match(failed, /quota-unknown/)
    assert.match(text(failed), new RegExp(`${label}Token—.*${label}请求数—`))
    assert.doesNotMatch(text(failed), /2900万|2400万|180|150/)
    assert.match(text(poolHTML(html, 'Argon')), /ArgonToken2900万.*Argon请求数180/)
    if (label === 'Carbon') {
      assert.match(text(poolHTML(html, 'Hydrogen')), /HydrogenToken2400万.*Hydrogen请求数150/)
    } else {
      assert.match(text(poolHTML(html, 'Carbon')), /CarbonToken2900万.*Carbon请求数180/)
    }
  })

  test(`unknown ${label} coverage stays unknown instead of showing a full pool`, async () => {
    const value = report(`gemini-4-${key}`)
    value.tokens.remaining = null
    value.requests.remaining = null
    const html = await render({ [`${key}Report`]: value })
    const unknown = poolHTML(html, label)
    assert.match(unknown, /quota-unknown/)
    assert.match(text(unknown), new RegExp(`${label}Token—.*${label}请求数—`))
    assert.doesNotMatch(text(unknown), /100%/)
  })
}

test('daily token exhaustion preserves positive request balances for every model', async () => {
  for (const label of models) {
    const value = report(`gemini-4-${label.toLowerCase()}`)
    value.tokens.remaining = 0
    value.accounts[0].tokens.remaining = 0
    value.accounts[0].exhausted_429 = true
    const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
    const html = poolHTML(await render({ [key]: value }), label)
    assert.match(text(html), new RegExp(`${label}Token0.*0%.*${label}请求数180.*90%`))
    assert.doesNotMatch(html, /quota-unknown/)
  }
})

test('daily request exhaustion preserves positive token balances for every model', async () => {
  for (const label of models) {
    const value = report(`gemini-4-${label.toLowerCase()}`)
    value.requests.remaining = 0
    value.accounts[0].requests.remaining = 0
    value.accounts[0].exhausted_429 = true
    const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
    const html = poolHTML(await render({ [key]: value }), label)
    assert.match(text(html), new RegExp(`${label}Token2900万.*${label}请求数0.*0%`))
    assert.doesNotMatch(html, /quota-unknown/)
  }
})

test('one unknown dimension leaves the other dimension zero or positive, never unknown', async () => {
  for (const label of models) {
    for (const unknown of ['tokens', 'requests']) {
      for (const zero of [true, false]) {
        const value = report(`gemini-4-${label.toLowerCase()}`)
        const known = unknown === 'tokens' ? 'requests' : 'tokens'
        value[unknown].remaining = null
        value.accounts[0][unknown].remaining = null
        value.accounts[0].exhausted_429 = true
        if (zero) {
          value[known].remaining = 0
          value.accounts[0][known].remaining = 0
        }
        const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
        const html = poolHTML(await render({ [key]: value }), label)
        const unknownLabel = unknown === 'tokens' ? 'Token' : '请求数'
        const knownLabel = known === 'tokens' ? 'Token' : '请求数'
        const remaining = zero ? '0' : known === 'tokens' ? '2900万' : '180'
        assert.match(text(html), new RegExp(`${label}${unknownLabel}—`))
        assert.match(text(html), new RegExp(`${label}${knownLabel}${remaining}`))
        assert.equal([...html.matchAll(/quota-unknown/g)].length, 1)
      }
    }
  }
})

test('API client preserves the default Argon URL and explicitly scopes Carbon and Hydrogen', async () => {
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
    await api.quotaEstimate(undefined, 'gemini-4-hydrogen')
    assert.deepEqual(calls, [
      '/api/usage/quota',
      '/api/usage/quota?model=gemini-4-carbon',
      '/api/usage/quota?model=gemini-4-hydrogen',
    ])
  } finally {
    globalThis.fetch = previous
  }
})
