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
    included: true,
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

test('absent model pools hide Carbon and Hydrogen without hiding Argon', async () => {
  for (const empty of [true, false]) {
    const html = await render({
      carbonReport: empty ? null : report('gemini-4-carbon', []),
      hydrogenReport: empty ? null : report('gemini-4-hydrogen', []),
    })
    assert.equal([...html.matchAll(/data-quota="/g)].length, 2)
    assert.doesNotMatch(html, /data-quota-model="(?:Carbon|Hydrogen)"/)
  }
})

test('a Pro-only Hydrogen pool remains visible while Carbon is absent', async () => {
  const value = report('gemini-4-hydrogen', ['fixture-pro'])
  value.tokens = balance(15000000, 1000000)
  value.requests = balance(50, 10)
  Object.assign(value.accounts[0], { tier: 'Pro', tokens: value.tokens, requests: value.requests })
  const html = await render({
    carbonReport: report('gemini-4-carbon', []),
    hydrogenReport: value,
  })
  assert.equal([...html.matchAll(/data-quota="/g)].length, 4)
  assert.doesNotMatch(html, /data-quota-model="Carbon"/)
  assert.match(text(poolHTML(html, 'Hydrogen')), /HydrogenToken1400万.*Hydrogen请求数40/)
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

for (const label of models) {
  for (const metric of ['tokens', 'requests']) {
    test(`${label} ${metric} preserves a partial subtotal without a redundant known prefix`, async () => {
      const value = report(`gemini-4-${label.toLowerCase()}`, ['known', 'unknown'])
      value[metric] = { limit: 1000, used: 100, remaining: null }
      value.accounts[0][metric] = { limit: 500, used: 100, remaining: 400 }
      value.accounts[1][metric] = { limit: 500, used: 0, remaining: null }
      const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
      const html = poolHTML(await render({ [key]: value }), label)
      assert.match(text(html), /400/)
      assert.doesNotMatch(text(html), /已知/)
      assert.match(text(html), /1\s*号未知/)
      assert.match(html, /data-quota-partial="true"/)
      assert.match(html, /aria-valuetext="[^"]*剩余[^"]*400[^"]*1[^"]*号未知"/)
      assert.doesNotMatch(html, /40%/)
      assert.match(html, /translateX\(-60%\)/)
      assert.equal(value[metric].remaining, null)
      assert.equal(value.accounts[1][metric].remaining, null)
    })
  }
}

test('a zero known subtotal with another unknown member is not a zero whole pool', async () => {
  const value = report('gemini-4-argon', ['known', 'unknown'])
  value.tokens = { limit: 1000, used: 500, remaining: null }
  value.accounts[0].tokens = { limit: 500, used: 500, remaining: 0 }
  value.accounts[1].tokens = { limit: 500, used: 0, remaining: null }
  const html = poolHTML(await render({ report: value }), 'Argon')
  const tokenText = text(html).split('Argon请求数')[0]
  assert.match(tokenText, /ArgonToken0/)
  assert.doesNotMatch(tokenText, /已知/)
  assert.match(tokenText, /1\s*号未知/)
  assert.doesNotMatch(tokenText, /0%/)
})

test('excluded and missing-included members are never inferred into a partial pool', async () => {
  for (const included of [false, undefined]) {
    const value = report('gemini-4-argon', ['known', 'unknown', 'not-in-pool'])
    value.tokens = { limit: 1000, used: 100, remaining: null }
    value.accounts[0].tokens = { limit: 500, used: 100, remaining: 400 }
    value.accounts[1].tokens = { limit: 500, used: 0, remaining: null }
    value.accounts[2].tokens = { limit: 500, used: 0, remaining: 500 }
    value.accounts[2].included = included
    const html = poolHTML(await render({ report: value }), 'Argon')
    assert.match(text(html), /ArgonToken400/)
    assert.doesNotMatch(text(html), /ArgonToken900|已知/)
  }
})

test('partial numbers are suppressed on errors or inconsistent member capacity', async () => {
  for (const mode of ['error', 'capacity', 'missing-included']) {
    const value = report('gemini-4-argon', ['known', 'unknown'])
    value.tokens = { limit: 1000, used: 100, remaining: null }
    value.accounts[0].tokens = { limit: 500, used: 100, remaining: 400 }
    value.accounts[1].tokens = { limit: 500, used: 0, remaining: null }
    if (mode === 'capacity') value.tokens.limit = 2000
    if (mode === 'missing-included') delete value.accounts[0].included
    const html = poolHTML(await render({ report: value, error: mode === 'error' ? '读取失败' : '' }), 'Argon')
    assert.doesNotMatch(html, /data-quota-partial="true"/)
    assert.doesNotMatch(text(html), /已知/)
    assert.match(html, /quota-unknown/)
  }
})

for (const label of models) {
  for (const metric of ['tokens', 'requests']) {
    test(`${label} ${metric} counts rate-limited unknown balances as zero only in the display`, async () => {
      const value = report(`gemini-4-${label.toLowerCase()}`, ['available', 'limited'])
      value[metric] = { limit: 1000, used: 100, remaining: null }
      value.accounts[0][metric] = { limit: 500, used: 100, remaining: 400 }
      value.accounts[1][metric] = { limit: 500, used: 0, remaining: null }
      value.accounts[1].exhausted_429 = true
      const original = globalThis.structuredClone(value)
      Object.freeze(value[metric])
      Object.freeze(value.accounts[1][metric])
      const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
      const full = await render({ [key]: value })
      const html = poolHTML(full, label)
      assert.match(text(html), /400/)
      assert.match(text(html), /40%/)
      assert.doesNotMatch(text(html), /已知|号未知/)
      assert.doesNotMatch(html, /quota-unknown|data-quota-partial="true"/)
      assert.match(html, /data-quota-assumed-zero="1"/)
      assert.match(html, /aria-valuetext="[^"]*限流，余额未知，暂按0计/)
      assert.match(html, /translateX\(-60%\)/)
      const otherMetric = metric === 'tokens' ? '请求数180' : 'Token2900万'
      assert.match(text(html), new RegExp(`${label}${otherMetric}`))
      assert.deepEqual(value, original)
      for (const other of models.filter((model) => model !== label)) {
        assert.doesNotMatch(poolHTML(full, other), /data-quota-assumed-zero=/)
      }
    })
  }
}

test('a whole rate-limited unknown pool displays zero rather than grey or a reduced capacity', async () => {
  for (const label of models) {
    const value = report(`gemini-4-${label.toLowerCase()}`)
    for (const metric of ['tokens', 'requests']) {
      value[metric].remaining = null
      value.accounts[0][metric].remaining = null
    }
    value.accounts[0].exhausted_429 = true
    const original = globalThis.structuredClone(value)
    const key = label === 'Argon' ? 'report' : `${label.toLowerCase()}Report`
    const html = poolHTML(await render({ [key]: value }), label)
    assert.match(text(html), new RegExp(`${label}Token0.*3000万.*0%.*${label}请求数0.*200.*0%`))
    assert.doesNotMatch(html, /quota-unknown/)
    assert.equal([...html.matchAll(/data-quota-assumed-zero="1"/g)].length, 2)
    assert.deepEqual(value, original)
  }
})

test('a non-limited unknown member remains unknown alongside a limited zero estimate', async () => {
  const value = report('gemini-4-hydrogen', ['available', 'limited', 'unobserved'])
  value.tokens = { limit: 1500, used: 100, remaining: null }
  value.accounts[0].tokens = { limit: 500, used: 100, remaining: 400 }
  value.accounts[1].tokens = { limit: 500, used: 0, remaining: null }
  value.accounts[1].exhausted_429 = true
  value.accounts[2].tokens = { limit: 500, used: 0, remaining: null }
  const html = poolHTML(await render({ hydrogenReport: value }), 'Hydrogen')
  assert.match(text(html), /400/)
  assert.match(text(html), /1\s*号未知/)
  assert.doesNotMatch(text(html), /已知|2\s*号未知/)
  assert.match(html, /data-quota-assumed-zero="1"/)
  assert.match(html, /data-quota-partial="true"/)
  assert.equal(value.tokens.remaining, null)
})

test('limited unknown estimates do not bypass failed reads or inconsistent membership', async () => {
  for (const mode of ['error', 'capacity', 'excluded', 'missing-included']) {
    const value = report('gemini-4-hydrogen')
    value.tokens.remaining = null
    value.accounts[0].tokens.remaining = null
    value.accounts[0].exhausted_429 = true
    if (mode === 'capacity') value.tokens.limit *= 2
    if (mode === 'excluded') value.accounts[0].included = false
    if (mode === 'missing-included') delete value.accounts[0].included
    const html = poolHTML(
      await render({ hydrogenReport: value, hydrogenError: mode === 'error' ? '读取失败' : '' }),
      'Hydrogen',
    )
    assert.match(html, /quota-unknown/)
    assert.doesNotMatch(html, /data-quota-assumed-zero=/)
  }
})

test('verification-only Carbon report keeps the pool visible without advertising remaining supply', async () => {
  const value = report('gemini-4-carbon')
  value.accounts[0].included = false
  value.accounts[0].block_code = 'ACCOUNT_VERIFICATION_REQUIRED'
  value.accounts[0].exhausted_429 = true
  value.tokens = value.requests = { limit: null, used: null, remaining: null }
  const html = poolHTML(await render({ carbonReport: value }), 'Carbon')
  assert.match(html, /quota-unknown/)
  assert.doesNotMatch(html, /data-quota-assumed-zero=|2900万/)
})
