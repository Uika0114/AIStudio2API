export const quotaZhCN = {
  'quota.scope': 'Playground · Argon',
  'quota.tokens': 'Token',
  'quota.requests': '请求数',
  'quota.remaining': '剩余',
  'quota.total': '总额',
  'quota.used': '已用',
  'quota.account': '账号',
  'quota.accounts': '{count} 个账号',
  'quota.noAccounts': '暂无账号明细',
  'quota.reset': '重置',
  'quota.estimate': '本地估算',
  'quota.unavailable': '读取失败',
} as const

export const quotaEn: Record<keyof typeof quotaZhCN, string> = {
  'quota.scope': 'Playground · Argon',
  'quota.tokens': 'Token',
  'quota.requests': 'Requests',
  'quota.remaining': 'Remaining',
  'quota.total': 'Total',
  'quota.used': 'Used',
  'quota.account': 'Account',
  'quota.accounts': '{count} accounts',
  'quota.noAccounts': 'No account breakdown available',
  'quota.reset': 'Resets',
  'quota.estimate': 'Local estimate',
  'quota.unavailable': 'Unavailable',
}
