<script setup lang="ts">
import { computed } from 'vue'
import type { UsageRecord } from '@/types'
import { recordModelName } from './recordDisplay'

const props = defineProps<{
  record: Pick<UsageRecord, 'model' | 'requested_model' | 'base_model' | 'anti_truncation'>
}>()
const name = computed(() => recordModelName(props.record))
</script>

<template>
  <span v-tooltip.overflow="name" class="block truncate">{{ name }}</span>
</template>
