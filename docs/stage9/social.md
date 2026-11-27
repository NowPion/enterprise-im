# 第九阶段联调报告：注册 / 多端登录 / 加好友（stage9-social）

## 1. 隔离联调环境

- **分支**：`agent/stage9-social`（基于 `main` `b2e4cf1`）
- **后端服务**：`SERVER_ADDR=:8081`、`MYSQL_DATABASE=im_stage9_social`、`REDIS_DB=1`、`SNOWFLAKE_NODE_ID=11`、`CORS_ALLOWED_ORIGINS=http://localhost:5174,http://127.0.0.1:5174`
- **前端服务**：`IM_VITE_PORT=5174`、`IM_BACKEND_ORIGIN=http://127.0.0.1:8081`（实测验证 `http://127.0.0.1:5174` 与 `http://localhost:5174` 双源访问，联调结束后已还原未授权本地修改的 `frontend/vite.config.ts`）
- **数据库初始化**：使用临时替换库名后的 SQL 副本导入 `im_stage9_social`（共 12 张表与默认管理员种子 `admin`）后立即删除临时文件，未修改冻结文件 `deploy/mysql/init.sql`。
- **真实依赖**：真实 MySQL 8.0（3306 `mysql8`）、真实 Redis 7（6379 `my-redis` DB 1）、真实 Go 后端与 Redis Streams 消费者组，前端无任何 API Mock。

## 2. 覆盖场景与实测结果

自包含 Playwright 用例位于 [frontend/e2e/social/](../../frontend/e2e/social/)，已在真实浏览器引擎上跑通：

| 序号 | 场景 | Chromium | WebKit (`@core`) | 验证要点 |
| --- | --- | --- | --- | --- |
| 1 | 多账号注册、重复用户名拦截、登录、退出、再登录 | 通过 | 通过 | `/api/v1/auth/register` 返回 `201` 与字符串十进制 ID；重复用户名返回 `409 / 11007`；`login` 发送 `device_type="web"` 与 `localStorage.im_device_id`；当前设备 `logout` 后旧 `access_token`（`401`）与 `refresh_token`（`401/403`）立即失效；再次登录复用 `im_device_id` 并签发新 `session_id` |
| 2 | 多端登录、单飞 Refresh、改密与全端下线失效 | 通过 | — | 同账号双浏览器上下文（不同 `device_id` / `session_id`）同时保持有效会话；同设备双标签页共享 `localStorage` 并发遭遇 `401 / 11001` 时通过 `im_auth_refresh_lock` 仅触发 1 次 `POST /api/v1/auth/refresh`，旧 `refresh_token` 轮换失效（`401 / 11003`），另一设备会话不受影响；修改密码后返回 `revoked_count >= 2`，全部设备旧 Token 失效且旧密码无法登录；`logout(scope="all")` 吊销全端会话 |
| 3 | 搜人、好友申请全流程、备注、删除好友 | 通过 | 通过 | 按用户名前缀搜索并展示实时在线状态；发送申请、重复申请幂等提示（`409 / 12004`）、申请人撤销（`canceled`）、收件人拒绝（`rejected`）、收件人同意（`accepted`，返回正整数 `conversation_id`）；双向好友各自独立维护备注；删除好友后列表移除且刷新仍不恢复 |
| 4 | 页面刷新持久化与个人资料/头像 | 通过 | 通过 | 上传真实 PNG 头像（`POST /api/v1/me/avatar`）并修改昵称/个性签名；`page.reload()` 后从 `localStorage.im_auth_tokens` 与 `im_device_id` 自动恢复登录态，头像、资料、好友列表与备注完整保留 |
| 5 | 共享 `WSClient` 真实握手 | 通过 | 通过 | 登录后及页面刷新后均自动建立 `/ws?access_token=...` 连接，观察到客户端发送 `ping` 帧、服务端回发 `pong` 帧，`ProfileSettingsDialog` 展示「实时通道：已连接」 |

### 实测截图

- 联系人好友详情与独立备注/在线状态：[contacts-friend-detail.png](./assets/social/contacts-friend-detail.png)
- 头像上传后联系人列表持久化视图：[profile-settings.png](./assets/social/profile-settings.png)

## 3. 联调发现并修复的缺陷（归属本阶段）

1. **`frontend/src/stores/auth.ts`：`initAuth()` 捕获非鉴权异常时误清 `localStorage.im_auth_tokens`**
   - **现象**：同设备多标签页并发刷新/重载时，若某标签页在页面跳转/重载过程中中止 `GET /api/v1/me` 请求，`initAuth()` 的 `catch` 分支无条件执行 `clearLocalSession()`，进而调用 `clearStoredTokens()` 清空共享的 `localStorage.im_auth_tokens`，导致另一标签页后续并发请求丢失 `Authorization` 头并被踢回 `/login`。
   - **修复**：在 `frontend/src/stores/auth.ts` 的 `initAuth()` 异常分支中，仅当确认是服务端鉴权失败（`isApiError(err)` 且 `httpStatus` 为 `401` / `403` 或业务码在 `11001..11005`）时才调用 `clearLocalSession()`；请求取消或瞬时传输异常仅清理当前标签页内存态与 WS 连接，不误删同设备其他标签页的持久化凭证。

## 4. 自检命令与退出码

| 命令 | 退出码 | 结果摘要 |
| --- | --- | --- |
| `curl -sS -w "\nHTTP %{http_code}\n" http://127.0.0.1:8081/healthz` | `0` | `HTTP 200`，`code=0`，`mysql="ok"`，`redis="ok"` |
| `cd frontend/e2e/social && SAVE_STAGE9_SHOTS=1 npx playwright test --project=chromium` | `0` | `3 passed (14.5s)`（场景 1–5 全量通过，零 console error / pageerror / 5xx） |
| `cd frontend/e2e/social && npx playwright test --project=webkit --grep @core` | `0` | `2 passed (12.5s)`（WebKit 复跑注册+登录+WS握手+好友申请+刷新持久化+删好友全路径通过） |
| `cd frontend/e2e/social && E2E_BASE_URL=http://localhost:5174 npx playwright test --project=chromium --grep @core` | `0` | `2 passed (9.9s)`（验证 `http://localhost:5174` CORS 与同源代理均通过） |
| `cd frontend && npm run typecheck && npm test` | `0` | `vue-tsc --noEmit` 0 错误；Vitest `4 passed (4)`, `25 passed (25)` |
| `cd frontend/e2e/social && npx tsc --noEmit` | `0` | E2E 用例 TypeScript 校验 0 错误 |

## 5. 跨归属问题（仅记录，未越界修改）

1. **好友申请/同意/拒绝实时 `system.notice` 未接入服务端事件发布（`backend/cmd/server/main.go`、`backend/internal/service/friend_request_service.go`、`backend/internal/mq/`）**
   - **现状**：`backend/internal/mq/handlers.go` 已实现 `KindFriend`（生成 `friend.requested`、`friend.accepted`、`friend.rejected` 的 `system.notice` 帧），且 `frontend/src/stores/contact.ts` 已监听这些事件自动刷新列表；但 `backend/cmd/server/main.go` 在初始化 `FriendRequestService`（第 200 行）时未传入 `mqProducer`（第 218 行构造）或 `hub`，`FriendRequestService` 也未调用事件发布。
   - **影响**：双方同时在线打开联系人页时，发起/同意/拒绝好友申请后对端不会实时收到 `system.notice` 自动刷新，需要点击「刷新」按钮或刷新页面拉取最新状态。因涉及共享入口 `backend/cmd/server/main.go` 与 MQ 归属，本阶段未跨界修改。
2. **WS 建连 HTTP 401 阶段失效时的错误分类（`frontend/src/utils/wsClient.ts`）**
   - **现状**：当 `/ws?access_token=...` 因 Access Token 过期在 HTTP 101 升级前被拒绝时，浏览器 `WebSocket` 触发 `onerror`，`wsClient.ts` 抛出 `{ kind: 'transport' }` 并以 close code `1006` 关闭，而 `auth.ts` 仅监听 `err.kind === 'frame'`（`18001`）与 `err.kind === 'protocol'` 触发静默刷新。在常规 HTTP + WS 并行路径中，HTTP 401 拦截器会完成单飞刷新，`wsClient` 下次重连自动读取新 Token；若长连接空闲过期且无并发 HTTP 请求时仅靠 WS 报错可能不会主动触发 Refresh。因 `wsClient.ts` 属禁止修改文件，仅作记录。
3. **已知第八阶段遗留 NIT（`frontend/src/layouts/AppLayout.vue`）**
   - `AppLayout.vue` 调用 `useChatRealtime()` 未传 `{ trackChatVisibility: false }`，本阶段未阻断联调路径，按约束未修改。
