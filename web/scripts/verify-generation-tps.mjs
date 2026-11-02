// Execution15: render the same model/Token components used by desktop and mobile.
import assert from 'node:assert/strict'
import { readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath, URL } from 'node:url'
import path from 'node:path'
import process from 'node:process'
import console from 'node:console'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { createServer } from 'vite'
import vue from '@vitejs/plugin-vue'

const root = fileURLToPath(new URL('..', import.meta.url))
const files = [
  'src/components/usage/recordDisplay.ts',
  'src/components/usage/RecordModelName.vue',
  'src/components/usage/RecordTokenTotal.vue',
  'src/components/usage/RecordsTable.vue',
  'src/types.ts',
  'scripts/verify-generation-tps.mjs',
]
assert(process.argv.length === 2 || (process.argv.length === 3 && process.argv[2] === '--format'))
if (process.argv[2] === '--format') {
  const prettier = await import('prettier')
  for (const name of files) {
    const filename = path.join(root, name)
    const source = await readFile(filename, 'utf8')
    const options = await prettier.resolveConfig(filename)
    await writeFile(filename, await prettier.format(source, { ...options, filepath: filename }))
  }
  console.log(
    JSON.stringify({ formatted: files.length, scope: 'execution15 model short name / inline TPS' }),
  )
} else {
  const values = new Map()
  globalThis.window = {
    localStorage: {
      getItem: (name) => values.get(name) ?? null,
      setItem: (name, value) => values.set(name, value),
    },
  }
  globalThis.document = { documentElement: { lang: '' } }
  let networkCalls = 0
  globalThis.fetch = () => {
    networkCalls++
    throw new Error('external network unavailable in synthetic component test')
  }
  const server = await createServer({
    configFile: false,
    root,
    plugins: [vue()],
    resolve: { alias: { '@': path.join(root, 'src') } },
    server: {
      host: '127.0.0.1',
      middlewareMode: true,
      hmr: false,
      ws: false,
      watch: null,
      proxy: {},
    },
  })
  try {
    const { default: Model } = await server.ssrLoadModule(
      '/src/components/usage/RecordModelName.vue',
    )
    const { default: Tokens } = await server.ssrLoadModule(
      '/src/components/usage/RecordTokenTotal.vue',
    )
    const { recordModelName } = await server.ssrLoadModule('/src/components/usage/recordDisplay.ts')
    const { useI18n } = await server.ssrLoadModule('/src/i18n.ts')
    const { useUsageFormat } = await server.ssrLoadModule('/src/components/usage/format.ts')
    const i18n = useI18n()
    const format = useUsageFormat()
    const models = [
      [
        { model: 'gemini-4-argon', base_model: 'gemini-4-argon', anti_truncation: false },
        'gemini-4-argon',
      ],
      [
        { model: 'gemini-4-argon', base_model: 'gemini-4-argon', anti_truncation: true },
        'gemini-4-argon/anti-truncation',
      ],
      [
        { model: 'gemini-4-flash', requested_model: 'build/anti-truncation/gemini-4-flash' },
        'gemini-4-flash/anti-truncation',
      ],
      [
        { model: 'not-a-model-whitelist', anti_truncation: true },
        'not-a-model-whitelist/anti-truncation',
      ],
      [{ model: 'fixture', requested_model: 'assistant/fixture' }, 'fixture'],
      [{ model: 'build/anti-truncation/fixture' }, 'fixture/anti-truncation'],
      [{ model: 'anti-truncation/fixture' }, 'anti-truncation/fixture'],
      [{ model: '', requested_model: 'anti-truncation/fixture' }, 'anti-truncation/fixture'],
      [{ model: 'old-unclassified-model' }, 'old-unclassified-model'],
      [
        { model: 'mentions-anti-truncation-in-its-real-id' },
        'mentions-anti-truncation-in-its-real-id',
      ],
      [{ model: 'old/path/model', anti_truncation: null }, 'old/path/model'],
      [{ model: 'fixture', anti_truncation: 'true' }, 'fixture'],
      [
        {
          model: 'fixture',
          anti_truncation: false,
          requested_model: 'build/anti-truncation/fixture',
        },
        'fixture',
      ],
      [{ model: 'fixture/anti-truncation', anti_truncation: true }, 'fixture/anti-truncation'],
      [{ model: '' }, '—'],
    ]
    const visibleText = (html) => html.replace(/<!--[\s\S]*?-->/g, '').replace(/<[^>]*>/g, '')
    const render = async (component, record) => {
      const app = createSSRApp(component, { record })
      app.directive('tooltip', { getSSRProps: () => ({}) })
      return renderToString(app)
    }
    let modelAssertions = 0,
      tokenAssertions = 0
    for (const locale of ['zh-CN', 'zh-TW', 'en', 'ja', 'ko', 'fr', 'de']) {
      i18n.setLocale(locale)
      for (const [record, expected] of models) {
        const before = JSON.stringify(record)
        assert.equal(recordModelName(record), expected)
        const html = await render(Model, record)
        assert.equal(visibleText(html), expected, locale + ' / short model name')
        assert(!html.includes('data-anti-truncation'), 'no mode badge')
        assert.equal(JSON.stringify(record), before, 'presentation must not rewrite IDs')
        modelAssertions++
      }
      const total = format.compact(126000)
      for (const value of [undefined, null, 0, -1, false, '70', NaN, Infinity, -Infinity]) {
        const html = await render(Tokens, { total_tokens: 126000, output_tps: value })
        assert.equal(visibleText(html), total, 'invalid/unavailable TPS omits the entire suffix')
        tokenAssertions++
      }
      for (const [value, number] of [
        [70, '70'],
        [125, '125'],
        [65.3, new Intl.NumberFormat(locale, { useGrouping: false }).format(65.3)],
        [0.01, new Intl.NumberFormat(locale, { useGrouping: false }).format(0.01)],
      ]) {
        const html = await render(Tokens, { total_tokens: 126000, output_tps: value })
        assert.equal(visibleText(html), total + '/' + number, locale + ' / numeric TPS')
        assert(
          !/(title=|tooltip|aria-label|data-quality|data-tps)/.test(html),
          'no TPS explanations or extra badge',
        )
        tokenAssertions++
      }
    }
    i18n.setLocale('zh-CN')
    assert.equal(
      visibleText(await render(Tokens, { total_tokens: 126000, output_tps: 70 })),
      '12.6万/70',
    )
    assert.equal(visibleText(await render(Tokens, { total_tokens: 126000 })), '12.6万')
    const table = await readFile(path.join(root, 'src/components/usage/RecordsTable.vue'), 'utf8')
    assert.equal(
      (table.match(/<RecordModelName /g) ?? []).length,
      2,
      'both desktop and mobile use the actual model component',
    )
    assert.equal(
      (table.match(/<RecordTokenTotal /g) ?? []).length,
      2,
      'both layouts use the same inline Token component',
    )
    assert(
      !table.includes('AntiTruncationBadge') && !table.includes("'tps'"),
      'no badge / TPS column / preference key',
    )
    assert(
      table.includes('compact(record.input_tokens)') &&
        table.includes('compact(record.reasoning_tokens)') &&
        table.includes('compact(record.reply_tokens)'),
      'second-line token breakdown remains',
    )
    assert.equal(networkCalls, 0)
    console.log(
      JSON.stringify({
        actual_component_assertions: modelAssertions + tokenAssertions + 2,
        model_assertions: modelAssertions,
        token_assertions: tokenAssertions,
        languages: 7,
        network_calls: networkCalls,
        scope: 'candidate components; not full browser/layout or online validation',
      }),
    )
  } finally {
    await server.close()
  }
}
