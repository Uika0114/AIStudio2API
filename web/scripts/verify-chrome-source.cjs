'use strict'
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const assert = require('node:assert/strict')
const ts = require('typescript')
const { parse, compileScript } = require('@vue/compiler-sfc')
const vue = require('vue')
const root = path.resolve(__dirname, '..')
const checks = []
const plain = (value) => JSON.parse(JSON.stringify(value))

function loadSource(relative, modules, extra = {}) {
  const filename = path.join(root, relative)
  let source = fs.readFileSync(filename, 'utf8')
  if (relative.endsWith('.vue')) {
    const parsed = parse(source, { filename })
    assert.equal(parsed.errors.length, 0)
    source = compileScript(parsed.descriptor, { id: 'chrome-source-offline-fixture' }).content
  }
  const output = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      esModuleInterop: true,
    },
  })
  const module = { exports: {} }
  const sandbox = {
    module,
    exports: module.exports,
    require: (name) => {
      if (Object.hasOwn(modules, name)) return modules[name]
      throw new Error('unapproved fixture import: ' + name)
    },
    console,
    Intl,
    URL,
    URLSearchParams,
    Headers,
    Response,
    Request,
    AbortController,
    TextEncoder,
    TextDecoder,
    TransformStream,
    ReadableStream,
    setTimeout,
    clearTimeout,
    navigator: { language: 'en-US', userAgent: 'offline-fixture' },
    window: {
      location: {
        origin: 'http://fixture.invalid',
        hostname: 'fixture.invalid',
        protocol: 'http:',
      },
      isSecureContext: false,
      setTimeout,
      dispatchEvent() {},
    },
    fetch() {
      throw new Error('real network is forbidden in Chrome UI fixtures')
    },
    ...extra,
  }
  new vm.Script(output.outputText, { filename }).runInNewContext(sandbox)
  return module.exports
}

function profile(root, id, existing = '') {
  return {
    id,
    chrome_root: root,
    profile: 'Default',
    display_name: 'fixture',
    email: 'fixture@example.com',
    locale: 'en-US',
    importable: true,
    token_format: 'v10',
    existing_account_id: existing,
  }
}
function harness(accounts = []) {
  const reads = [],
    posts = [],
    events = []
  const fakeApi = {
    chromeImportProfiles: (root) =>
      new Promise((resolve, reject) => reads.push({ root, resolve, reject })),
    importChromeAccounts: async (input) => {
      posts.push(plain(input))
      return { accounts: [{ id: 'fixture@example.com' }] }
    },
  }
  const scope = vue.effectScope()
  const exported = loadSource('src/components/AccountsPanel.vue', {
    vue,
    '@/api': { api: fakeApi },
    '@/i18n': {
      useI18n: () => ({
        t: (key) => key,
        locale: vue.ref('en-US'),
        errorText: (err) => String(err?.message ?? err),
      }),
    },
    '@/proxy': { maskedProxy: (value) => value },
    './UiIcon.vue': {},
  })
  const state = scope.run(() =>
    exported.default.setup(vue.reactive({ accounts, loading: false, error: '' }), {
      expose() {},
      emit: (...args) => events.push(plain(args)),
    }),
  )
  return { state, reads, posts, events, fakeApi, stop: () => scope.stop() }
}
async function discover(h, root, profiles) {
  h.state.chromeRootInput.value = root
  await vue.nextTick()
  const pending = h.state.discoverChromeProfiles()
  h.reads.at(-1).resolve({ chrome_root: root, profiles })
  await pending
}

async function run() {
  if (process.argv.includes('--format')) {
    const prettier = require('prettier')
    const files = [
      'src/api.ts',
      'src/types.ts',
      'src/components/AccountsPanel.vue',
      ...['zh-CN', 'zh-TW', 'en', 'ja', 'ko', 'fr', 'de'].map(
        (language) => 'src/locales/' + language + '.ts',
      ),
      'scripts/verify-chrome-source.cjs',
    ]
    for (const relative of files) {
      const file = path.join(root, relative)
      const source = fs.readFileSync(file, 'utf8')
      const options = await prettier.resolveConfig(file, { editorconfig: false })
      const formatted = await prettier.format(source, { ...options, filepath: file })
      if (source !== formatted) fs.writeFileSync(file, formatted)
    }
  }

  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    assert.equal(h.reads.length, 0)
    await h.state.discoverChromeProfiles()
    assert.equal(h.reads.length, 0)
    assert.equal(h.state.chromeCanImport.value, false)
    h.stop()
    checks.push('open/empty-custom never discovers default Chrome')
  }
  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    h.state.chromeRootInput.value = 'C:/FixtureA'
    await vue.nextTick()
    const a = h.state.discoverChromeProfiles()
    h.state.chromeRootInput.value = 'C:/FixtureB'
    await vue.nextTick()
    const b = h.state.discoverChromeProfiles()
    const rowA = profile('C:/FixtureA', 'opaque-a'),
      rowB = profile('C:/FixtureB', 'opaque-b')
    h.reads[0].resolve({ chrome_root: 'C:/FixtureA', profiles: [rowA] })
    await a
    assert.equal(h.state.chromeResolvedRoot.value, '')
    h.reads[1].resolve({ chrome_root: 'C:/FixtureB', profiles: [rowB] })
    await b
    h.state.toggleChromeProfile(rowA)
    assert.deepEqual(plain(h.state.selectedChromeProfiles.value), [])
    h.state.toggleChromeProfile(rowB)
    assert.equal(h.state.chromeCanImport.value, true)
    await h.state.importChromeAccounts()
    assert.equal(h.posts.length, 1)
    assert.equal(h.posts[0].chrome_root, 'C:/FixtureB')
    assert.deepEqual(h.posts[0].account_ids, ['opaque-b'])
    h.stop()
    checks.push('late discovery and stale IDs cannot cross roots')
  }
  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    h.state.chromeRootMode.value = 'default'
    await vue.nextTick()
    const pending = h.state.discoverChromeProfiles()
    assert.equal(h.reads[0].root, '')
    h.reads[0].resolve({
      chrome_root: 'C:/FixtureDefault',
      profiles: [profile('C:/FixtureDefault', 'default-id')],
    })
    await pending
    assert.equal(h.state.chromeResolvedRoot.value, 'C:/FixtureDefault')
    h.stop()
    checks.push('default root requires explicit mode and read action')
  }
  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    h.state.chromeRootInput.value = 'C:/FixtureA'
    await vue.nextTick()
    const pending = h.state.discoverChromeProfiles()
    h.state.closeChromeImport()
    h.reads[0].resolve({ chrome_root: 'C:/FixtureA', profiles: [profile('C:/FixtureA', 'late')] })
    await pending
    assert.equal(h.state.showChromeImport.value, false)
    assert.equal(h.state.chromeProfiles.value.length, 0)
    assert.equal(h.posts.length, 0)
    h.stop()
    checks.push('closed dialog rejects late discovery')
  }
  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    h.state.chromeRootInput.value = 'C:/FixtureA'
    await vue.nextTick()
    const pending = h.state.discoverChromeProfiles()
    h.reads[0].resolve({ profiles: [profile('C:/FixtureA', 'legacy')] })
    await pending
    assert.equal(h.state.chromeResolvedRoot.value, '')
    assert.equal(h.state.chromeCanImport.value, false)
    h.stop()
    checks.push('missing source contract stops without fallback')
  }
  {
    const h = harness()
    h.state.openChromeImport()
    await vue.nextTick()
    const good = profile('C:/FixtureB', 'new'),
      existing = profile('C:/FixtureB', 'existing', 'fixture@example.com'),
      bad = { ...profile('C:/FixtureB', 'unsupported'), importable: false }
    await discover(h, 'C:/FixtureB', [good, existing, bad])
    h.state.toggleAllChromeProfiles()
    assert.deepEqual(plain(h.state.selectedChromeProfiles.value), ['new'])
    h.fakeApi.importChromeAccounts = async (input) => {
      h.posts.push(plain(input))
      throw new Error('fixture partial batch')
    }
    await h.state.importChromeAccounts()
    assert.equal(h.state.chromeCanImport.value, false)
    assert.equal(h.state.selectedChromeProfiles.value.length, 0)
    assert.ok(h.events.some(([event]) => event === 'refresh'))
    await h.state.importChromeAccounts()
    assert.equal(h.posts.length, 1)
    h.stop()
    checks.push('eligible-only selection; partial error clears receipt and never replays')
  }
  {
    const account = {
      id: 'fixture@example.com',
      label: 'fixture@example.com',
      enabled: true,
      state: 'ready',
    }
    const h = harness([account])
    h.state.openChromeImport(account)
    await vue.nextTick()
    const row = profile('C:/FixtureB', 'rebind', account.id)
    await discover(h, 'C:/FixtureB', [row, profile('C:/FixtureB', 'unrelated')])
    assert.equal(h.state.chromeVisibleProfiles.value.length, 1)
    h.state.toggleChromeProfile(row)
    assert.equal(h.state.chromeCanImport.value, false)
    h.state.chromeRebindConfirmed.value = true
    assert.equal(h.state.chromeCanImport.value, true)
    await h.state.importChromeAccounts()
    assert.deepEqual(h.posts[0], {
      account_ids: ['rebind'],
      chrome_root: 'C:/FixtureB',
      rebind_account_id: account.id,
      confirm_rebind: true,
    })
    h.stop()
    checks.push('single-target confirmed rebind omits environment changes')
  }
  {
    const requests = []
    const exported = loadSource(
      'src/api.ts',
      { 'eventsource-parser/stream': { EventSourceParserStream: class {} } },
      {
        fetch: async (url, options) => {
          requests.push({ url, options })
          return new Response(
            JSON.stringify({ chrome_root: 'C:/FixtureB', profiles: [], accounts: [] }),
            { headers: { 'content-type': 'application/json' } },
          )
        },
      },
    )
    const rootValue = 'C:/Fixture Root #2/专用'
    await exported.api.chromeImportProfiles(rootValue)
    const uri = new URL(requests[0].url, 'http://fixture.invalid')
    assert.equal(uri.pathname, '/api/accounts/import/chrome/source')
    assert.equal(uri.searchParams.get('chrome_root'), rootValue)
    await exported.api.importChromeAccounts({ chrome_root: rootValue, account_ids: ['opaque'] })
    assert.equal(requests[1].url, '/api/accounts/import/chrome/source')
    assert.equal(JSON.parse(requests[1].options.body).chrome_root, rootValue)
    checks.push('actual API client uses source-only routes and preserves encoded root')
  }
  console.log(
    JSON.stringify(
      {
        suite: 'chrome-source-ui-script-fixtures',
        checks,
        real_network_requests: 0,
        real_profiles_opened: 0,
      },
      null,
      2,
    ),
  )
}
run().catch((error) => {
  console.error(error)
  process.exitCode = 1
})
