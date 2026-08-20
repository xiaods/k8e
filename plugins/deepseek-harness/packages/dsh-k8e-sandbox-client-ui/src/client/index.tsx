/**
 * Client half of dsh-k8e-sandbox (KIP-20 M2): registers the sandbox settings
 * page (`settings.section`) with locale dictionaries. The terminal panel is a
 * lazy chunk (see chunk-loader.ts / terminal.tsx) so xterm never ships in this
 * core bundle.
 * @module @k8e-sandbox/dsh-k8e-sandbox-client-ui/client
 */

import type { Context } from './context-types.ts'
import { K8eSettingsSection } from './settings-section.tsx'
import { watchSandboxActivity } from './activity.ts'
import { registerBetterSidebarTab } from './better-sidebar.ts'

const NS = 'k8e-sandbox'

const zh = {
  'section.nav': 'K8E 沙盒',
  'section.title': 'K8E 沙盒',
  'status.heading': '沙盒状态',
  'status.connected': '已连接',
  'status.noGrpc': '未配置 gRPC（终端需要显式 endpoint）',
  'status.cwd': '工作目录',
  'status.endpointFromProfile': 'connect 地址自动发现自 ~/.k8e/sandbox/profiles.yaml',
  'status.endpointFromEnv': 'connect 地址来自环境变量 K8E_SANDBOX_ENDPOINT',
  'status.endpointFromConfig': 'connect 地址来自 dsh profile 的 dsh-k8e-sandbox 配置',
  'status.noEndpointHint': '未发现 connect 地址 — 运行 k8e sandbox connect --endpoint <addr> --apikey <key> 生成 ~/.k8e/sandbox/profiles.yaml 后重启 dsh',
  'prefs.heading': '终端偏好',
  'prefs.rows': '行数',
  'prefs.cols': '列数',
  'prefs.autoOpen': '调用沙盒时自动打开终端',
  'actions.openTerminal': '打开终端',
  'actions.save': '保存',
  'actions.saved': '已保存',
  'hint.hostConfig': 'endpoint 未显式配置时，插件会自动从 ~/.k8e/sandbox/profiles.yaml 发现（KIP-17）；runtimeClass 等其余配置在 dsh-k8e-sandbox 行里设置。',
} as const

const en: Record<keyof typeof zh, string> = {
  'section.nav': 'K8E Sandbox',
  'section.title': 'K8E Sandbox',
  'status.heading': 'Sandbox status',
  'status.connected': 'Connected',
  'status.noGrpc': 'gRPC not configured (terminal needs an explicit endpoint)',
  'status.cwd': 'Working directory',
  'status.endpointFromProfile': 'Connect address auto-discovered from ~/.k8e/sandbox/profiles.yaml',
  'status.endpointFromEnv': 'Connect address from env K8E_SANDBOX_ENDPOINT',
  'status.endpointFromConfig': 'Connect address from the dsh-k8e-sandbox profile row',
  'status.noEndpointHint': 'No connect address found — run k8e sandbox connect --endpoint <addr> --apikey <key> to generate ~/.k8e/sandbox/profiles.yaml, then restart dsh',
  'prefs.heading': 'Terminal preferences',
  'prefs.rows': 'Rows',
  'prefs.cols': 'Columns',
  'prefs.autoOpen': 'Auto-open terminal on sandbox activity',
  'actions.openTerminal': 'Open terminal',
  'actions.save': 'Save',
  'actions.saved': 'Saved',
  'hint.hostConfig': 'When endpoint is not configured explicitly, the plugin auto-discovers it from ~/.k8e/sandbox/profiles.yaml (KIP-17); other deploy config (runtimeClass) lives in the dsh-k8e-sandbox profile row.',
}

/** Client services required before activation. */
export const inject = ['slots', 'locale']

export function apply(ctx: Context): void {
  ctx.effect(() => ctx.locale.register(NS, { zh, en }), 'k8e-sandbox-ui: dictionaries')

  // The nav label is a locale-following thunk (re-resolved per render); the
  // section content reads the framework-injected `t` seat (`locale: NS`).
  const t = ctx.locale.bind(NS)
  ctx.slots.inject('settings.section', () => ctx.slots.register({
    name: 'settings.section',
    id: 'k8e-sandbox',
    order: 300,
    label: () => t('section.nav'),
    locale: NS,
  }, K8eSettingsSection))

  // Auto-open the terminal panel on sandbox activity (M3). The EventSource is
  // cheap; the heavy xterm chunk is loaded lazily on the first activity.
  ctx.effect(() => watchSandboxActivity(), 'k8e-sandbox-ui: activity watcher')

  // Optional better-sidebar tab (M4): skipped when better-sidebar is absent.
  registerBetterSidebarTab(ctx)
}

// Named exports only (no default): the client loader returns the module
// namespace when there is no `default`, which is how it reads both `apply` and
// `inject`. An `export default apply` would drop `inject` and fail with
// "cannot get property ... without inject".
