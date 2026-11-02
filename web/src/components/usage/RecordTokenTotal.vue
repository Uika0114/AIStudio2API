<script setup lang="ts">
import { computed } from 'vue'
import type { UsageRecord } from '@/types'
import { useI18n } from '@/i18n'
import { useUsageFormat } from './format'
import { tokenTotalWithTPS } from './recordDisplay'

const props = defineProps<{ record: Pick<UsageRecord, 'total_tokens' | 'output_tps'> }>()
const { locale } = useI18n()
const { compact } = useUsageFormat()
const line = computed(() =>
  tokenTotalWithTPS(compact(props.record.total_tokens), props.record.output_tps, locale.value),
)
</script>

<template>
  <span>{{ line }}</span>
</template>
