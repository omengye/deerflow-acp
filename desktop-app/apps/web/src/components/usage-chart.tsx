import { areaY, crosshair, defineChart, lineY } from '@tanstack/charts'
import { decorative } from '@tanstack/charts/mark/decorative'
import { Chart } from '@tanstack/charts/react'
import { scaleLinear } from '@tanstack/charts/scales/linear'
import { tooltip } from '@tanstack/charts/tooltip'
import type { UsageHistory } from '@waku/client'
import { scaleUtc } from 'd3-scale'
import { useI18n, type AppLocale } from '@/lib/i18n'

interface UsageChartRow {
  id: string
  date: Date
  provider: 'Claude Code' | 'Codex'
  value: number
}

export function UsageTrendChart({
  history,
}: {
  history: UsageHistory
}) {
  const { locale, t } = useI18n()
  const rows: UsageChartRow[] = history.daily.flatMap((day) => {
    const date = parseUsageDay(day.day)
    return [
      {
        id: `claude-${day.day}`,
        date,
        provider: 'Claude Code',
        value: day.byProvider[0].totalTokens,
      },
      {
        id: `codex-${day.day}`,
        date,
        provider: 'Codex',
        value: day.byProvider[1].totalTokens,
      },
    ]
  })

  const definition = defineChart({
    marks: [
      decorative(areaY(rows, {
        id: 'usage-area',
        x: 'date',
        y: 'value',
        z: 'provider',
        color: 'provider',
        key: 'id',
        fillOpacity: 0.1,
      })),
      lineY(rows, {
        id: 'usage-line',
        x: 'date',
        y: 'value',
        z: 'provider',
        color: 'provider',
        key: 'id',
        strokeWidth: 1.8,
      }),
      crosshair({
        x: { label: false, stroke: 'var(--text-ghost)', strokeOpacity: 0.7 },
        y: false,
      }),
    ],
    x: {
      scale: scaleUtc,
      axis: {
        line: false,
        ticks: {
          count: 3,
          size: 0,
          padding: 8,
          format: (value) => formatChartDay(value, locale),
        },
        tickLabels: { thin: { priority: 'ends', minGap: 24 }, fontSize: 10 },
      },
    },
    y: {
      scale: scaleLinear,
      nice: true,
      grid: true,
      axis: {
        line: false,
        ticks: {
          count: 4,
          size: 0,
          padding: 8,
          format: (value) => formatCompactNumber(value, locale),
        },
        tickLabels: { fontSize: 10 },
      },
    },
    color: {
      domain: ['Claude Code', 'Codex'],
      range: ['#d97757', 'var(--provider-codex)'],
    },
    focus: 'group-x',
    maxFocusDistance: Number.POSITIVE_INFINITY,
    tooltip,
  })

  if (!rows.length) {
    return (
      <div className="grid h-[188px] place-items-center text-[11.5px] text-[var(--text-tertiary)]">
        {t('usage.no_activity_window')}
      </div>
    )
  }

  return (
    <Chart
      ariaDescription={t('usage.chart_description', {
        metric: t('usage.processed_tokens'),
      })}
      ariaLabel={t('usage.daily_processed_tokens')}
      className="waku-usage-chart"
      definition={definition}
      height={188}
      initialWidth={640}
    />
  )
}

function parseUsageDay(day: string) {
  return new Date(`${day}T00:00:00Z`)
}

function formatChartDay(value: Date, locale: AppLocale) {
  return new Intl.DateTimeFormat(locale, { month: 'short', day: 'numeric', timeZone: 'UTC' }).format(value)
}

function formatCompactNumber(value: number, locale: AppLocale) {
  if (!value) return '0'
  return new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}
