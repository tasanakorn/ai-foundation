const intFmt = new Intl.NumberFormat()

export function fmtInt(n: number | undefined): string {
  return intFmt.format(n ?? 0)
}

/** USD with enough decimals for tiny per-request costs. */
export function fmtUSD(n: number | undefined): string {
  const v = n ?? 0
  if (v === 0) return '$0'
  const digits = v >= 1 ? 2 : v >= 0.01 ? 4 : 6
  return `$${v.toFixed(digits)}`
}

export function fmtTime(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString()
}

export function fmtDate(iso: string | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleDateString()
}
