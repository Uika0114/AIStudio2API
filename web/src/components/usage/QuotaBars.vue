<script setup lang="ts">
import { computed } from 'vue'
import {
  ProgressIndicator,
  ProgressRoot,
  TooltipContent,
  TooltipPortal,
  TooltipProvider,
  TooltipRoot,
  TooltipTrigger,
} from 'reka-ui'
import { useI18n } from '@/i18n'
import type { QuotaBalance, QuotaEstimateReport } from '@/types'
import UiIcon from '../UiIcon.vue'

const props = defineProps<{ report: QuotaEstimateReport | null; error: string }>()
const { t, locale } = useI18n()
const compact = computed(
  () => new Intl.NumberFormat(locale.value, { notation: 'compact', maximumFractionDigits: 1 }),
)
const integer = computed(() => new Intl.NumberFormat(locale.value))
const percentage = computed(
  () => new Intl.NumberFormat(locale.value, { style: 'percent', maximumFractionDigits: 1 }),
)
const metrics = computed(() => [
  {
    key: 'tokens' as const,
    icon: 'tokens' as const,
    label: t('quota.tokens'),
    balance: props.report?.tokens,
    color: 'bg-blue-500/35',
  },
  {
    key: 'requests' as const,
    icon: 'requests' as const,
    label: t('quota.requests'),
    balance: props.report?.requests,
    color: 'bg-green-500/30',
  },
])
const accounts = computed(() => props.report?.accounts ?? [])
function valid(value: number | null | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}
function ratio(balance: QuotaBalance | undefined): number | undefined {
  if (!balance || !valid(balance.limit) || balance.limit === 0 || !valid(balance.remaining))
    return undefined
  return Math.min(100, (balance.remaining / balance.limit) * 100)
}
function number(value: number | null | undefined, tokens: boolean): string {
  return valid(value) ? (tokens ? compact.value : integer.value).format(value) : '—'
}
function exact(value: number | null | undefined): string {
  return valid(value) ? integer.value.format(value) : '—'
}
function used(balance: QuotaBalance): number | null {
  return valid(balance.used) ? balance.used : null
}
function percent(balance: QuotaBalance | undefined): string {
  const value = ratio(balance)
  return value === undefined ? '—' : percentage.value.format(value / 100)
}
</script>

<template>
  <TooltipProvider :delay-duration="300" :skip-delay-duration="0">
    <section class="space-y-2.5" :aria-label="t('quota.scope')" data-quota-bars>
      <TooltipRoot v-for="metric in metrics" :key="metric.key">
        <TooltipTrigger as-child>
          <div
            :data-quota="metric.key"
            :aria-label="metric.label"
            role="group"
            tabindex="0"
            class="cursor-help rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-blue-500/70 focus-visible:ring-offset-2 focus-visible:ring-offset-canvas"
          >
            <ProgressRoot
              :model-value="ratio(metric.balance) ?? null"
              :max="100"
              :aria-label="metric.label"
              :aria-valuetext="`${t('quota.remaining')} ${number(metric.balance?.remaining, metric.key === 'tokens')} / ${number(metric.balance?.limit, metric.key === 'tokens')}`"
              class="relative h-8 w-full overflow-hidden rounded-md bg-raised"
              :class="{ 'quota-unknown': ratio(metric.balance) === undefined }"
            >
              <ProgressIndicator
                v-if="ratio(metric.balance) !== undefined"
                class="h-full w-full"
                :class="metric.color"
                :style="{ transform: `translateX(-${100 - ratio(metric.balance)!}%)` }"
              />
              <div
                class="pointer-events-none absolute inset-0 flex items-center justify-between gap-3 px-3 text-xs font-medium tabular-nums text-gray-200"
                data-quota-overlay
              >
                <span class="flex min-w-0 items-center gap-2 whitespace-nowrap">
                  <UiIcon :name="metric.icon" :size="14" class="shrink-0 opacity-70" />
                  <span class="shrink-0">{{ metric.label }}</span>
                  <span
                    ><span class="font-semibold">{{
                      number(metric.balance?.remaining, metric.key === 'tokens')
                    }}</span
                    ><span class="px-1.5 opacity-50">/</span
                    ><span class="opacity-75">{{
                      number(metric.balance?.limit, metric.key === 'tokens')
                    }}</span></span
                  >
                </span>
                <span class="shrink-0">{{ percent(metric.balance) }}</span>
              </div>
            </ProgressRoot>
          </div>
        </TooltipTrigger>
        <TooltipPortal>
          <TooltipContent
            side="bottom"
            align="start"
            :side-offset="7"
            :collision-padding="12"
            class="z-50 max-h-[70vh] w-[430px] max-w-[calc(100vw-24px)] overflow-auto rounded-lg border border-line bg-overlay p-3 text-xs text-gray-300 shadow-xl"
            :data-quota-breakdown="metric.key"
          >
            <div class="mb-3 flex items-center justify-between gap-4">
              <span class="flex items-center gap-2 font-medium text-gray-200"
                ><UiIcon :name="metric.icon" :size="14" class="text-gray-500" />{{
                  metric.label
                }}</span
              >
              <span v-if="accounts.length" class="text-[11px] text-gray-500">{{
                t('quota.accounts').replace('{count}', String(accounts.length))
              }}</span>
            </div>
            <div v-if="error" role="status" class="py-3 text-red-400">{{ error }}</div>
            <table v-else-if="accounts.length" class="w-full border-collapse tabular-nums">
              <thead>
                <tr class="border-b border-line text-[11px] text-gray-500">
                  <th scope="col" class="pb-2 text-left font-normal">{{ t('quota.account') }}</th>
                  <th scope="col" class="pb-2 pl-2 text-right font-normal">
                    {{ t('quota.used') }}
                  </th>
                  <th scope="col" class="pb-2 pl-2 text-right font-normal">
                    {{ t('quota.remaining') }}
                  </th>
                  <th scope="col" class="pb-2 pl-2 text-right font-normal">
                    {{ t('quota.total') }}
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="account in accounts"
                  :key="account.id"
                  class="border-b border-line/50 last:border-0"
                  :data-quota-detail-account="account.id"
                >
                  <th
                    scope="row"
                    class="max-w-28 truncate py-2 pr-2 text-left font-normal text-gray-400"
                  >
                    <span v-if="account.exhausted_429" class="mr-1 text-amber-400">429</span>
                    {{ account.label }}
                  </th>
                  <td class="py-2 pl-2 text-right">{{ exact(used(account[metric.key])) }}</td>
                  <td
                    class="py-2 pl-2 text-right font-medium"
                    :class="account[metric.key].remaining === 0 ? 'text-red-400' : 'text-gray-200'"
                  >
                    {{ exact(account[metric.key].remaining) }}
                  </td>
                  <td class="py-2 pl-2 text-right text-gray-500">
                    {{ exact(account[metric.key].limit) }}
                  </td>
                </tr>
              </tbody>
            </table>
            <div v-else class="py-3 text-gray-500">{{ t('quota.noAccounts') }}</div>
          </TooltipContent>
        </TooltipPortal>
      </TooltipRoot>
    </section>
  </TooltipProvider>
</template>

<style scoped>
.quota-unknown {
  background: repeating-linear-gradient(
    135deg,
    var(--color-line) 0,
    var(--color-line) 2px,
    var(--color-raised) 2px,
    var(--color-raised) 6px
  );
}
</style>
