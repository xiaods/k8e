# dsh-k8e-sandbox 客户端半身设计：配置页 + 沙盒终端页

> 参考实现：[DSH-better-sidebar](https://github.com/omdsh-dev/DSH-better-sidebar)（v0.12.x）。
> 目标：给 `@k8e-sandbox/dsh-k8e-sandbox` 补上 **client 半身**——① 一个配置页面；② 一个调用 sandbox 时出现的终端页，实时显示运行命令与输出。对应 issue #542 的「侧边栏集成」。

## 0. 现状与差距

KIP-20 目前只有 **host 半身**（`ctx.fs` / `ctx.subprocess` / `ctx.k8eSandbox`），没有浏览器侧 UI。`package.json` 只有 `dsh.bundle`，没有 `dsh.client`。

## 1. 从 DSH-better-sidebar 学到的可借鉴点

| 机制 | better-sidebar 怎么做 | 我们照搬/适配 |
|---|---|---|
| **双半身 manifest** | `package.json#dsh` 同时有 `bundle.patch`（host）+ `client: { inject, platform }`（浏览器） | 给 bundle 包加 `dsh.client` |
| **client inject** | `['@deepseek-ai/dsh-client-runtime','-locale','-ui-slots','-ui-conversation','-ui-settings','-ui-primitives','-web-react']` | 同款（按需裁剪） |
| **本地 type-only 契约** | `context-types.ts` 里用 `declare module 'cordis'` 重述 `ctx.webServer/slots/settings/...` 的结构面，避开 npm 断裂依赖 | 我们照做（`ctx.k8eSandbox` + webServer/slots/settings/connection） |
| **host HTTP/WS 路由** | `ctx.webServer.register({kind:'prefix', path, handler})` + `ctx.webServer.registerUpgrade({path, handler})` | 照做：`/k8e-sandbox/api/*` + `/k8e-sandbox/ws/terminal` |
| **配置页（设置 seam）** | host: `ctx.inject(['settings'], s => s.settings.register(ns, PrefsSchema))`；client: `ctx.slots.inject('settings.section', () => ctx.slots.register({name:'settings.section', id, order, label, inject}, Section))` | 照做，Section 渲染 endpoint/certDir/runtimeClass/rows/cols |
| **终端页** | host 用 node-pty + WebSocket；client 用 xterm.js + `@xterm/addon-fit`，WS 协议 = 裸文本输入 + `{type:'resize'}` JSON | 照做 WS 协议，但 pty 后端换成**我们的 KIP-19 远程 PTY（gRPC spawnTerminal）**，不是 node-pty |
| **构建** | tsdown：host → ESM `lib/index.js`；client → CJS closure factory `window.__ModuleLoader__.load({id, factory})`，react 等平台模块 external | 照做（或先 esbuild 等价实现） |
| **纯度门** | client bundle 禁止 value-import `@deepseek-ai/*`（跨插件只走 cordis 服务） | 照做：client 不 import dsh 服务包 |

关键结论：**我们的终端不是「本机 node-pty」，而是「远端沙盒 PTY」**——这是与 better-sidebar 最大的不同。其余（配置页、WS 桥、xterm 渲染、dsh.client 打包）可以几乎原样借鉴。

## 2. 总体架构

```
浏览器（client half，@k8e-sandbox/dsh-k8e-sandbox/client）
  ├── 设置页 section（配置页）：endpoint / certDir / runtimeClass / rows / cols
  ├── 沙盒终端面板：xterm.js，实时渲染命令输出 + 可交互 PTY
  │     │ WebSocket (/k8e-sandbox/ws/terminal)
  ▼     │
host half（新增 k8e-sandbox-host-ui 插件）
  ├── ctx.webServer.register('/k8e-sandbox/api/*')   # status / session / prefs
  ├── ctx.webServer.registerUpgrade('/k8e-sandbox/ws/terminal')
  │     ├── 交互式终端：GrpcK8eClient.spawnTerminal → 把 PTY 流桥成 WS
  │     └── 实时监控：订阅 ctx.subprocess 的 spawn 输出 → 推到 WS
  └── ctx.settings.register(ns, PrefsSchema)          # 用户偏好
        └── ctx.k8eSandbox（已有 owner 服务，读 endpoint/certDir）
```

## 3. 配置页面（两个层次）

### 3.1 部署级 Config（cordis.patch.yml 已能配）

已有：`endpoint / profile / certDir / cwd / runtimeClass / tenant / allowedHosts / pauseOnDispose`。这是「管理员在 profile 里写」的层，不改。

### 3.2 用户级 Prefs（设置页 section，新增）

用设置 seam 暴露一份**用户可在 UI 改**的偏好：

```ts
export const PrefsSchema = z.object({
  endpoint: z.string().default(''),        // 空 = 用 CLI 本地自动发现
  certDir: z.string().default(''),
  runtimeClass: z.string().default('gvisor'),
  rows: z.number().min(1).max(200).default(24),
  cols: z.number().min(1).max(400).default(80),
  autoOpenTerminal: z.boolean().default(true),  // 调用 sandbox 时自动弹终端
})
```

- **host**：`ctx.inject(['settings'], s => s.settings.register('k8e-sandbox', PrefsSchema))`，并把 endpoint/certDir 喂给 `ctx.k8eSandbox`（owner 的 `getGrpcClient()` 优先读运行时设置，其次读 Config）。
- **client**：`ctx.slots.inject('settings.section', () => ctx.slots.register({ name:'settings.section', id:'k8e-sandbox', order, label, inject }, K8eSettingsSection))`。Section 渲染上面几个字段（text/number 控件，学 better-sidebar §8 的声明式设置）。

## 4. 沙盒终端页

两个能力合成一个终端面板：

### 4.1 交互式 PTY（用户自己敲命令）

```
client xterm.js  ──WS──▶  host /k8e-sandbox/ws/terminal
                            │  GrpcK8eClient.createTerminal({argv:['bash','-l'], rows, cols})
                            │  GrpcK8eClient.terminalStream(terminalId)  → 喂 WS
                            │  WS input → GrpcK8eClient.terminalWrite
                            │  WS resize → GrpcK8eClient.terminalResize
                            │  WS close → GrpcK8eClient.terminalDestroy
```

WS 协议照搬 better-sidebar：裸文本 = 输入；`{type:'resize',cols,rows}` = 尺寸；`{type:'close'}` = 关闭；host 侧 `terminalStream` 的 data 帧逐段 `ws.send`，exit 帧发 `\r\n[process exited with code N]\r\n`。

### 4.2 实时命令监控（agent 跑命令时自动显示）

这是「调用 sandbox 时出来一个终端页、实时显示运行命令」的核心：

- host 在 `ctx.subprocess`（我们的 `K8eSubprocessRuntime`）每次 `spawn` 时，把命令 + stdout/stderr 流**旁路一份**到监控总线（一个进程内 EventEmitter/广播）。
- WS 连接附带 `?watch=1`（监控模式）时，host 把这股流转发过去；client 在一个只读的「命令回放/实时」区域渲染（`$ echo hello` → 输出 → exit code）。
- `autoOpenTerminal` 偏好开启时，client 侦听到「sandbox 有活动」就自动展开/聚焦终端面板。

> 实现落点：要么在 `K8eSubprocessRuntime.spawn/spawnTerminal` 里发一个 `ctx.on('k8e-sandbox/exec', ...)` 事件（把输出流桥出来），要么让 host-ui 插件包一层。优先前者——provider 自己发事件，最小侵入。

## 5. 包结构变化

新增一个 client/UI 包（或塞进现有 bundle）：

```
plugins/deepseek-harness/packages/
├── dsh-k8e-sandbox-bundle/       # 已有：dsh.bundle.patch（挂 host 行）
│   └── cordis.patch.yml          # 增加一行：k8e-sandbox-host-ui
├── dsh-k8e-sandbox-host-ui/      # 新增：host 半身的 UI 桥（HTTP/WS/settings）
│   └── src/index.ts
└── dsh-k8e-sandbox-client-ui/    # 新增：client 半身（dsh.client 的 ./client 入口）
    └── src/index.tsx             # 设置 section + 终端面板（xterm.js）
```

`dsh-k8e-sandbox-bundle/package.json` 增加：

```jsonc
"dsh": {
  "bundle": { "patch": "./cordis.patch.yml" },
  "client": {
    "inject": [
      "@deepseek-ai/dsh-client-runtime",
      "@deepseek-ai/dsh-client-locale",
      "@deepseek-ai/dsh-client-ui-slots",
      "@deepseek-ai/dsh-client-ui-conversation",
      "@deepseek-ai/dsh-client-ui-settings",
      "@deepseek-ai/dsh-client-ui-primitives",
      "@deepseek-ai/dsh-client-web-react"
    ],
    "platform": "web"
  }
}
```

## 6. 构建与按需加载（核心 ~325KB + 懒 chunk）

**目标：启动只拉核心 bundle（~325KB）；终端（xterm）等重依赖首次用到才按需拉。** 照搬 better-sidebar 的懒 chunk 机制（`chunk-loader.ts` + `bundle-route.ts` + tsdown `chunkBundle`）：

### 产物拆分

| 产物 | 内容 | 加载时机 |
|---|---|---|
| `lib/index.js`（host，ESM） | host-ui 桥（HTTP/WS/settings），`@deepseek-ai/*` peer | profile 启动 |
| `lib/client.js`（client 核心，CJS closure `window.__ModuleLoader__.load({id, factory})`） | 设置 section + 终端**外壳**（占位 + 懒加载触发）；`react`/平台模块 external；**不内联 xterm** | 页面启动（~325KB） |
| `lib/client-terminal.js`（懒 chunk） | xterm + `@xterm/addon-fit` 打包成独立脚本，`globalThis.__dshChunks__["terminal"] = (require) => {...}` | 终端首次打开 |

### 懒加载流程（client `chunk-loader`）

1. 终端 tab 首次打开 → `loadChunk('terminal')`；
2. inject `<script src="/k8e-sandbox/bundle/terminal.js">`（同源经典 script）；
3. 从 `globalThis.__dshChunks__["terminal"]` 取 factory，用 `window.__DSH_MODULES__.import(spec)` 构造 require（平台模块走 seed 表）调用之；
4. 缓存三层：in-flight promise 记忆化 / 失败清缓存重试 / 每次激活 `resetChunks()`（HMR 安全）。

### chunk 服务（host 路由 `/k8e-sandbox/bundle/<name>.js`）

- allowlist 名字（`terminal`），防路径穿越；
- `cache-control: no-cache` + ETag（mtime/size 记忆化哈希），304 复用——刷新/HMR 不重复下载多 MB chunk；
- 走与 `/api` 相同的 trust fence。

### 构建工具

Phase 1 用 esbuild 等价实现：核心 bundle + `terminal` chunk 各打一个（`__ModuleLoader__` / `__dshChunks__` 手交格式，`codeSplitting: false`）；摸清官方 `tsdown.client.ts` preset 后再切 tsdown。

## 7. 落地顺序

1. **M1**：`dsh-k8e-sandbox-host-ui`（`/k8e-sandbox/api/status` + settings namespace + 终端 WS 桥，先把 `spawnTerminal` 的 gRPC 流桥成 WS）。
2. **M2**：`dsh-k8e-sandbox-client-ui`（设置 section + xterm 终端面板 + `__ModuleLoader__` 打包）。
3. **M3**：实时命令监控（`spawn` 输出旁路 + `autoOpenTerminal` 自动展开）。
4. **M4**：可选 better-sidebar 集成（若装了 better-sidebar，用 `ctx.betterSidebar.registerTab` 挂我们的终端 tab）。

## 8. 已知风险

- **dsh.client 打包格式**（`__ModuleLoader__.load` 的 closure factory + 纯度门 + 平台模块 external 清单）是最容易踩坑的地方，需要对着 dsh 官方 `packages/client/tsdown.client.ts` preset 逐项核对。
- **xterm.js 体积**：已由懒 chunk 解决（见 §6）——核心 bundle 不内联 xterm，终端首次打开才拉 `client-terminal.js`。
- **终端 WS 的 mTLS**：WS 走 dsh webServer 的 trust fence（better-sidebar 同款），不直接暴露 gRPC mTLS；host 侧持 gRPC client 证书。

## 9. 现状审计（2026-08-21）：K8E 沙盒配置能否在网页编辑？

### 9.1 审计结论

**当前不能。** 网页 UI（`client-ui` 设置页）只能编辑三项**纯浏览器终端偏好**（rows / cols /
autoOpenTerminal，存 localStorage），K8E 沙盒配置本身全部只读：

| 配置项 | 来源 | 网页可编辑？ | 现状 |
|---|---|---|---|
| `endpoint` | Config / env / profiles.yaml (KIP-17) | ❌ | 设置页仅展示 + 标注来源；改 endpoint 需写 profile 行或 `k8e-sandbox-cli connect` 后重启 dsh |
| `runtimeClass` | Config（profile 行，默认 `gvisor`） | ❌ | 设置页仅展示；owner 构造时一次性解析（`dsh-k8e-sandbox/src/index.ts` `ResolvedConfig`） |
| `certDir` | Config | ❌ | status API 有返回，UI 不展示 |
| `tenant` / `cwd` / `allowedHosts` / `pauseOnDispose` | Config | ❌ | 均不可见、不可编辑 |
| `rows` / `cols` / `autoOpenTerminal` | localStorage | ✅ | 设置页可改，但只影响浏览器渲染，不经过 host，换浏览器即丢 |

**根因**：owner（`dsh-k8e-sandbox`）在插件 apply 时把 Config 一次解析进
`ResolvedConfig`，`getGrpcClient()` / 会话创建都读这份快照；host-ui 只有
`POST /k8e-sandbox/api/status` 一个只读方法，没有任何 prefs 读写接口。

### 9.2 设计：分层编辑模型

把「沙盒配置」按**改动成本 / 生效范围**分成三层，网页只开放安全的那层：

```
┌─ L0 Deployment Config（网页只读）─────────────────────────────┐
│   endpoint / certDir / tenant / allowedHosts / pauseOnDispose │
│   基础设施绑定 + 安全面；改它要重建 gRPC client，需重启 dsh。   │
│   设计决策：不提供网页编辑，设置页保持「展示 + 指引」。          │
├─ L1 User Prefs（网页可编辑 ✅ 本次新增）───────────────────────┤
│   runtimeClass（gvisor/kata/firecracker…）                    │
│   terminal.rows / terminal.cols / terminal.autoOpen           │
│   纯会话参数，按会话生效，无安全面；持久化到 host（跨浏览器）。  │
├─ L2 Session 覆盖（可选 Phase 2，终端面板内）───────────────────┤
│   终端页加 runtime 下拉，仅覆盖「本次打开的终端会话」。          │
└────────────────────────────────────────────────────────────────┘
```

**为什么 `runtimeClass` 放 L1（而不是 L0）**：runtime 是**按会话**指定的
（warm pool 也按 runtimeClass 分池，见 `sandbox.k8e.io/runtime-class` label），
不涉及连接重建，改了只影响**新会话**，安全且高频（用户换 gVisor→Kata 试隔离）。

### 9.3 Host API 扩展（host-ui）

在现有 `/k8e-sandbox/api` prefix 下新增两个方法（保持 POST-only，与 status 一致）：

```jsonc
// POST /k8e-sandbox/api/prefs        → 读：返回「生效中」的合并视图
{
  "ok": true,
  "prefs": {
    "runtimeClass": "gvisor",        // L1，用户可改
    "rows": 24, "cols": 80, "autoOpenTerminal": true
  },
  "effective": {
    "runtimeClass": "gvisor",        // L1 覆盖后最终值
    "endpoint": "10.0.0.5:50051",    // L0 只读，供 UI 展示
    "endpointSource": "profile"
  }
}

// POST /k8e-sandbox/api/prefs/set    → 写：整包替换 L1（校验后持久化）
// body: { "prefs": { "runtimeClass": "kata", "rows": 40, "cols": 120, "autoOpenTerminal": false } }
// → 200 { "ok": true, "prefs": { ...已保存 } }
// → 400 { "ok": false, "error": { "code": "validation", "message": "..." } }
```

- **持久化**：host 侧写 `~/.k8e/sandbox/ui-prefs.json`（与 profiles.yaml 同目录，
  避免塞进用户目录根），atomic rename 写入；读时缺失回退默认值。
- **校验**：`runtimeClass` 只允许 allowlist（gvisor / kata / firecracker，可从
  `ctx.k8eSandbox` 的可用 runtime 列表校验，无列表时用内置 allowlist）；
  rows∈[1,200]、cols∈[1,400]、autoOpen 为 bool —— 复用现有 `clamp()`。
- **生效语义**：保存**不重建** gRPC client；`runtimeClass` 对**新建会话**生效，
  已存在会话不迁移（与 warm-pool 分池语义一致）；rows/cols/autoOpen 对终端即时生效。

### 9.4 Client 改动（client-ui）

- `settings-section.tsx`：`终端偏好` 卡片改为**从 `/k8e-sandbox/api/prefs` 读取、
  保存到 `/k8e-sandbox/api/prefs/set`**（不再是 localStorage）；新增 `沙盒运行时` 字段
  （下拉，可选 gvisor/kata/firecracker），保存后提示「对新建会话生效」。
- `prefs.ts`：`TerminalPrefs` 扩展 `runtimeClass`；`loadPrefs/savePrefs` 改为
  host API 调用（保留 localStorage 作为**离线兜底缓存**，host 不可达时降级）。
- `status` 卡片增加 `runtimeClass` 的「生效中」标注（区分 L1 覆盖 vs L0 默认）。

### 9.5 落地顺序

1. **M5a**：host-ui 加 `prefs` 读 + `prefs/set` 写（持久化 + 校验 + 单测）。
2. **M5b**：owner（`dsh-k8e-sandbox`）暴露 `setRuntimeClass()` 运行时覆盖
   （`ResolvedConfig.runtimeClass` 变为 getter，优先读 L1 prefs）。
3. **M5c**：client-ui 设置页接 host prefs API + runtime 下拉（替换 localStorage 为主路径）。
4. **M6**（可选 Phase 2）：终端面板内 runtime 下拉（L2 会话级覆盖）。

### 9.6 实现状态（2026-08-21）

| 里程碑 | 状态 | 落点 |
|---|---|---|
| M5a host-ui prefs 读写 API | ✅ | `dsh-k8e-sandbox-host-ui/src/prefs-store.ts`（校验/持久化/生效优先级，纯模块）+ `index.ts` 的 `prefs` / `prefs/set` 路由（body 校验 → atomic 写 `~/.k8e/sandbox/ui-prefs.json` → `ctx.k8eSandbox.setRuntimeClass()`） |
| M5b owner 运行时覆盖 | ✅ | `dsh-k8e-sandbox/src/index.ts`：`setRuntimeClass()` / `effectiveRuntimeClass` / `runtimeClassSource`；会话创建改用 effective；status API 上报 `runtimeClassSource` |
| M5c client 设置页接 host prefs | ✅ | `settings-section.tsx`：运行时下拉（gvisor/kata/firecracker + 默认）+ rows/cols/autoOpen 改为 host 读写；`prefs.ts` host 主路径 + localStorage 离线兜底；`index.tsx` 新增 zh/en 文案（saveError/runtimeHint/source 标注） |
| 顺带修复 | ✅ | client-ui 首次纳入 dsh 全量 typecheck（旧 tsconfig.check.json 未含 client-ui），修复 `exactOptionalPropertyTypes` 下 prefs.ts/exec-types.ts/terminal.tsx 的潜在类型错误 |
| 验证 | ✅ | `npm test` 7/7（新增 `tests/prefs.test.mjs`：校验/文件往返/生效优先级）；host-ui + owner `tsc` 构建通过；client 双 bundle 构建通过；dsh-checkout 全量 typecheck 通过 |

> L2 会话级 runtime 覆盖（终端面板内下拉）按 §9.2 设计留作可选 Phase 2，未实现。
