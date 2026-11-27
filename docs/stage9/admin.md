# 第九阶段报告：联调工装与管理后台全链路（stage9-admin-qa）

## 一、交付摘要

- **分支**：`agent/stage9-admin-qa`（基于 `main` 的 `b2e4cf1`）
- **隔离联调环境**：
  - 后端监听：`:8084`（`SERVER_ADDR=:8084`）
  - Vite 前端：`127.0.0.1:5177`（`IM_VITE_PORT=5177`、`IM_BACKEND_ORIGIN=http://127.0.0.1:8084`）
  - MySQL 隔离库：`im_stage9_admin`（由 `deploy/mysql/init.sql` 临时副本替换库名导入，已清理临时文件）
  - Redis 隔离 DB：`4`（`REDIS_DB=4`）
  - Snowflake 节点：`14`（`SNOWFLAKE_NODE_ID=14`）
  - CORS：`http://localhost:5177,http://127.0.0.1:5177`

## 二、Playwright QA 公共工装与安装说明

### 1. 已交付文件
- [frontend/vite.config.ts](../../frontend/vite.config.ts)：支持环境变量 `IM_BACKEND_ORIGIN`（默认 `http://127.0.0.1:8080`）与 `IM_VITE_PORT`（默认 `5173`），统一反代 `/api`、`/uploads` 与 `/ws`（`ws: true`）。
- [frontend/package.json](../../frontend/package.json)：新增 `@playwright/test`（`^1.65.1`）及脚本 `test:e2e`、`test:e2e:admin`。
- [frontend/playwright.config.ts](../../frontend/playwright.config.ts)：根配置收口 `social`、`chat`、`reconnect`、`admin`（Chromium）及 `admin-firefox`（Firefox 复跑踢下线用例）五个项目。
- `frontend/e2e/helpers/`：`env.ts`、`api.ts`、`auth.ts`
- `frontend/e2e/fixtures/imFixtures.ts`：多上下文账号注册/登录/WS 就绪与自动注销清理 Fixture。
- [scripts/stage9-stack.sh](../../scripts/stage9-stack.sh)：基于环境变量启动隔离后端与 Vite 的示例脚本（不含任何明文口令）。

### 2. 安装与运行说明
```bash
cd frontend
npm ci
npx playwright install chromium firefox

# 针对隔离后端与 Vite 运行管理后台 E2E（示例端口 5177 / 8084）
IM_VITE_PORT=5177 IM_BACKEND_ORIGIN=http://127.0.0.1:8084 npm run test:e2e:admin

# 运行全量 Playwright 工程（含 social/chat/reconnect/admin）
IM_VITE_PORT=5177 IM_BACKEND_ORIGIN=http://127.0.0.1:8084 npm run test:e2e
```

## 三、后端 Admin API 实现与完成度

依据 `docs/api.md` 补齐了全部 11 个管理后台 REST 接口，挂载于 `/api/v1/admin/*`，统一受 `jwtAuth` + `RequirePlatformAdmin()`（`role == admin`，非管理员返回 HTTP 403 / `17001`）保护，零 Mock 数据：

| 接口 | 状态 | 实现说明 |
| --- | --- | --- |
| `GET /api/v1/admin/users` | 已完成并实测 | 支持 `q` / `status` / `role` 过滤与游标分页，批量统计活跃 `user_sessions` 并从 Redis Pipeline 读取 `online` 状态 |
| `GET /api/v1/admin/users/{user_id}` | 已完成并实测 | 返回用户详情、设备会话列表、好友数、加群数及近期 `audit_logs` 摘要 |
| `PATCH /api/v1/admin/users/{user_id}/status` | 已完成并实测 | 事务内校验唯一管理员保护（`17004`）、更新 `users.status`、撤销全部活跃会话并写 `audit_logs`；提交后写入 Redis JTI 黑名单并调用 `Hub.RevokeSessions(..., "account.disabled")` 实时踢下线，返回 `revoked_session_count` 与 `kicked_connection_count` |
| `POST /api/v1/admin/users/{user_id}/kick` | 已完成并实测 | 支持按 `session_id` 单端踢出或全部踢出，撤销会话、拉黑 JTI 并调用 `Hub.RevokeSessions(..., "account.kicked")` 实时关闭对应 WS 连接 |
| `GET /api/v1/admin/groups` | 已完成并实测 | 支持 `q` / `status` 过滤与游标分页，批量关联 `conversations`（`conversation_id`、`last_seq`）、成员数与群主信息 |
| `GET /api/v1/admin/groups/{group_id}` | 已完成并实测 | 返回群详情、含在线态的群成员列表与系统/审计事件摘要 |
| `DELETE /api/v1/admin/groups/{group_id}` | 已完成并实测 | 事务内标记 `disbanded`、分配会话 `seq` 写入解散系统消息并写 `audit_logs`，提交后向在线成员广播 `msg.push` 与 `system.notice(group.disbanded)` |
| `GET /api/v1/admin/messages/audit` | 已完成并实测 | 支持 `conversation_id`、`sender_id`、`keyword`、`from`/`to`（含 90 天窗口与非法参数拦截 `17005`）与游标分页 |
| `GET /api/v1/admin/metrics/overview` | 已完成并实测 | 实时读取 Redis `im:presence:active` + `im:presence:user:*`（`online_users`、`ws_connections`）、MySQL 近 1 分钟消息数、`XLEN`/`XPENDING`（`streams`）、`runtime.NumGoroutine()` 与 `MemStats.HeapAlloc` |
| `GET /api/v1/admin/metrics/streams` | 已完成并实测 | 实时读取 `im:stream:messages` 与 `im:stream:events` 下 4 个消费组及 Consumer 的 `last_delivered_id`、`pending_count` 与 `oldest_pending_age` |
| `GET /api/v1/admin/health/dependencies` | 已完成并实测 | 并发探活 MySQL（`SELECT VERSION()`）与 Redis（`INFO server`），记录探测延迟、服务端版本与最近错误时间/摘要；依赖异常时返回 HTTP 503 + `17006` 与诊断数据 |

## 四、自检命令、退出码与覆盖场景

| 检查项 | 执行命令 | 退出码 | 结果摘要 |
| --- | --- | --- | --- |
| 后端静态检查与编译 | `cd backend && go vet ./... && go build ./...` | `0` | 全部包编译通过 |
| 后端单测（含 `-race`） | `cd backend && go test -p 1 -count=1 -race ./internal/repository ./internal/service ./internal/controller ./internal/api ./cmd/server` | `0` | 5 个包全部通过（含新增 `admin_service_test.go`、`admin_controller_test.go`、`admin_routes_test.go`） |
| 前端类型检查 | `cd frontend && npm run typecheck` | `0` | `vue-tsc --noEmit` 零错误 |
| 前端单元测试 | `cd frontend && npm test` | `0` | 4 个测试文件、25 个用例全部通过 |
| Playwright 管理员 E2E（Chromium + Firefox） | `cd frontend && IM_VITE_PORT=5177 IM_BACKEND_ORIGIN=http://127.0.0.1:8084 npm run test:e2e:admin` | `0` | **6/6 通过**（16.2s）：Chromium 4 用例 + Firefox 2 用例 |

### 实测覆盖的真实浏览器场景
1. **权限拦截**：普通用户访问 `/admin/overview` 被前端路由守卫拦截重定向回 `/chat`，直接请求 `/api/v1/admin/metrics/overview` 被后端以 HTTP 403 / `17001` 拒绝（Chromium & Firefox）。
2. **实时监控大盘（Overview）**：普通用户注册登录后保持 WS 在线，管理员登录 `/admin/overview` 查看到真实的 `online_users >= 2`、`ws_connections >= 2`、MySQL `8.0.44` / Redis `7.4.7` 探活 `ok`，以及 `im:stream:messages` / `im:stream:events` 消费组指标（[overview 截图](./assets/admin/admin-overview-live.png)）。
3. **禁用并实时踢下线 + 启用恢复**：管理员在 `/admin/users` 搜索在线目标用户（[users 截图](./assets/admin/admin-users-online.png)）并执行「禁用」，返回撤销 1 个会话、关闭 1 条 WS 连接；被禁用户的浏览器端实时收到下线通知并自动跳转至 `/login`，使用旧 Token 请求 `/api/v1/me` 返回 `401/403`（`11002/11004/11005`），禁用期间重新登录提示「该账号已被禁用」；`/admin/overview` 刷新后在线计数实时回落；管理员执行「启用」后该用户重新登录成功并重建 WS 连接（Chromium & Firefox 双引擎均实测通过）。
4. **单端会话踢出（Kick）**：管理员打开用户详情抽屉，在设备会话列表中踢出活跃会话，用户浏览器端实时掉线跳回 `/login`。
5. **群组监管与消息审计**：通过真实 API 建群并发送群消息后，管理员在 `/admin/groups` 检索群聊、查看群详情与成员列表、执行「强制解散」使状态变为 `disbanded`，并跳转 `/admin/audit` 按 `conversation_id` 与关键词查询到对应真实消息记录。

## 五、残留与跨归属问题

1. **跨归属观察（`backend/internal/ws`）**：`ws.Hub.RevokeSessions` 当前仅返回 `error`，未直接返回实际关闭的本地/跨节点连接数；`admin_service.go` 在调用 `RevokeSessions` 前通过读取 Redis `im:route:session:{session_id}`、`im:route:user:{user_id}` 与 `im:presence:conn:{conn_id}` 统计 `kicked_connection_count`。若后续 `ws` 模块导出带连接计数的撤销方法，可直接由 Hub 返回精确本地关闭数。
2. **跨归属观察（`backend/internal/mq`）**：管理端写操作沿用 `group_service.go` 模式，在 MySQL 业务事务内同步追加 `audit_logs`，确保用户/群详情抽屉与审计页立即可见；若需同步计入 Redis Stream 事件指标，可在事务提交后附带相同 `event_id` 投递 `im:stream:events`（依托 `uk_audit_event_id` 幂等去重）。
3. **第八阶段既有提示**：`frontend/src/layouts/AppLayout.vue` 未向 `useChatRealtime` 传递 `trackChatVisibility: false`，未阻碍管理后台联调路径，按约束保留未修改。
