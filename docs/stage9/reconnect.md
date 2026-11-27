# 第九阶段（stage9-reconnect）：断网重连 / 离线补齐 / 刷新持久化联调报告

## 一、联调环境与隔离栈

- 分支：`agent/stage9-reconnect`（基线 `main` `b2e4cf1`）
- 后端：`SERVER_ADDR=:8083`（Go + Gin + Redis Streams + MySQL）
- 前端：`IM_VITE_PORT=5176`（Vite 开发服务器反代 `/api`、`/uploads`、`/ws` 到 `http://127.0.0.1:8083`）
- 隔离存储：MySQL 数据库 `im_stage9_reconnect`（基于 `deploy/mysql/init.sql` 临时副本导入 12 张表后立即删除副本）、Redis DB `3`、`SNOWFLAKE_NODE_ID=13`
- 浏览器引擎：Playwright Chromium (`1.65.1`) 与 WebKit (`1.65.1`)，全流程使用真实浏览器上下文与真实后端 `/api/v1` + `/ws`，未使用任何 API Mock。

## 二、覆盖场景与实测结果

### 1. 正常在线收发基线（Chromium）
- 真实账号注册（`/register`）、登录（`/login`）、搜索并加好友（`/contacts`）、好友申请同意并自动建立单聊会话。
- 发送方发送文本消息，接收方实时通过 WS `msg.push` 收到消息，发送方气泡状态推进至 `已送达`/`已读`。

### 2. 接收方断网 → 对端连续发送与撤回 → 重连增量补齐（Chromium + WebKit）
- 接收方上下文触发真实离线（`context.setOffline(true)`），前端 WS 状态指示立即切换为 `连接中`。
- 发送方继续发送 4 条离线消息并确认服务端返回 `已发送`（已持久化 MySQL 并投递至 Redis Streams），随后追加发送 1 条消息并在接收方离线期间完成撤回。
- 接收方恢复网络（`context.setOffline(false)`），WS 自动重连并完成 `ping/pong` 鉴权后执行 `/api/v1/conversations` 与 `/api/v1/conversations/:id/sync?after_seq=` 增量补偿：
  - 4 条离线消息 **零丢失、零重复**，按 `seq` 严格升序渲染；
  - 离线期间被撤回的消息在接收方侧就地显示「该消息已撤回」与撤回系统提示，原正文不再作为正常气泡展示；
  - 接收方补发 `delivered` / `read` 回执后，发送方状态推进至 `已读`。

### 3. 发送方未确认消息断网重试（Chromium）
- 发送方上下文断网后，状态切为 `连接中` 并显示离线补发提示。
- 发送方在离线状态下发送文本消息，界面立即渲染 `发送中` 乐观气泡并将消息保留在 `WSClient.pendingByAccount`。
- 恢复网络后，WS 在鉴权成功时先按原 `client_msg_id` 重发未确认帧（服务端按 `(sender_id, client_msg_id)` 幂等处理），随后执行 `/sync?after_seq=` 并合并实时订阅。
- 通过 `page.on('websocket')` 抓取发送帧验证 `client_msg_id` 保持一致；发送方与接收方界面均 **只出现 1 个气泡（零重复）**，状态推进为 `已发送`/`已读`。

### 4. 完整刷新页面（`page.reload()`）持久化与防重（Chromium + WebKit）
- 在会话详情页执行完整刷新后：登录态（`im_auth_tokens` + `GET /api/v1/me`）、当前会话路由、历史消息列表、已读/未读状态完整恢复，触发增量同步后旧消息 **零重复插入**。
- 离开当前会话并累计 2 条未读消息后执行刷新，左侧会话未读角标 `2` 完整持久化；点击进入会话后未读角标清零且发送方同步变为 `已读`；刷新后继续实时收发消息仍保持零重复。

### 5. 同账号多端登录（一端在线、一端离线后重连，Chromium）
- 同一用户登录两个独立浏览器上下文（各自独立 `device_id` 与 WS 会话）。
- 设备 2 离线期间，设备 1 发送消息并接收对端回复；设备 2 恢复网络后自动重连并通过 `/sync` 补齐自己发送的消息与对端回复，零丢失、零重复。

## 三、联调发现的缺陷与修复（仅限独占文件）

1. **`frontend/src/utils/wsClient.ts`**
   - **问题**：浏览器进入离线状态时，若本地 loopback WebSocket 尚未立即收到内核 FIN，`WSClient` 在心跳超时前仍维持 `authenticated = true` 假在线状态；同时离线期间重连触发 pre-101 `onerror + onclose(1006)` 会将有效 Token 写入 `rejectedToken` 并累计 `authFailures` 直至 `maxAuthFailures` 后永久停止重连。
   - **修复函数**：
     - 新增 `isBrowserOffline()`、`WSClient.bindNetworkListeners()`、`WSClient.unbindNetworkListeners()`：监听 `window` 的 `offline`/`online` 事件；离线时主动释放当前连接并触发 `close`，使发送队列稳定暂存待发消息；恢复联网时立即清理离线导致的阻断标记并重置退避上限发起重连。
     - 调整 `WSClient.attempt()` 与 `socket.onclose`：当 `navigator.onLine === false` 时不将连接失败计入 `rejectedToken` / `authFailures`。
     - 新增 `WSClient.clearAuthRejection()` 与 `WSClient.clearAuthRejectionAndRetry()`：供认证层在凭证校验/轮换后解除退避阻断并恢复重连。

2. **`frontend/src/stores/auth.ts`**
   - **问题**：`WSClient` 因瞬断触发 `transport` 错误或 Axios 触发 Refresh Token 轮换（`onTokensRotated`）后，若 `WSClient` 正处于退避或已停止重连状态，未主动唤醒重连；若瞬断发生时 `navigator.onLine` 仍为 `true`，`rejectedToken` 可能阻断同一有效 Token 重试。
   - **修复函数**：
     - 新增 `probeAuthOnWSTransportError()`：在 `client.on('error')` 收到 `transport` 错误且在线时通过 `userApi.getMe()` 校验当前凭证（`200` 则调用 `clearAuthRejectionAndRetry()`；`401` 交由 Axios 单飞刷新；网络/`5xx` 调用 `clearAuthRejection()` 保留退避而不误判 Token 失效）。
     - 在 `setAuthLifecycleCallbacks.onTokensRotated()` 中补充：刷新得到新 Token 后若存在已绑定且未连接的 `WSClient`，调用 `clearAuthRejectionAndRetry()` 立即以新凭证建连。

## 四、跨归属问题与遗留观察

- **Stage 8 遗留 NIT（`frontend/src/layouts/AppLayout.vue`）**：`AppLayout.vue` 第 13 行调用 `useChatRealtime()` 未传 `{ trackChatVisibility: false }`，导致 `AppLayout` 与 `ChatView` 均注册 `visibilitychange` / `focus` 监听并在页面聚焦时重复触发 `markRead`。本阶段按约定未修改 `AppLayout.vue`，仅记作跨归属观察（后端 `msg.read_ack` / `POST /read` 幂等，未阻断联调路径）。
- **`frontend/vite.config.ts` 本地端口/后端源适配**：当前基线 `vite.config.ts` 写死端口 `5173` 与后端 `127.0.0.1:8080`。联调期间本地临时改用 `IM_VITE_PORT` / `IM_BACKEND_ORIGIN` 启动隔离栈，交付前已按约束还原 `vite.config.ts`，待 `admin-qa` 统一提交通用环境变量改动。
- **`frontend/package.json` 未含 Playwright 依赖**：按约束未修改 `package.json` / `package-lock.json`；E2E 脚本 `frontend/e2e/reconnect/reconnect.e2e.mjs` 优先尝试常规 `import('playwright')`，若项目未安装则自动回退读取 `PW_PATH`（默认 `/tmp/pw-stage9/node_modules/playwright/index.mjs`）。

## 五、自检命令与退出码

| 检查项 | 执行命令 | 退出码 | 结果摘要 |
| --- | --- | --- | --- |
| 前端类型检查 | `cd frontend && npm run typecheck` | `0` | `vue-tsc --noEmit` 通过 |
| 前端单测 | `cd frontend && npm test` | `0` | 4 个测试文件、25/25 用例全部通过 |
| 后端 WS / MQ 单测 | `cd backend && go test -p 1 ./internal/ws ./internal/mq` | `0` | `internal/ws`、`internal/mq` 全部通过 |
| 真实浏览器全链路 E2E | `PW_BROWSERS=chromium,webkit IM_FRONTEND_ORIGIN=http://127.0.0.1:5176 node frontend/e2e/reconnect/reconnect.e2e.mjs` | `0` | Chromium 场景 1–6 + WebKit 核心离线补齐与刷新持久化全部通过 |

## 六、联调截图

- 接收方断网恢复后离线消息与撤回同步截图：`docs/stage9/assets/reconnect/01-offline-sync-chromium.png`
- 刷新页面持久化及未读数同步截图：`docs/stage9/assets/reconnect/02-reload-persistence-chromium.png`
