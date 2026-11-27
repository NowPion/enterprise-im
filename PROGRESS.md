# Enterprise IM 开发进度与阶段自检报告

**第五阶段后端 Hub 已在 `agent/stage5-ws` 实现，等待 live 联调和整合审阅。** 基线为 `main` 的 `d87ba29`。

---

## 一、阶段进度总览

依据 [PROJECT_GOALS.md](PROJECT_GOALS.md) 第九节严格开发阶段划分：

| 阶段 | 状态 | 基线 / 交付物摘要 |
| --- | --- | --- |
| 第一阶段：需求分析与技术架构设计 | **已合入 `main`**（`357340b`） | 已完成 `.gitignore`、[docs/architecture.md](docs/architecture.md) 与 [docs/api.md](docs/api.md)，并正式合入 `main` 分支（`Merge branch 'agent/stage1-architecture'`） |
| 第二阶段：数据库设计与 ER 图 | **已合入 `main`**（`78e7abe`） | 已完成 [deploy/mysql/init.sql](deploy/mysql/init.sql) 与 [docs/er-diagram.md](docs/er-diagram.md)，完成真实 MySQL 8.0.44 导入与 Mermaid ER 图渲染校验后正式合入 `main`（`Merge branch 'agent/stage2-database'`） |
| 第三阶段：后端基础工程搭建 | **已合入 `main`**（`f5ea066`） | 已完成 `backend/` 分层骨架、Viper 配置与环境变量覆盖、脱敏 `log/slog` 访问日志、全量 69 个冻结错误码与统一信封、Snowflake 生成器、12 张表 GORM 模型、真实 MySQL 8.0 / Redis 7 初始化、`/healthz` 实探与 5 步优雅关闭，已正式合入 `main`（`Merge branch 'agent/stage3-backend'`） |
| 第四阶段：用户认证与好友管理 | **已合入 `main`**（`d87ba29`） | 认证、用户资料与搜索、好友申请及好友关系 HTTP 接线 |
| 第五阶段：WebSocket 实时通信 | **后端工作树待整合** | 64 分片 Hub、JWT 握手与逐帧鉴权、心跳与多端租约、好友在线状态、会话撤销及优雅停机 |

### 第五阶段后端 Hub · 2026-11-26

- 服务入口挂载 `GET /ws`。查询参数和 Bearer 头共用第四阶段的 Access JWT、MySQL 用户与设备会话、Redis JTI 黑名单校验。握手拒绝发 `18001` 并包含原认证原因，每个已连接客户端的入站帧也重新校验。登出、改密和同设备重新登录按 session 路由全集关闭命中的标签页。刷新提交后按旧 JTI 精确关闭，允许新的同会话连接继续工作。
- 每条连接独占读写泵，发送队列容量为 256。读写与网络投递在分片锁外执行。心跳只由有效的 JSON `ping` 续租，`pong` 回传 `nonce`、服务端毫秒时间和数据库 session 到期时间。心跳超时走统一注销。停机发 `server.draining` 后以 `1000` 关闭并等待注销完成。
- Redis Lua 原子维护冻结的四类键。默认连接租约和用户连接数 TTL 为 75 秒，用户和 session 路由 TTL 为 90 秒。`WS_HEARTBEAT_TIMEOUT` 可覆盖连接租期，路由比租期多 15 秒。用户计数按有效连接计算，`device_count` 按不同 `device_id` 计算。同设备两个标签页占两条连接、一个设备。Pub/Sub 向各节点的在线好友和用户自身投递状态。最后一条租约消失后更新 `last_online_at`，后台按租约到期索引回收异常退出节点的连接，并补写待落库的离线时间。
- 信封、未知类型和超限分别返回 `18002`、`18003`、`10002`。对已知的 `msg.send`、`msg.delivered_ack`、`msg.read_ack`、`msg.recall` 选用冻结码 `18004`，错误说明为「本阶段不处理发送/回执/撤回」，`retryable=false`。返回该错误时消息、回执和序号均保持原状态。
- 在 `backend` 执行 `go vet ./...`、`go build ./...`、`go build -o /tmp/stage5-ws-verified-server ./cmd/server`、`go test -count=1 -race ./internal/ws ./internal/repository` 和 `go test -count=1 -race ./...` 均退出 0。全量覆盖 15 个包。包内测试验证分片并发、满队列隔离、轮换目标、协议拒绝，以及真实 WebSocket 握手的错误帧、关闭码和凭证脱敏。
- 本环境缺少 MySQL/Redis 运行口令，Docker socket 拒绝访问。`TestLiveConnectionLeaseOwnershipAndTTL` 和 `TestLiveReapLastLeaseAndRetryOfflineTimestamp` 条件跳过，真实 MySQL 8.0 / Redis 7 联调待验。外层 Git 元数据目录拒绝创建 `index.lock`，本路改动保留在工作树，未合入 `main`。

---

## 二、第四阶段已完成项与整合接线说明

| 交付物 | 状态 | 覆盖内容摘要 |
| --- | --- | --- |
| 认证、会话与 JWT 中间件 | 已完成 | [backend/internal/config/jwt.go](backend/internal/config/jwt.go)、[backend/internal/pkg/jwt/jwt.go](backend/internal/pkg/jwt/jwt.go)、[backend/internal/pkg/password/password.go](backend/internal/pkg/password/password.go)、[backend/internal/middleware/jwt.go](backend/internal/middleware/jwt.go)、[backend/internal/repository/auth_repo.go](backend/internal/repository/auth_repo.go)、[backend/internal/repository/session_repo.go](backend/internal/repository/session_repo.go)、[backend/internal/service/auth_service.go](backend/internal/service/auth_service.go)、[backend/internal/controller/auth_controller.go](backend/internal/controller/auth_controller.go)、[backend/internal/api/auth_routes.go](backend/internal/api/auth_routes.go)：实现 `POST /api/v1/auth/{register,login,refresh,logout}` 与 `PUT /api/v1/me/password`，支持多设备会话上限、同设备替换、Refresh Token SHA-256 摘要轮换与 Redis 并发锁（`11008`）、Access JTI Redis 黑名单及登出/改密事务撤销 |
| 当前用户资料与搜索 | 已完成 | [backend/internal/repository/user_repo.go](backend/internal/repository/user_repo.go)、[backend/internal/service/user_service.go](backend/internal/service/user_service.go)、[backend/internal/controller/user_controller.go](backend/internal/controller/user_controller.go)、[backend/internal/api/user_routes.go](backend/internal/api/user_routes.go)：实现 `GET /api/v1/me`、`PATCH /api/v1/me` 与 `GET /api/v1/users/search`（`username`/`nickname` 精确或字面前缀转义匹配、游标绑定分页、Redis 在线态读取）；并在 `extractAuthIdentity` 对接 `middleware.GetAuthContext(c)` 以在真实 `JWTAuth` 下准确输出 `session_id` 与 `device_id` |
| 好友申请工作流 | 已完成 | [backend/internal/repository/friend_request_repo.go](backend/internal/repository/friend_request_repo.go)、[backend/internal/repository/friend_request_relation_writer.go](backend/internal/repository/friend_request_relation_writer.go)、[backend/internal/service/friend_request_service.go](backend/internal/service/friend_request_service.go)、[backend/internal/controller/friend_request_controller.go](backend/internal/controller/friend_request_controller.go)、[backend/internal/api/friend_request_routes.go](backend/internal/api/friend_request_routes.go)：实现 `POST/GET /api/v1/friend-requests` 与 `POST /api/v1/friend-requests/:id/{accept,reject,cancel}`；创建与接受按用户 ID 升序加锁防死锁，接受时在单事务内建立/恢复双向好友与唯一单聊会话并追加 `conv:lock:*` / `conv:seq:*` 有序 Snowflake 系统消息 |
| 好友关系管理与 `RelationWriter` 适配 | 已完成 | [backend/internal/repository/friendship_repo.go](backend/internal/repository/friendship_repo.go)、[backend/internal/repository/friendship_relation_writer.go](backend/internal/repository/friendship_relation_writer.go)、[backend/internal/service/friend_service.go](backend/internal/service/friend_service.go)、[backend/internal/controller/friend_controller.go](backend/internal/controller/friend_controller.go)、[backend/internal/api/friend_routes.go](backend/internal/api/friend_routes.go)：实现 `GET /api/v1/friends`（零 N+1 批量查用户、单聊会话与多设备在线态）、`PATCH /api/v1/friends/:friend_id`（单向备注）与 `DELETE /api/v1/friends/:friend_id`（双向软删并隐藏双方会话、保留历史消息）；通过薄适配器使 `*repository.FriendshipRepository` 实现 `repository.RelationWriter` 并在 `main.go` 显式注入 `NewFriendRequestService` |
| HTTP 路由装配与服务端入口 | 已完成 | [backend/internal/api/router.go](backend/internal/api/router.go)、[backend/cmd/server/main.go](backend/cmd/server/main.go)、[.env.example](.env.example)：保持 `NewRouter(cfg, log, healthCtrl)` 原签名兼容各分包测试，新增 `Stage4Dependencies`、`RegisterStage4Routes` 与 `NewRouterWithStage4`；`cmd/server/main.go` 同步加载 `config.Load` 与 `config.LoadAuthConfig`（生产模式拒绝占位符 `JWT_SECRET`）、初始化 Snowflake 与 `pkgjwt.Manager`、装配同一真实 `middleware.JWTAuth`、挂接带结构化日志的 `service.NoopSessionRevokeHook` 并保留 `GET /healthz`；`.env.example` 仅添加无真实密钥的 JWT/Auth 占位环境变量 |
| 四路合入最小编译对接 | 已完成 | 消除四路并行合入的包内符号重声明：`user_service.go` 重命名 `validateUserNickname`、`friend_controller.go` 重命名 `extractFriendAuthUserID`、`friend_service_test.go` 重命名 `inMemoryFriendsStore` / `newInMemoryFriendsStore`，未改动既有业务规则与冻结表结构 |

### 活跃 Worktree / 分支状态

| 项目 | 当前值 |
| --- | --- |
| 主仓库路径 | `/Users/niezidong/Projects/go`（`main` 基线 `f5ea066`） |
| 当前 Worktree | `/Users/niezidong/.herdr/worktrees/go/agent-stage4-auth-integrated` |
| 当前分支 | `agent/stage4-auth-integrated` |
| 职责边界 | 仅完成第四阶段四路合入与 HTTP 整合接线；未修改已冻结契约文件（`PROJECT_GOALS.md`、`docs/*`、`deploy/mysql/init.sql`）与全量 `errcode` 表，未实现第五阶段 WS Hub / 群聊 / 上传 / 后台 / 消息收发，未执行 `merge` 到 `main` 或 `push` |

---

## 三、本阶段关键 Commit 摘要（自 `main` 基线 `f5ea066` 起）

1. `f5ea066` (`main`) — `Merge branch 'agent/stage3-backend'`（第一至三阶段交付已合入 `main`）
2. `2206c88` / `77b0cb3` / `046dd61` / `8e17a43`（`agent/stage4-auth`）— JWT 签发校验、密码哈希、会话管理、认证接口与实时测试
3. `d597706`（合入 `agent/stage4-user`：`2c36907` / `8b53763` / `ea40567` / `e7b9750`）— 用户资料查询更新与前缀搜索
4. `3f108c1`（合入 `agent/stage4-friend-request`：`7b15905` / `3d4daac` / `f68ebcd`）— 好友申请、分布式 seq 分配与原子接受流程
5. `d46050a`（合入 `agent/stage4-friends`：`e41eefb` / `33d09a8` / `9e82dba`）— 好友列表、备注与双向删除
6. `2c48752` — `feat(stage4): wire auth, user, friend-request, and friend routes in router and server`（路由与入口装配、RelationWriter 薄适配器、编译对接及端到端接线测试）
7. 本收口提交 — `docs(stage4): update PROGRESS.md for integrated stage four verification`

---

## 四、编译与自动化测试验证结果（不含任何账密或密钥）

> 说明：当前构建与测试环境未注入 `MYSQL_PASSWORD` / `REDIS_PASSWORD`（以及 `IM_MYSQL_PASSWORD` / `IM_REDIS_PASSWORD`），因此依赖外部容器口令的 live MySQL/Redis 用例按设计前置条件自动 `SKIP`；其余全部静态检查、编译、竞态单元测试与全链路真实 `JWTAuth` HTTP 集成测试均已执行并通过。所有文档、配置与提交均不包含任何真实口令或 JWT 密钥。

### 1. 静态检查与编译

- `cd backend && go vet ./...`：退出码 **`0`**。
- `cd backend && go build -o /tmp/enterprise-im-server ./cmd/server`：退出码 **`0`**（验证后已删除 `/tmp/enterprise-im-server`）。

### 2. 全量竞态测试（`cd backend && go test -count=1 -race -v ./...`）

全部 **14** 个 Go 包均退出码 **`0`**（**0 失败**）：

- **通过包清单**：
  - `cmd/server`：`TestOrderedGracefulShutdownDrainsInFlightAndRunsStepsInOrder`、`TestLoggedNoopSessionRevokeHook`（校验无 WS Hub 时调用空钩子并记录脱敏日志）、`TestBuildRouterMountsHealthzAndAllStage4Routes`（校验 `/healthz` 与全部 16 条 Stage 4 REST 路由注册及未鉴权 401/11001 拦截）均通过；
  - `internal/api`：`TestRegisterAuthRoutesMountsExpectedPathsAndGuardsMiddleware`、`TestRegisterFriendRequestRoutesMountTargetsAndJWTStub`、`TestRegisterFriendRoutesFailClosedWithoutJWTAndDispatchWithStub` 均通过；
  - `internal/config`：YAML 加载、ENV 覆盖、生产模式占位符 `JWT_SECRET` 拒绝与边界校验全部通过；
  - `internal/controller`：`TestAuthControllerMaxBytesAndHTTPEnvelope`、`TestFriendControllerValidationAndResponses`、`TestFriendRequestControllerEndpointsAndEnvelopes`、`TestUserControllerHandlers` 全部通过；
  - `internal/middleware`：`TestJWTAuthMiddlewareAllPaths` 与 `TestMiddlewareChainLoggingRedactionAndRecovery` 通过；
  - `internal/model`、`internal/pkg/errcode`、`internal/pkg/jwt`、`internal/pkg/logger`、`internal/pkg/password`、`internal/pkg/snowflake`：全部单元测试通过；
  - `internal/repository`：`TestDirectKeyCanonicalOrder`、`TestRedisSeqAllocatorAndPresenceFailFastOnInvalidClient`、`TestFriendshipRelationWriterGuards`、`TestFriendshipHelpersAndNilGuards`、`TestStartupConnectivityFailuresFailFast`、`TestEscapeLikeLiteralAndGuards` 通过；
  - `internal/service`：认证、好友申请（并发防重与事务回滚）、好友列表/备注/删除（零 N+1 与并发/降级）、健康探活与用户搜索/更新全部 15 个单元测试通过；
  - `tests`：
    - `TestStage4IntegratedRouterRealJWTAuthAndCrossModuleFlow` 通过：在同一 `api.NewRouterWithStage4` 引擎上以真实 `middleware.JWTAuth` 串联 `/healthz` → 注册 → 登录 → `GET /api/v1/me`（校验 `session_id` / `device_id`）→ `PATCH /api/v1/me` → 搜索 → 发起/查看/接受好友申请 → 好友列表/改备注/删好友 → `POST /api/v1/auth/logout` 与 `PUT /api/v1/me/password` 吊销后旧 Token 遭黑名单拦截（`11002`）→ 日志零敏感泄露；
    - `TestHealthzFailurePathsWithoutMockingRedisClient`、`TestStage4AuthRedisUnavailableFailsClosedWithoutMocking`、`TestStage4FriendRequestHTTPFlowAndLogRedaction`、`TestStage4FriendsHTTPContractAndAuthStub`、`TestStage4UserRoutesAndEnvelopeContract` 均通过。
- **条件跳过清单（因未设置 `MYSQL_PASSWORD` / `REDIS_PASSWORD`）**：
  - `internal/repository`（5 项）：`TestLiveFriendRequestRepositoryIntegration`、`TestFriendshipRepositoryLiveMySQLAndRedis`、`TestFriendshipRepositoryLiveTxCommitAndDeleteRestore`、`TestLiveMySQLAndRedisModelsIntegration`、`TestLiveUserRepositoryAndPresenceIntegration`；
  - `tests`（6 项）：`TestHealthzLiveMySQLAndRedis`、`TestStage4AuthLiveMySQLAndRedisIntegration`、`TestStage4FriendRequestLiveMySQLAndRedisIntegration`、`TestStage4FriendsLiveMySQLAndRedisHTTP`、`TestStage4UserLiveMySQLAndRedis`、`TestStage4IntegratedLiveMySQLAndRedis`。

---

## 五、后续衔接注意事项

1. **审阅与合入前 live 复测**：可在配置本地容器 `MYSQL_PASSWORD` / `REDIS_PASSWORD` 环境变量后执行 `cd backend && go test -count=1 -race -v ./...`，复核上述 11 个 live MySQL/Redis 集成用例及实际二进制 `/healthz` + Stage 4 真实请求，再由主仓库审阅合入 `main`。
2. **第五阶段接入点**：后续阶段实现 WebSocket Hub（`/ws`）、Redis Streams 消费组后，只需在 [backend/cmd/server/main.go](backend/cmd/server/main.go) 将 `newLoggedNoopSessionRevokeHook` 与 `ShutdownHooks.{NotifyAndCloseWS,DrainMQ}` 替换为真实 Hub/MQ 实现即可。

---

## 六、第九阶段进展（联调工装与管理后台监控 / 禁用踢下线）

### `agent/stage9-admin-qa` · 2026-11-27

- **公共联调工装**：
  - 更新 [frontend/vite.config.ts](frontend/vite.config.ts) 读取 `IM_BACKEND_ORIGIN` 与 `IM_VITE_PORT` 环境变量代理 `/api`、`/uploads` 与 `/ws`（保持默认 `127.0.0.1:8080` / `5173` 行为不变）。
  - 在 [frontend/package.json](frontend/package.json) 引入 `@playwright/test`，新增 [frontend/playwright.config.ts](frontend/playwright.config.ts)（收口 `social`、`chat`、`reconnect`、`admin` 与 `admin-firefox` 项目）、`frontend/e2e/helpers/`、`frontend/e2e/fixtures/imFixtures.ts` 以及隔离栈启动脚本 [scripts/stage9-stack.sh](scripts/stage9-stack.sh)。
- **后端 Admin API 补齐与接线**：
  - 新增 [backend/internal/repository/admin_repo.go](backend/internal/repository/admin_repo.go)、[backend/internal/service/admin_service.go](backend/internal/service/admin_service.go)、[backend/internal/controller/admin_controller.go](backend/internal/controller/admin_controller.go)、[backend/internal/api/admin_routes.go](backend/internal/api/admin_routes.go) 并在 [backend/cmd/server/main.go](backend/cmd/server/main.go) 完成路由注册。
  - 实现并接通 `docs/api.md` 约定的全部 11 个 `/api/v1/admin/*` 接口：用户列表/详情、禁用/启用（含唯一管理员保护 `17004`、会话撤销、JTI 黑名单与 `Hub.RevokeSessions` 实时踢下线）、单端/全端 `kick`、群监管列表/详情/强制解散、消息审计查询、运行监控概览（真实 Redis 在线态/Streams + MySQL + Go Runtime）、Streams 消费组指标与 MySQL/Redis 依赖健康探活。
- **实测验证结果**：
  - `cd backend && go vet ./... && go build ./... && go test -p 1 -count=1 -race ./internal/repository ./internal/service ./internal/controller ./internal/api ./cmd/server`：退出码 **`0`**。
  - `cd frontend && npm run typecheck && npm test`：退出码 **`0`**（`25/25` 通过）。
  - `cd frontend && IM_VITE_PORT=5177 IM_BACKEND_ORIGIN=http://127.0.0.1:8084 npm run test:e2e:admin`：退出码 **`0`**（Chromium 4 用例 + Firefox 2 用例共 **6/6** 通过），详细报告与截图见 [docs/stage9/admin.md](docs/stage9/admin.md)。
