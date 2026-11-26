# Enterprise IM 开发进度与阶段自检报告

**当前阶段结论：第三阶段（后端基础工程搭建）已完成实测，待审阅合入 `main`**

---

## 一、阶段进度总览

依据 [PROJECT_GOALS.md](PROJECT_GOALS.md) 第九节严格开发阶段划分：

| 阶段 | 状态 | 基线 / 交付物摘要 |
| --- | --- | --- |
| 第一阶段：需求分析与技术架构设计 | **已合入 `main`**（`357340b`） | 已完成 `.gitignore`、[docs/architecture.md](docs/architecture.md) 与 [docs/api.md](docs/api.md)，并正式合入 `main` 分支（`Merge branch 'agent/stage1-architecture'`） |
| 第二阶段：数据库设计与 ER 图 | **已合入 `main`**（`78e7abe`） | 已完成 [deploy/mysql/init.sql](deploy/mysql/init.sql) 与 [docs/er-diagram.md](docs/er-diagram.md)，完成真实 MySQL 8.0.44 导入与 Mermaid ER 图渲染校验后正式合入 `main`（`Merge branch 'agent/stage2-database'`） |
| 第三阶段：后端基础工程搭建 | **已完成（待审阅合并）** | 已完成 `backend/` 分层骨架、Viper 配置与环境变量覆盖、脱敏 `log/slog` 访问日志、全量 69 个冻结错误码与统一信封、Snowflake 生成器、12 张表 GORM 模型、真实 MySQL 8.0 / Redis 7 初始化、`/healthz` 实探与 5 步优雅关闭，并在真实容器依赖下全部实测通过 |
| 第四阶段：用户认证与好友管理 | 未开始 | 本阶段严格止步于基础工程交付，未注册业务路由，未实现认证、业务 REST、WebSocket Hub 或 Redis Streams 消费者 |

---

## 二、第三阶段已完成项与边界说明

| 交付物 | 状态 | 覆盖内容摘要 |
| --- | --- | --- |
| 模块与工程分层（`backend/`） | 已完成 | `go.mod` / `go.sum`（Go 1.26.0 / 工具链 1.26.3）、`cmd/server`、`configs/config.yaml`、`internal/{api,config,controller,service,repository,model,middleware}` 及 `internal/pkg/{errcode,logger,snowflake}`、`tests/`；Controller 仅负责校验调用与响应封装，未提前编写第四阶段及之后的业务路由与 WS Hub |
| 配置管理与模板（[backend/configs/config.yaml](backend/configs/config.yaml)、[.env.example](.env.example)、[Makefile](Makefile)） | 已完成 | `internal/config` 基于 Viper 支持 YAML 加载、`IM_*` 与标准环境变量覆盖、`development`/`production` 模式区分及启动前参数校验；仓库仅保留空口令默认值与无真实密钥的 `.env.example` |
| 结构化日志与脱敏（[backend/internal/pkg/logger/logger.go](backend/internal/pkg/logger/logger.go)） | 已完成 | 使用标准库 `log/slog`（支持 JSON/Text 与级别配置），内建敏感键脱敏（`password`、`token`、`authorization`、`cookie`、`dsn` 等）与 Header / Query 参数脱敏辅助函数 |
| 冻结错误码与统一信封（[backend/internal/pkg/errcode/codes.go](backend/internal/pkg/errcode/codes.go)、[backend/internal/pkg/errcode/response.go](backend/internal/pkg/errcode/response.go)） | 已完成 | 完整集中实现 `docs/api.md` 冻结码表全部 69 个码（`0` 与 `10000`–`19004`），统一返回 `{code, message, data, request_id}` 并回写 `X-Request-ID`，错误信息不泄露路径、密码或 Token |
| 中间件链路（[backend/internal/middleware](backend/internal/middleware)） | 已完成 | 按序装配 `RequestID`、`AccessLogger`（记录 `request_id`、`user_id`、`latency_ms`、结果 `code` 并脱敏认证头）、`Recovery`（Panic 恢复返回 `19004`）、`CORS`（Origin 白名单校验）与 `MaxBodySize`（超限返回 `10002`） |
| 12 表 GORM 模型与 Snowflake（[backend/internal/model](backend/internal/model)、[backend/internal/pkg/snowflake/snowflake.go](backend/internal/pkg/snowflake/snowflake.go)） | 已完成 | 严格对齐 `deploy/mysql/init.sql` 12 张表：`messages.id` 显式配置 `autoIncrement:false` 且 `BeforeCreate` 拦截零值 ID；`assets.storage_key` / `thumbnail_key` 使用 `json:"-"` 禁止对外暴露；`audit_logs` 按 append-only 建模（无 `UpdatedAt`/`DeletedAt` 与 Update/Delete 业务钩子）；Snowflake 线程安全且拒绝超容忍度的时钟回拨 |
| MySQL / Redis 真实连接与 `/healthz`（[backend/internal/repository](backend/internal/repository)、[backend/internal/service/health_service.go](backend/internal/service/health_service.go)、[backend/internal/controller/health_controller.go](backend/internal/controller/health_controller.go)） | 已完成 | 启动即真实 `Ping + SELECT 1` / `PING`，不可用时非 0 退出；`GET /healthz` 免鉴权并发实探 MySQL 与 Redis，成功返回 200 + `code=0` + `{status:"ok", mysql:"ok", redis:"ok", timestamp}`（UTC 毫秒级 RFC 3339） |
| 优雅关闭（[backend/cmd/server/main.go](backend/cmd/server/main.go)） | 已完成 | 监听 `SIGINT`/`SIGTERM`，按序执行：① 停止接入新 HTTP 请求 → ② WS 通知关闭钩子（本阶段无连接空钩子）→ ③ MQ 停止拉取并排空钩子（本阶段无消费者空钩子）→ ④ 限时排空在途 HTTP 请求 → ⑤ 关闭 MySQL 与 Redis 连接池 |

### `/healthz` 依赖失败与双失败错误码选择

- 单项失败：MySQL 探活失败返回 HTTP `503` + 业务码 `19001`（`data.mysql="unavailable"`）；Redis 探活失败返回 HTTP `503` + 业务码 `19002`（`data.redis="unavailable"`）。
- **双依赖同时失败选择**：当 MySQL 与 Redis 同时不可用时，返回 HTTP `503` 与冻结业务码 **`19001`（MySQL 不可用优先）**，同时在 `data` 中如实给出 `{"status":"unavailable","mysql":"unavailable","redis":"unavailable","timestamp":"..."}`。原因：MySQL 是系统持久化事实源且在冻结码表中 `19001` 排位优先于 `19002`，不发明新错误码。

### 活跃 Worktree / 分支状态

| 项目 | 当前值 |
| --- | --- |
| 主仓库路径 | `/Users/niezidong/Projects/go`（`main` 基线 `78e7abe`） |
| 当前 Worktree | `/Users/niezidong/.herdr/worktrees/go/agent-stage3-backend` |
| 当前分支 | `agent/stage3-backend` |
| 职责边界 | 仅交付第三阶段后端基础工程与测试/进度更新；未修改已冻结契约文件（`PROJECT_GOALS.md`、`docs/*`、`deploy/mysql/init.sql`），未实现登录/业务 API/WS Hub/MQ 消费者，未执行 `push` / `merge` |

---

## 三、本阶段 Commit 清单（含前置主干基线）

1. `78e7abe` (`main`) — `Merge branch 'agent/stage2-database'`（第一、二阶段交付已合入 `main`）
2. `62037f6` — `feat(backend): initialize Go module, config loader, and slog logger`
3. `f081880` — `feat(backend): add full frozen errcode table, response envelope, and middlewares`
4. `13335f5` — `feat(backend): add snowflake generator, 12-table GORM models, and MySQL/Redis repositories`
5. `26cb192` — `feat(backend): add /healthz probe, router, cmd/server lifecycle, and integration tests`
6. 收口提交（紧随 `26cb192`）— `docs(backend): update stage three progress and verification report`（`PROGRESS.md`）

---

## 四、真实环境验收实测结果

> 注：所有实测均在本地已运行的 `mysql8`（`mysql:8.0.44`，3306）与 `my-redis`（`redis:7.4.7`，6379）容器上通过环境变量传入连接凭证完成；通过 `deploy/mysql/init.sql` 初始化 `enterprise_im` 12 张表后，集成测试对写表验证全程包裹在 `tx.Rollback()` 回滚事务中，未污染已有库表数据。所有文档、配置与提交均不包含任何数据库或 Redis 口令。

### 1. 编译与自动化测试（`go vet` / `go test -race` / `go build`）

- **静态检查与编译**：`cd backend && go vet ./...` 与 `go build -o /tmp/enterprise-im-server ./cmd/server` 均退出码 `0`（测试后已清理 `/tmp` 临时产物）。
- **全量竞态测试（带真实 MySQL/Redis 环境变量）**：`cd backend && go test -count=1 -race -v ./...` 全部 10 个包通过、**0 跳过、0 失败**：
  - `cmd/server`：`TestOrderedGracefulShutdownDrainsInFlightAndRunsStepsInOrder` 通过（验证在途请求排空、新请求拦截及 5 步关闭顺序日志）；
  - `internal/config`：YAML 加载、ENV 覆盖、非法参数校验、脱敏摘要通过；
  - `internal/middleware`：`X-Request-ID` 同步、`Authorization`/Query 脱敏、Panic 恢复（`19004`）、请求体超限（`10002`）、CORS 校验通过；
  - `internal/model`：12 张表映射、`Message` 非自增校验、`Asset` 内部键不序列化、`AuditLog` 无更新删除字段通过；
  - `internal/pkg/errcode`：全量 69 个冻结错误码与 WS close code 核验通过；
  - `internal/pkg/logger`：敏感属性与认证头脱敏通过；
  - `internal/pkg/snowflake`：32 协程并发唯一性与时钟回拨拒绝通过；
  - `internal/repository`：`TestStartupConnectivityFailuresFailFast` 与 `TestLiveMySQLAndRedisModelsIntegration`（真实连接 MySQL/Redis、校验 `enterprise_im` 12 表与默认 `admin` 种子、事务内验证 Snowflake `Message` 与 `AuditLog` 插入并回滚）通过；
  - `internal/service`：`TestHealthServiceCheckStates` 覆盖健康、单 MySQL 故障（`19001`）、单 Redis 故障（`19002`）及双故障优先（`19001`）分支；
  - `tests`：`TestHealthzLiveMySQLAndRedis` 与 `TestHealthzFailurePathsWithoutMockingRedisClient` 通过。

### 2. 真实进程启动、`GET /healthz` 实探与失败路径验证

- **失败快速退出实测**：将 `REDIS_ADDR` 临时指向不可达端口 `127.0.0.1:16379` 启动编译后的服务二进制，进程立即失败并以非 `0` 退出（日志输出 `init redis: verify redis startup connectivity: ... connection refused`）。
- **`GET /healthz` 正常实探（监听 `:18080`）**：
  - HTTP 状态：`200 OK`
  - 响应头：`X-Request-Id: r-917fb2b96aa1ccde`、`Content-Type: application/json; charset=utf-8`
  - 响应体：`{"code":0,"message":"ok","data":{"status":"ok","mysql":"ok","redis":"ok","timestamp":"2026-11-26T06:00:55.964Z"},"request_id":"r-917fb2b96aa1ccde"}`
- **未注册路由实测（`GET /api/v1/not-registered`）**：返回 HTTP `404 Not Found`、同一 `X-Request-Id` 与统一 JSON 错误信封（`code=10001`, `data=null`），确认本阶段未注册任何业务路由。
- **故障注入分支实测**：集成测试使用真实 `go-redis` 客户端连接不可达本地端口及已关闭连接池实测 `GET /healthz`，均真实返回 HTTP `503 Service Unavailable`、业务码 `19002`、`data.mysql="ok"`、`data.redis="unavailable"` 与一致的 `X-Request-ID`。

### 3. `SIGTERM` 优雅关闭实测

- 向运行中的真实服务进程发送 `SIGTERM`，进程在超时窗口内以退出码 **`0`** 平滑退出。
- 结构化 `slog` 日志按序输出：
  1. `configuration_loaded`（`mysql_password: "[REDACTED]"`, `redis_password: "[REDACTED]"`）
  2. `shutdown_signal_received`
  3. `shutdown_step_1_stop_accepting_http`
  4. `shutdown_ws_hook_noop` → `shutdown_step_2_websocket_closed`
  5. `shutdown_mq_hook_noop` → `shutdown_step_3_mq_drained`
  6. `shutdown_step_4_http_drained`
  7. `shutdown_step_5_db_and_redis_closed`

---

## 五、第四阶段衔接注意事项

1. **审阅与合入**：第三阶段代码位于 `agent/stage3-backend`，等待审阅通过后由主仓库协调合入 `main`。
2. **第四阶段接入点**：
   - 在 `backend/internal/pkg/jwt/`、`backend/internal/middleware/jwt.go`、`backend/internal/repository/*_repo.go`、`backend/internal/service/{auth,user,friend}_service.go` 与 `backend/internal/controller/{auth,user,friend}_controller.go` 扩展用户认证与好友管理业务；
   - 在 `backend/internal/api/router.go` 注册 `/api/v1/auth/*`、`/api/v1/me*`、`/api/v1/users/search`、`/api/v1/friend-requests*`、`/api/v1/friends*` 路由；
   - 在第五、六、七阶段将真实 WS Hub 与 Redis Streams Consumer 的关闭排空实现接入 `cmd/server/main.go` 的 `ShutdownHooks`。
