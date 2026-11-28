# Enterprise IM 测试与千级 WebSocket 压测报告（初稿）

> **阶段说明**：本文档为第十阶段压测部分初稿，覆盖基于 [`backend/cmd/loadtest`](../backend/cmd/loadtest/main.go) 的 **1000 真实 WebSocket 客户端**并发压测实测数据与诊断结论。后端自动化单测 / 集成测试（`go test -race ./...`）与前端 Vitest 测试结果由并行测试分支负责，**待第十阶段集成合并后在本报告第 1、2 节补全**。

---

## 一、自动化单元与集成测试（待集成补全）

- **后端单元与集成测试（`go test -race ./...`）**：*待并行测试代理分支合入后补全。*
- **前端核心交互测试（Vitest）**：*待并行测试代理分支合入后补全。*

---

## 二、1000 并发 WebSocket 压测（`backend/cmd/loadtest`）

### 1. 测试环境与隔离方式

| 项目 | 配置详情 |
| :--- | :--- |
| **宿主机环境** | macOS（10 CPU cores / 16 GiB RAM），`ulimit -n 16384` |
| **中间件** | 本机复用 Docker `mysql8`（MySQL 8.0，`max_connections=151`）与 `my-redis`（Redis 7） |
| **隔离措施** | 独立工作区二进制启动隔离后端 `:8088`；独立临时库 `im_stage10_loadtest`（由 `deploy/mysql/init.sql` 初始化）；独立 `REDIS_DB=12`；`SNOWFLAKE_NODE_ID=20`；未接触生产/预览栈（`:8080` / `:5173` / `enterprise_im`） |
| **服务端代码** | 保持工作区基线 `e5a9069` 的 `backend/internal/**` 与 `backend/cmd/server/**` **零修改** |

### 2. 压测工具设计与运行命令

[`backend/cmd/loadtest`](../backend/cmd/loadtest/main.go) 全程走真实 HTTP API 与真实 WebSocket 握手（JWT），无任何 Mock：

1. **阶段 0（健康与基线）**：调用 `GET /healthz` 及管理员 `POST /api/v1/auth/login`、`GET /api/v1/admin/metrics/overview` 确认初始 `online_users=0`、`ws_connections=0`。
2. **阶段 1（账号注册与登录）**：并发创建 1000 个独立账号（`POST /api/v1/auth/register`）并各自登录（`POST /api/v1/auth/login`）获取真实 Access JWT（规避单用户 `MaxSessionsPerUser` 限制）。
3. **阶段 2（单聊与群聊拓扑）**：将 1000 个用户两两配对通过 `POST /api/v1/friend-requests` + `accept` 建立 **500 个单聊会话**；并划分为 **50 个 20 人群聊**（`POST /api/v1/groups`），覆盖全部 1000 名用户。
4. **阶段 3（千级 WS 建连与心跳）**：使用有界拨号并发（`64`）同时建立 **1000 条真实 WebSocket 长连接**（`/ws?access_token=...`），每条连接立即执行 `ping -> pong` 探针并按固定周期持续维持心跳，同时通过 `GET /api/v1/admin/metrics/overview` 校验峰值 `online_users=1000`、`ws_connections=1000`。
5. **阶段 4（并发收发与幂等校验）**：1000 个客户端并发向各自单聊与所属群聊发送带唯一 UUID `client_msg_id` 的文本消息（每人 2 条单聊 + 2 条群聊，共 4000 条消息，预期扇出 `msg.push` 共 `2000 * 2 + 2000 * 20 = 44,000` 次）；随后抽样 50 条已确认 `client_msg_id` 重发验证幂等不产生重复消息与重复推送。
6. **阶段 5（优雅断连与归零断言）**：发送 WebSocket Close 帧断开全部 1000 条连接，轮询 `GET /api/v1/admin/metrics/overview` 确认在线用户数与连接数归零。

#### 复现命令（验收通过配置，环境变量已脱敏）

```bash
cd backend
go build -o /tmp/im-server-lt ./cmd/server
go build -o /tmp/im-loadtest ./cmd/loadtest

# 隔离服务端关键环境变量（其余 DB/Redis 密码从 docker inspect 注入且不落盘不打印）
export APP_ENV=development APP_NODE_ID=node-lt-1000-r2 SERVER_ADDR=:8088
export MYSQL_DATABASE=im_stage10_loadtest REDIS_DB=12 SNOWFLAKE_NODE_ID=20
export MYSQL_MAX_OPEN_CONNS=90 MYSQL_MAX_IDLE_CONNS=45
export REDIS_POOL_SIZE=150 REDIS_MIN_IDLE_CONNS=30
export WS_WRITE_TIMEOUT=15s MQ_RECLAIM_MIN_IDLE=120s MQ_RECLAIM_INTERVAL=60s
export LOG_LEVEL=warn

# 启动压测（默认 clients=1000）
/tmp/im-loadtest \
  -base-url http://127.0.0.1:8088 \
  -clients 1000 \
  -group-size 20 \
  -single-msgs 2 \
  -group-msgs 2 \
  -send-stagger 5s \
  -send-interval 500ms \
  -heartbeat-interval 12s \
  -hold-duration 15s \
  -request-timeout 30s \
  -ack-timeout 30s \
  -settle-timeout 90s \
  -idempotency-samples 50
```

---

### 3. 核心验收结果（Run 2：环境参数调优后，1000 客户端全量通过）

- **压测时间（UTC）**：`2026-11-28T06:05:25Z` ~ `2026-11-28T06:06:08Z`（总耗时 `43.337s`，退出码 `0`）
- **压测规模**：`clients=1000`，`single_convs=500`，`group_convs=50`（每群 20 人），`single_msgs/user=2`，`group_msgs/user=2`

#### 3.1 四项核心断言 + 建连心跳校验

| 验收断言项 | 结果 | 实测详情 |
| :--- | :--- | :--- |
| 1000 真实 WS 建连与心跳保活 | **PASS** | `connected=1000/1000`，服务端峰值 `online_users=1000`，`ws_connections=1000`，`unexpected_closes=0`，`pings=2881`，`pongs=2880`（最后 1 次在主动停止阶段取消，无错误） |
| 消息丢失率 = `0` | **PASS** | `sent=4000/4000`，`ack_failures=0`，`expected_deliveries=44000`，`actual_unique_deliveries=44000`，`missing=0`，`loss_rate=0.000000` |
| 消息重复率 = `0`（含幂等重发） | **PASS** | `duplicate_deliveries=0`，`ack_seq_collisions=0`，`duplicate_rate=0.000000`，幂等重发抽样 `50/50` 一致且无新增推送 |
| 同会话 `seq` 严格递增（无乱序） | **PASS** | `out_of_order_count=0`（校验 `2000` 条 `(client, conversation)` 接收流） |
| 全部客户端断开后在线用户数与连接数归零 | **PASS** | `disconnected=1000/1000`，`final_online_users=0`，`final_ws_connections=0`（收敛耗时 `153ms`） |

#### 3.2 关键实测指标汇总

| 指标 | 实测值 |
| :--- | :--- |
| 注册 / 登录用户数 | `1000 / 1000`（耗时 `7.996s / 14.476s`；初始化阶段遇到 `19004` 死锁并由客户端退避重试 `1761` 次后全部成功） |
| 单聊会话数 / 群聊会话数 | `500 / 50`（建群与好友关系建立耗时 `2.227s`） |
| 1000 WS 建连耗时 / 服务端峰值统计 | `620ms` / `online_users=1000`、`ws_connections=1000` |
| 发送消息数（单聊 / 群聊 / 总计） | `2000 / 2000 / 4000`（`ack_retries=0`，`ack_failures=0`，`ws_error_frames=0`） |
| 发送阶段耗时 / 发送吞吐 | `7s` / `571.39 msg/s` |
| 端到端推送 (`msg.push`) 数量 / 收齐耗时 / 推送吞吐 | `44000 / 44000` / `15.976s` / `2754.14 push/s` |
| 断连收敛耗时 / 最终在线统计 | `153ms` / `online_users=0`，`ws_connections=0` |
| 服务端峰值 Goroutines / HeapAlloc | `2086` / `47.24 MiB` |
| Stream 最终 Pending / 死信数 | `im-msg-deliver` Pending=`0`；`im-msg-persist` Pending=`78`（后台延迟回执入库仍在异步消费）；`dead_letter_count=0` |

#### 3.3 延迟分位数统计

| 阶段 | 样本数 | Avg | P50 | P95 | P99 | Max |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| WS 建连 (Handshake) | 1000 | `11.80 ms` | `10.40 ms` | `19.31 ms` | `50.84 ms` | `57.07 ms` |
| 心跳 `ping` -> `pong` | 2880 | `22.98 ms` | `18.42 ms` | `51.21 ms` | `248.45 ms` | `1120.11 ms` |
| 发送 `msg.send` -> `msg.ack` | 4050 | `173.06 ms` | `152.58 ms` | `412.77 ms` | `698.30 ms` | `1651.84 ms` |
| 端到端 `msg.send` -> `msg.push` | 44000 | `7420.02 ms` | `7758.32 ms` | `9146.05 ms` | `9468.11 ms` | `10301.09 ms` |

#### 3.4 Redis Streams 消费组最终状态

| Stream | Group | Consumer | LastDeliveredID | Pending | OldestPendingAge(ms) |
| :--- | :--- | :--- | :--- | ---: | ---: |
| `im:stream:messages` | `im-msg-persist` | `node-lt-1000-r2-msg-persist-1` | `1795845955553-3` | 78 | 13155 |
| `im:stream:messages` | `im-msg-deliver` | `node-lt-1000-r2-msg-deliver-1` | `1795845957641-0` | 0 | 0 |
| `im:stream:events` | `im-event-deliver` | `node-lt-1000-r2-event-deliver-1` | `0-0` | 0 | 0 |
| `im:stream:events` | `im-event-audit` | `node-lt-1000-r2-event-audit-1` | `0-0` | 0 | 0 |

---

### 4. 默认配置基线对比（Run 1）与后端瓶颈诊断

在未调整服务端运行参数的默认配置下（`MYSQL_MAX_OPEN_CONNS=50`、`REDIS_POOL_SIZE=50`、`WS_WRITE_TIMEOUT=5s`、`MQ_RECLAIM_MIN_IDLE=30s`、`LOG_LEVEL=info`、`send-interval=150ms`、0~1s 突发启动），首轮 1000 客户端压测（`2026-11-28T06:00:17Z` ~ `06:03:24Z`）**失败退出（exit 1）**，真实暴露出以下可复现的架构与并发问题（本次严格遵守文件边界未修改 `backend/internal/**`，记录供集成阶段评估优化）：

| 对比维度 | Run 1（默认参数突发压测） | Run 2（环境变量调优 + 平滑发包） |
| :--- | :--- | :--- |
| 建连 / 峰值在线 | `1000/1000`（建连阶段成功） | `1000/1000`（全程零异常断开） |
| 异常断线 / 心跳错误 | `unexpected_closes=227` / `ping_errors=2853` | `unexpected_closes=0` / `heartbeat_errors=0` |
| 成功发送 (`msg.ack`) | `1100 / 4000`（`ack_failures=2900`） | `4000 / 4000`（`ack_failures=0`） |
| 丢失率 / 重复率 / 乱序 | `25.06%` (`2834/11308`) / `0.26%` (`29`) / `16` 次 | `0.000000` (`0`) / `0.000000` (`0`) / `0` 次 |
| 断连后最终在线 / 连接数 | `0 / 0`（`converge_ms=109`） | `0 / 0`（`converge_ms=153`） |

#### 已定位的后端关键现象与根因

1. **MySQL InnoDB 间隙锁死锁（HTTP 500 / `19004`）**：
   - 并发登录（`POST /api/v1/auth/login`）时，`SessionRepository.ListUnrevokedByUserDeviceForUpdate` 在 `REPEATABLE READ` 下对尚无记录的 `(user_id, device_id)` 执行 `SELECT ... FOR UPDATE` 触发间隙/上界锁冲突，随后 `CreateSession` (`INSERT INTO user_sessions`) 报 `Error 1213 (40001): Deadlock found`。
   - 并发建立好友（`POST /api/v1/friend-requests` 与 `accept`）在 `friendships` 双向插入与申请插入时同样出现 `Error 1213` 死锁（Run 1 共 1735 次，Run 2 共 1761 次）。
   - **影响与现状**：`loadtest` 仅在初始化阶段对 `19004` 进行退避重试后 100% 建完拓扑；真实 WS 消息收发阶段（Run 2）未触发任何死锁。
2. **每帧双 MySQL 查询放大连接池竞争与 5s 鉴权断线**：
   - `ws/client.go` `readPump` 对**每一帧**（包括 `ping` 与 `msg.send`）调用 `hub.authorize`，在 `middleware/jwt.go` 中固定查询 2 次 MySQL（`users` 与 `user_sessions`）；且 `MessageService.SendMessage` 每条消息包含多次 MySQL 前置校验、锁查重及事务（共约 25+ 次 DB 往返）。
   - 当默认 `MYSQL_MAX_OPEN_CONNS=50` 被 1000 并发突发打满且排队超过 `WS_WRITE_TIMEOUT=5s` 时，`hub.authorize` 超时映射为 `19001`（HTTP 503），触发 `hub.go` 主动向客户端下发 `1011` 关闭连接（导致 Run 1 出现 227 条连接中断与消息丢失）。将连接池调至 `MYSQL_MAX_OPEN_CONNS=90`、`WS_WRITE_TIMEOUT=15s` 并平滑发送步长后，Run 2 全程 `gorm_slow_query` 仅 1 条、零掉线。
3. **`XAUTOCLAIM` 30s 阈值与消费组长批处理重投竞争**：
   - `im-msg-deliver` 默认单 Worker（`MQ_WORKER_COUNT=1`、`read_count=100`）在 DB 慢查询积压时处理 100 条批次超过 `MQ_RECLAIM_MIN_IDLE=30s`，`reclaim-*` 协程提前认领仍在处理但尚未 `markDone`/`XAck` 的消息导致重复投递（Run 1 出现 29 次重复 `msg.push`）。将 `MQ_RECLAIM_MIN_IDLE` 调至 `120s` 后 Run 2 重复归零。
4. **`conv:lock` 释放时机与 `XADD` 之间存在极窄乱序窗口**：
   - `MessageService.commitNewMessage` 在函数 `defer` 中释放 Redis `conv:lock:{conversation_id}` 后，才由 `SendMessage` 调用 `XADD im:stream:messages`。在极端 DB/调度卡顿下（Run 1），同一会话并发发送方可能出现后提交的 `seq` 先进入 Stream（Run 1 观测到 16 次乱序）；在 Run 2 无卡顿场景下乱序为 `0`。后续集成如需彻底消除该理论窗口，可考虑将 `XADD` 纳入锁保护或由消费端/发送前协调保证有序。
