<script setup lang="ts">
import type { QuotaEstimateReport } from '@/types'
import QuotaBars from './QuotaBars.vue'

defineProps<{
  report: QuotaEstimateReport | null
  error: string
  carbonReport: QuotaEstimateReport | null
  carbonError: string
  hydrogenReport: QuotaEstimateReport | null
  hydrogenError: string
}>()
</script>

<template>
  <div class="space-y-2.5" data-quota-pools>
    <QuotaBars :report="report" :error="error" model-label="Argon" />
    <QuotaBars
      v-if="(carbonReport?.accounts?.length ?? 0) > 0"
      :report="carbonError ? null : carbonReport"
      :error="carbonError"
      model-label="Carbon"
    />
    <QuotaBars
      v-if="(hydrogenReport?.accounts?.length ?? 0) > 0"
      :report="hydrogenError ? null : hydrogenReport"
      :error="hydrogenError"
      model-label="Hydrogen"
    />
  </div>
</template>
