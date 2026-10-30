import type {
  DaySlice,
  ModelSlice,
  MonthSlice,
  Project,
  ProjectSlice,
  ProviderDay,
  ProviderSlice,
  UsageHistory,
  UsageWindow,
} from '@waku/client'
import { useState, type ReactNode } from 'react'
import { Virtuoso } from 'react-virtuoso'
import { ControlMenu } from '@/components/control-menu'
import { UsageTrendChart } from '@/components/usage-chart'
import { ProviderIcon, WakuIcon } from '@/components/waku-icon'
import { useUsageHistory } from '@/hooks/use-daemon-data'
import { useI18n, type AppLocale } from '@/lib/i18n'
import type { Translator } from '@/lib/transcript-presentation'
import { projectDisplayName } from '@/lib/project-presentation'
import { cn } from '@/lib/utils'

type UsageView = 'daily' | 'monthly' | 'projects'
type UsageBreakdown = 'model' | 'day'

const USAGE_WINDOWS: Array<{ window: UsageWindow; labelKey: string }> = [
  { window: { trailingDays: 7 }, labelKey: 'usage.last_7_days' },
  { window: { trailingDays: 30 }, labelKey: 'usage.last_30_days' },
  { window: { trailingDays: 90 }, labelKey: 'usage.last_90_days' },
  { window: 'thisMonth', labelKey: 'usage.this_month' },
  { window: 'lastMonth', labelKey: 'usage.last_month' },
]

export function UsageSettings({ projects }: { projects: Project[] }) {
  const { locale, t } = useI18n()
  const [view, setView] = useState<UsageView>('daily')
  const [window, setWindow] = useState<UsageWindow>({ trailingDays: 30 })
  const [breakdown, setBreakdown] = useState<UsageBreakdown>('model')
  const [projectFilter, setProjectFilter] = useState('')
  const requestedWindow: UsageWindow = view === 'monthly' ? { months: 12 } : window
  const usage = useUsageHistory(requestedWindow, projects)
  const history = usage.data && historyMatchesView(usage.data, view) ? usage.data : undefined
  const range = history
    ? formatUsageRange(history.sinceDay, history.untilDay, view === 'monthly', locale, t)
    : t('usage.scanning')

  return (
    <div className="mt-1.5 flex min-h-0 flex-col">
      <div className="flex min-h-8 flex-wrap items-center gap-2.5">
        <div className="min-w-0 flex-1 truncate text-[12.5px] text-[var(--text-secondary)]">{range}</div>
        <Segmented
          options={[['daily', t('usage.daily')], ['monthly', t('usage.monthly')], ['projects', t('usage.projects')]]}
          value={view}
          onChange={(next) => setView(next as UsageView)}
        />
        {view !== 'monthly' && (
          <ControlMenu
            align="right"
            items={USAGE_WINDOWS.map((option) => ({
              id: usageWindowKey(option.window),
              label: t(option.labelKey),
              selected: usageWindowKey(option.window) === usageWindowKey(window),
              onSelect: () => setWindow(option.window),
            }))}
            label={usageWindowLabel(window, t)}
            menuClassName="w-[170px]"
            placement="below"
            triggerClassName="h-7 w-[124px] max-w-none justify-between border bg-background px-2.5 text-[10.5px]"
          />
        )}
        <button
          aria-label={t(usage.isFetching ? 'usage.scanning' : 'usage.rescan')}
          className="grid size-7 place-items-center rounded-[7px] border text-[var(--text-tertiary)] outline-none hover:bg-accent focus-visible:ring-1 focus-visible:ring-ring"
          title={t(usage.isFetching ? 'usage.scanning' : 'usage.rescan')}
          type="button"
          onClick={() => void usage.refetch()}
        >
          <WakuIcon className={cn('size-3', usage.isFetching && 'motion-safe:animate-spin')} name={usage.isFetching ? 'loaderCircle' : 'rotateCw'} />
        </button>
      </div>

      {usage.error && (
        <UsageNotice>{errorMessage(usage.error)}</UsageNotice>
      )}
      {history && history.errors.length > 0 && (
        <UsageNotice>
          {history.errors.map((error) => <div key={error}>{error}</div>)}
        </UsageNotice>
      )}

      {!history ? (
        <UsageSkeleton view={view} />
      ) : (
        <>
          {view === 'daily' && (
            <DailyUsage history={history} breakdown={breakdown} onBreakdownChange={setBreakdown} />
          )}
          {view === 'monthly' && <MonthlyUsage history={history} />}
          {view === 'projects' && (
            <ProjectsUsage history={history} projects={projects} filter={projectFilter} onFilterChange={setProjectFilter} />
          )}
          <div className="mt-4 text-[9.5px] text-[var(--text-ghost)]">
            {t('usage.scan_summary', {
              scanned: formatCount(history.scannedFiles, locale),
              skipped: formatCount(history.skippedFiles, locale),
              records: formatCount(history.records, locale),
              seconds: formatDurationValue(history.scanDuration, locale),
            })}
          </div>
        </>
      )}
    </div>
  )
}

function DailyUsage({
  history,
  breakdown,
  onBreakdownChange,
}: {
  history: UsageHistory
  breakdown: UsageBreakdown
  onBreakdownChange: (breakdown: UsageBreakdown) => void
}) {
  const { t } = useI18n()
  return (
    <>
      <div className="mt-5 flex flex-col items-start gap-7 xl:flex-row">
        <UsageSummary history={history} />
        <div className="min-w-0 flex-1 self-stretch">
          <div className="flex flex-wrap items-center gap-3">
            <div className="min-w-0 flex-1 text-[12.5px] font-medium">
              {t('usage.daily_processed_tokens')}
            </div>
            <ProviderLegend provider="claude" label="Claude Code" />
            <ProviderLegend provider="codex" label="Codex" />
          </div>
          <div className="mt-2 min-w-0">
            <UsageTrendChart history={history} />
          </div>
        </div>
      </div>

      <UsageMetricStrip history={history} />

      <div className="mt-6 flex flex-col items-start gap-8 xl:flex-row">
        <div className="min-w-0 flex-1 self-stretch">
          <div className="flex items-center gap-3">
            <div className="min-w-0 flex-1 text-[12.5px] font-medium">{t('usage.breakdown')}</div>
            <MiniSegmented
              options={[['model', t('usage.model_upper')], ['day', t('usage.day_upper')]]}
              value={breakdown}
              onChange={(next) => onBreakdownChange(next as UsageBreakdown)}
            />
          </div>
          <div className="mt-2 min-w-0 overflow-x-auto">
            {breakdown === 'model' ? <ModelTable models={history.models} /> : <DayTable days={history.daily} />}
          </div>
        </div>
      </div>
    </>
  )
}

function UsageSummary({ history }: { history: UsageHistory }) {
  const { locale, t } = useI18n()
  const providers = [...history.providers].sort((left, right) => (
    right.totalTokens - left.totalTokens
  ))
  return (
    <div className="w-full shrink-0 xl:w-[300px]">
      <div className="text-[10px] uppercase tracking-[0.02em] text-[var(--text-tertiary)]">
        {t('usage.processed_tokens_upper')}
      </div>
      <div className="mt-0.5 text-[30px] font-medium leading-tight tabular-nums">
        {formatNumber(history.totalTokens, locale)}
      </div>
      <div className="mt-1 text-[10.5px] text-[var(--text-tertiary)]">
        {t(history.sessions === 1 ? 'usage.session_one' : 'usage.session_many', { count: formatCount(history.sessions, locale) })}
      </div>
      <div className="mt-[18px] flex flex-col gap-[18px]">
        {providers.length ? providers.map((provider) => (
          <ProviderSummary key={provider.provider} provider={provider} />
        )) : (
          <div className="text-[11.5px] text-[var(--text-tertiary)]">{t('usage.no_activity_window')}</div>
        )}
      </div>
    </div>
  )
}

function ProviderSummary({ provider }: { provider: ProviderSlice }) {
  const { locale, t } = useI18n()
  const share = provider.tokenShare
  const value = formatNumber(provider.totalTokens, locale)
  const detail = t('usage.token_share', { share: formatPercent(share, locale) })
  return (
    <div>
      <div className="flex items-center gap-2">
        <ProviderIcon className={cn('size-3.5', provider.provider === 'claude' ? 'text-[#d97757]' : 'text-foreground')} provider={provider.provider} />
        <div className="min-w-0 flex-1 truncate text-[12.5px]">{provider.provider === 'claude' ? 'Claude Code' : 'Codex'}</div>
        <div className="text-[12.5px] tabular-nums">{value}</div>
      </div>
      <div className="mt-1.5 h-1 overflow-hidden rounded-full bg-[var(--inset)]">
        <div className={cn('h-full rounded-full', provider.provider === 'claude' ? 'bg-[#d97757]' : 'bg-[var(--provider-codex)]')} style={{ width: `${Math.max(0, Math.min(100, share * 100))}%` }} />
      </div>
      <div className="mt-1 text-[10.5px] text-[var(--text-tertiary)]">{detail}</div>
    </div>
  )
}

function UsageMetricStrip({ history }: { history: UsageHistory }) {
  const { locale, t } = useI18n()
  const activeDays = history.daily.filter((day) => day.totalTokens > 0).length
  const dailyAverage = activeDays ? history.totalTokens / activeDays : 0
  const observedInput = history.totals.uncachedInput + history.totals.cachedInput
  const cachedShare = observedInput ? history.totals.cachedInput / observedInput : 0
  const tiles = [
    [t('usage.processed_tokens'), formatNumber(history.totalTokens, locale), t('usage.per_active_day', { count: formatNumber(dailyAverage, locale) })],
    [t('usage.cached_input'), formatNumber(history.totals.cachedInput, locale), t('usage.observed_input_share', { share: formatPercent(cachedShare, locale) })],
    [t('usage.uncached_input'), formatNumber(history.totals.uncachedInput, locale), t('usage.cache_writes', { count: formatNumber(history.totals.cacheCreation, locale) })],
    [t('usage.output'), formatNumber(history.totals.output, locale), t('usage.includes_reasoning', { count: formatNumber(history.totals.reasoning, locale) })],
  ]
  return (
    <div className="mt-6 grid border-y sm:grid-cols-2 lg:grid-cols-4">
      {tiles.map(([label, value, detail], index) => (
        <div className={cn('min-w-0 px-3.5 py-2.5', index > 0 && 'border-t sm:border-l sm:border-t-0', index === 2 && 'sm:border-l-0 lg:border-l')} key={label}>
          <div className="truncate text-[10px] text-[var(--text-tertiary)]">{label}</div>
          <div className="mt-0.5 truncate text-[15px] tabular-nums">{value}</div>
          <div className="mt-0.5 truncate text-[9.5px] text-[var(--text-tertiary)]">{detail}</div>
        </div>
      ))}
    </div>
  )
}

function ModelTable({ models }: { models: ModelSlice[] }) {
  const { locale, t } = useI18n()
  return (
    <UsageTable
      columns={<><span className="flex-1">{t('usage.model')}</span><span className="w-16 text-right">{t('usage.share')}</span><span className="w-20 text-right">{t('usage.tokens')}</span></>}
    >
      {models.length ? models.map((model) => (
        <div className="flex min-w-[500px] items-center gap-3 border-b py-2 text-[11.5px]" key={`${model.provider}:${model.model}`}>
          <span className="flex min-w-0 flex-1 items-center gap-2">
            <ProviderIcon className={cn('size-3', model.provider === 'claude' ? 'text-[#d97757]' : 'text-foreground')} provider={model.provider} />
            <span className="truncate">{model.model}</span>
          </span>
          <span className="w-16 text-right tabular-nums text-[var(--text-tertiary)]">{formatPercent(model.tokenShare, locale)}</span>
          <span className="w-20 text-right tabular-nums text-[var(--text-tertiary)]">{formatNumber(model.totalTokens, locale)}</span>
        </div>
      )) : <UsageEmpty />}
    </UsageTable>
  )
}

function DayTable({ days }: { days: DaySlice[] }) {
  const { locale, t } = useI18n()
  return (
    <UsageTable
      columns={<><span className="flex-1">{t('usage.day')}</span><span className="w-20 text-right">Claude Code</span><span className="w-20 text-right">Codex</span><span className="w-20 text-right">{t('usage.tokens')}</span></>}
    >
      {days.length ? [...days].reverse().slice(0, 8).map((day) => (
        <div className="flex min-w-[600px] items-center gap-3 border-b py-2 text-[11.5px]" key={day.day}>
          <span className="flex-1">{formatDay(day.day, locale)}</span>
          <span className="w-20 text-right tabular-nums text-[var(--text-tertiary)]">{formatNumber(day.byProvider[0].totalTokens, locale)}</span>
          <span className="w-20 text-right tabular-nums text-[var(--text-tertiary)]">{formatNumber(day.byProvider[1].totalTokens, locale)}</span>
          <span className="w-20 text-right tabular-nums text-[var(--text-tertiary)]">{formatNumber(day.totalTokens, locale)}</span>
        </div>
      )) : <UsageEmpty />}
    </UsageTable>
  )
}

function UsageTable({ columns, children }: { columns: ReactNode; children: ReactNode }) {
  return (
    <div>
      <div className="flex min-w-[500px] items-center gap-3 border-b pb-[7px] text-[10.5px] text-[var(--text-tertiary)]">{columns}</div>
      {children}
    </div>
  )
}

function MonthlyUsage({ history }: { history: UsageHistory }) {
  const { locale, t } = useI18n()
  const months = [...history.months].reverse()
  const peak = Math.max(0, ...months.map((month) => usageValue(month)))
  return (
    <UsageListCard
      caption={t('usage.tokens_and_sessions', {
        tokens: formatNumber(history.totalTokens, locale),
        sessions: t(history.sessions === 1 ? 'usage.session_one' : 'usage.session_many', {
          count: formatCount(history.sessions, locale),
        }),
      })}
      title={t('usage.last_12_months')}
      total={formatNumber(history.totalTokens, locale)}
    >
      <div className="h-[calc(100dvh-210px)] min-h-[360px] overflow-y-auto px-5 pb-2">
        {months.length ? months.map((month) => (
          <MonthUsageRow history={history} key={month.firstDay} locale={locale} month={month} peak={peak} t={t} />
        )) : <UsageEmpty message={t('usage.no_activity_12_months')} />}
      </div>
    </UsageListCard>
  )
}

function MonthUsageRow({
  history,
  month,
  peak,
  locale,
  t,
}: {
  history: UsageHistory
  month: MonthSlice
  peak: number
  locale: AppLocale
  t: Translator
}) {
  const currentMonth = history.untilDay.slice(0, 7) === month.firstDay.slice(0, 7)
  return (
    <div className="border-b py-3.5">
      <div className="flex items-baseline gap-2">
        <span className="text-[13px] font-medium">{formatMonth(month.firstDay, locale)}</span>
        {currentMonth && <span className="text-[9.5px] text-[var(--text-ghost)]">{t('usage.so_far')}</span>}
        <span className="flex-1" />
        <span className="text-[14px] font-medium tabular-nums">{formatNumber(month.totalTokens, locale)}</span>
      </div>
      <div className="mt-0.5 flex items-end gap-3">
        <span className="min-w-0 flex-1 truncate text-[10.5px] text-[var(--text-tertiary)]">
          {t('usage.month_caption', {
            tokens: formatNumber(month.totalTokens, locale),
            sessions: t(month.sessions === 1 ? 'usage.session_one' : 'usage.session_many', { count: formatCount(month.sessions, locale) }),
            days: t(month.activeDays === 1 ? 'usage.active_day_one' : 'usage.active_day_many', { count: formatCount(month.activeDays, locale) }),
          })}
        </span>
        <MonthActivityStrip history={history} month={month} />
      </div>
      <UsageSplitBar byProvider={month.byProvider} length={peak ? usageValue(month) / peak : 0} />
      <div className="mt-1.5 flex items-center gap-3 text-[9.5px] text-[var(--text-tertiary)]">
        <ProviderValue provider="claude" value={providerValue(month.byProvider[0])} locale={locale} />
        <ProviderValue provider="codex" value={providerValue(month.byProvider[1])} locale={locale} />
        <span className="min-w-0 flex-1 truncate text-right">{topModelsLabel(month.topModels)}</span>
      </div>
    </div>
  )
}

function ProjectsUsage({
  history,
  projects,
  filter,
  onFilterChange,
}: {
  history: UsageHistory
  projects: Project[]
  filter: string
  onFilterChange: (filter: string) => void
}) {
  const { locale, t } = useI18n()
  const normalized = filter.trim().toLocaleLowerCase()
  const rows = history.projects.filter((project) => {
    const identity = projectIdentity(project, projects, t)
    return !normalized || `${identity.name} ${identity.path ?? ''}`.toLocaleLowerCase().includes(normalized)
  })
  const peak = Math.max(0, ...rows.map((project) => usageValue(project)))
  return (
    <UsageListCard
      caption={`${t('usage.projects_shown', {
        shown: formatCount(rows.length, locale),
        projects: formatCount(history.projects.length, locale),
      })} · ${t(history.sessions === 1 ? 'usage.session_one' : 'usage.session_many', {
        count: formatCount(history.sessions, locale),
      })}`}
      title={t('usage.by_project')}
      total={formatNumber(history.totalTokens, locale)}
      action={(
        <label className="flex h-7 w-[240px] shrink-0 items-center gap-2 rounded-md border bg-background px-2.5 focus-within:border-ring">
          <WakuIcon className="size-3 text-[var(--text-tertiary)]" name="search" />
          <input
            aria-label={t('usage.filter_projects')}
            className="min-w-0 flex-1 bg-transparent text-[11px] outline-none placeholder:text-[var(--text-ghost)]"
            placeholder={t('usage.filter_projects')}
            value={filter}
            onChange={(event) => onFilterChange(event.target.value)}
          />
        </label>
      )}
    >
      <div className="h-[calc(100dvh-210px)] min-h-[360px]">
        {rows.length ? (
          <Virtuoso
            className="h-full pb-2"
            computeItemKey={(_, project) => project.path || 'other-sessions'}
            data={rows}
            itemContent={(_, project) => (
              <div className="px-5">
                <ProjectUsageRow project={project} projects={projects} locale={locale} peak={peak} t={t} />
              </div>
            )}
          />
        ) : <UsageEmpty message={t(history.projects.length ? 'usage.no_projects_match' : 'usage.no_activity_window')} />}
      </div>
    </UsageListCard>
  )
}

function ProjectUsageRow({
  project,
  projects,
  peak,
  locale,
  t,
}: {
  project: ProjectSlice
  projects: Project[]
  peak: number
  locale: AppLocale
  t: Translator
}) {
  const identity = projectIdentity(project, projects, t)
  const caption = [
    project.tokenShare > 0 ? t('usage.token_share', { share: formatPercent(project.tokenShare, locale) }) : null,
    t('usage.token_count', { count: formatNumber(project.totalTokens, locale) }),
    t(project.sessions === 1 ? 'usage.session_one' : 'usage.session_many', { count: formatCount(project.sessions, locale) }),
    project.lastDay ? t('usage.last_active', { date: formatDay(project.lastDay, locale) }) : null,
  ].filter(Boolean).join(' · ')
  return (
    <div className="border-b py-3.5">
      <div className="flex items-baseline gap-2">
        <span className="max-w-[40%] truncate text-[13px] font-medium">{identity.name}</span>
        {identity.path && <span className="min-w-0 flex-1 truncate text-[9.5px] text-[var(--text-ghost)]">{identity.path}</span>}
        <span className="shrink-0 text-[14px] font-medium tabular-nums">{formatNumber(project.totalTokens, locale)}</span>
      </div>
      <div className="mt-0.5 truncate text-[10.5px] text-[var(--text-tertiary)]">{caption}</div>
      <UsageSplitBar byProvider={project.byProvider} length={peak ? usageValue(project) / peak : 0} />
      <div className="mt-1.5 flex items-center gap-3 text-[9.5px] text-[var(--text-tertiary)]">
        <ProviderValue provider="claude" value={providerValue(project.byProvider[0])} locale={locale} />
        <ProviderValue provider="codex" value={providerValue(project.byProvider[1])} locale={locale} />
        <span className="min-w-0 flex-1 truncate text-right">{topModelsLabel(project.topModels)}</span>
      </div>
    </div>
  )
}

function UsageListCard({ title, caption, total, action, children }: { title: string; caption: string; total: string; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="mt-5 min-h-0 overflow-hidden rounded-[13px] bg-[var(--raised)]">
      <div className="flex items-center gap-4 border-b px-5 py-3.5">
        <div className="min-w-0 flex-1">
          <div className="text-[13.5px] font-medium">{title}</div>
          <div className="mt-0.5 flex items-baseline gap-1.5">
            <span className="text-[11.5px] font-medium tabular-nums">{total}</span>
            <span className="min-w-0 truncate text-[10.5px] text-[var(--text-tertiary)]">· {caption}</span>
          </div>
        </div>
        {action}
      </div>
      {children}
    </section>
  )
}

function UsageSplitBar({ byProvider, length }: { byProvider: [ProviderDay, ProviderDay]; length: number }) {
  const claude = providerValue(byProvider[0])
  const codex = providerValue(byProvider[1])
  const total = claude + codex
  return (
    <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-[var(--inset)]">
      <div className="flex h-full min-w-px overflow-hidden rounded-full" style={{ width: `${Math.max(0, Math.min(100, length * 100))}%` }}>
        <div className="h-full bg-[#d97757]" style={{ width: `${total ? claude / total * 100 : 0}%` }} />
        <div className="h-full bg-[var(--provider-codex)]" style={{ width: `${total ? codex / total * 100 : 0}%` }} />
      </div>
    </div>
  )
}

function MonthActivityStrip({ history, month }: { history: UsageHistory; month: MonthSlice }) {
  const days = history.daily.filter((day) => day.day.startsWith(month.firstDay.slice(0, 7)))
  const peak = Math.max(0, ...days.map((day) => day.totalTokens))
  return (
    <div className="flex h-5 w-[168px] shrink-0 items-end gap-px" aria-hidden="true">
      {days.map((day) => {
        const value = day.totalTokens
        const claude = providerValue(day.byProvider[0])
        const total = claude + providerValue(day.byProvider[1])
        return (
          <div className="flex h-full min-w-px flex-1 flex-col-reverse overflow-hidden rounded-t-[1px] bg-[var(--inset)]" key={day.day}>
            <div className="w-full bg-[var(--provider-codex)]" style={{ height: `${peak ? value / peak * 100 * (total ? 1 - claude / total : 0) : 0}%` }} />
            <div className="w-full bg-[#d97757]" style={{ height: `${peak ? value / peak * 100 * (total ? claude / total : 0) : 0}%` }} />
          </div>
        )
      })}
    </div>
  )
}

function ProviderValue({
  provider,
  value,
  locale,
}: {
  provider: 'claude' | 'codex'
  value: number
  locale: AppLocale
}) {
  return (
    <span className="flex items-center gap-1.5">
      <ProviderIcon className={cn('size-[11px]', provider === 'claude' ? 'text-[#d97757]' : 'text-foreground')} provider={provider} />
      {formatNumber(value, locale)}
    </span>
  )
}

function ProviderLegend({ provider, label }: { provider: 'claude' | 'codex'; label: string }) {
  return (
    <span className="flex items-center gap-1.5 text-[10.5px] text-[var(--text-secondary)]">
      <ProviderIcon className={cn('size-3', provider === 'claude' ? 'text-[#d97757]' : 'text-foreground')} provider={provider} />
      {label}
    </span>
  )
}

function Segmented({ value, options, onChange }: { value: string; options: Array<[string, string]>; onChange: (value: string) => void }) {
  return (
    <div className="flex overflow-hidden rounded-[7px] border">
      {options.map(([id, label]) => (
        <button
          aria-pressed={value === id}
          className={cn('h-[26px] px-[11px] text-[10.5px] text-[var(--text-secondary)] outline-none hover:text-foreground focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-ring', value === id && 'bg-accent text-foreground')}
          key={id}
          type="button"
          onClick={() => onChange(id)}
        >
          {label}
        </button>
      ))}
    </div>
  )
}

function MiniSegmented(props: Parameters<typeof Segmented>[0]) {
  return <Segmented {...props} />
}

function UsageNotice({ children }: { children: ReactNode }) {
  return <div className="mt-3 rounded-lg border px-3 py-2 text-[10.5px] leading-4 text-[var(--text-tertiary)]">{children}</div>
}

function UsageSkeleton({ view }: { view: UsageView }) {
  return (
    <div className="mt-5 motion-safe:animate-pulse">
      {view === 'daily' ? (
        <>
          <div className="flex gap-7"><div className="h-52 w-[300px] rounded-xl bg-[var(--raised)]" /><div className="h-52 flex-1 rounded-xl bg-[var(--raised)]" /></div>
          <div className="mt-6 h-20 rounded-xl bg-[var(--raised)]" />
        </>
      ) : <div className="h-[calc(100dvh-210px)] min-h-[360px] rounded-[13px] bg-[var(--raised)]" />}
    </div>
  )
}

function UsageEmpty({ message }: { message?: string }) {
  const { t } = useI18n()
  return <div className="py-8 text-center text-[11.5px] text-[var(--text-tertiary)]">{message ?? t('usage.no_activity_window')}</div>
}

function historyMatchesView(history: UsageHistory, view: UsageView) {
  const monthly = typeof history.window === 'object' && 'months' in history.window
  return view === 'monthly' ? monthly : !monthly
}

function usageWindowKey(window: UsageWindow) {
  return typeof window === 'string' ? window : 'trailingDays' in window ? `days:${window.trailingDays}` : `months:${window.months}`
}

function usageWindowLabel(window: UsageWindow, t: Translator) {
  const option = USAGE_WINDOWS.find((candidate) => usageWindowKey(candidate.window) === usageWindowKey(window))
  return option ? t(option.labelKey) : t('usage.custom')
}

function formatUsageRange(
  start: string,
  end: string,
  monthly: boolean,
  locale: AppLocale,
  t: Translator,
) {
  const formatter = new Intl.DateTimeFormat(locale, monthly
    ? { month: 'short', year: 'numeric', timeZone: 'UTC' }
    : { month: 'short', day: 'numeric', year: start.slice(0, 4) === end.slice(0, 4) ? undefined : 'numeric', timeZone: 'UTC' })
  return t('usage.range', {
    start: formatter.format(parseDay(start)),
    end: formatter.format(parseDay(end)),
  })
}

function formatDay(day: string, locale?: AppLocale) {
  return new Intl.DateTimeFormat(locale, { month: 'short', day: 'numeric', timeZone: 'UTC' }).format(parseDay(day))
}

function formatMonth(day: string, locale?: AppLocale) {
  return new Intl.DateTimeFormat(locale, { month: 'long', year: 'numeric', timeZone: 'UTC' }).format(parseDay(day))
}

function parseDay(day: string) {
  return new Date(`${day}T00:00:00Z`)
}

function projectIdentity(project: ProjectSlice, projects: Project[], t?: Translator) {
  if (!project.path) return { name: t ? t('usage.other_sessions') : 'Other sessions', path: null }
  const known = projects.find((candidate) => candidate.path === project.path)
  const normalized = project.path.replace(/[\\/]+$/, '')
  return {
    name: known
      ? projectDisplayName(known, t ? t('project.no_project_name') : 'No project')
      : normalized.split(/[\\/]/).at(-1) || project.path,
    path: project.path,
  }
}

function topModelsLabel(models: Array<[string, number]>) {
  if (!models.length) return ''
  return models.slice(0, 2).map(([model]) => model).join(' · ')
}

function providerValue(provider: ProviderDay) {
  return provider.totalTokens
}

function usageValue(item: { totalTokens: number }) {
  return item.totalTokens
}

function formatNumber(value: number, locale?: AppLocale) {
  return new Intl.NumberFormat(locale, { notation: value >= 10_000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(value)
}

function formatPercent(value: number, locale?: AppLocale) {
  return new Intl.NumberFormat(locale, { style: 'percent', maximumFractionDigits: 1 }).format(value)
}

function formatCount(value: number, locale?: AppLocale) {
  return new Intl.NumberFormat(locale).format(value)
}

function formatDurationValue(duration: { secs: number; nanos: number }, locale: AppLocale) {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(
    duration.secs + duration.nanos / 1_000_000_000,
  )
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}
