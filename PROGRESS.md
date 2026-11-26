# Enterprise IM 开发进度与阶段自检报告

**当前阶段结论：第二阶段（数据库设计与 ER 图）已完成实测，待审阅合入 `main`**

---

## 一、阶段进度总览

依据 [PROJECT_GOALS.md](PROJECT_GOALS.md) 第九节严格开发阶段划分：

| 阶段 | 状态 | 基线 / 交付物摘要 |
| --- | --- | --- |
| 第一阶段：需求分析与技术架构设计 | **已合入 `main`**（`357340b`） | 已完成 `.gitignore`、[docs/architecture.md](docs/architecture.md) 与 [docs/api.md](docs/api.md)，并完成 4 项审阅阻塞项整改后正式合入 `main` 分支（`Merge branch 'agent/stage1-architecture'`） |
| 第二阶段：数据库设计与 ER 图 | **已完成（待审阅合并）** | 已完成 [deploy/mysql/init.sql](deploy/mysql/init.sql)、[docs/er-diagram.md](docs/er-diagram.md) 与本阶段进度报告，并在真实 MySQL 8.0.44 容器与 `mermaid-cli`（`mmdc 12.2.0`）完成导入和渲染实测 |
| 第三阶段：后端基础工程搭建 | 未开始 | 本阶段严格止步于数据库与文档交付，未编写 Go 业务代码 |

---

## 二、第二阶段已完成项与范围说明

| 交付物 | 状态 | 覆盖内容摘要 |
| --- | --- | --- |
| [deploy/mysql/init.sql](deploy/mysql/init.sql) | 已完成 | 建立 `enterprise_im` 库（`InnoDB` / `utf8mb4` / `utf8mb4_0900_ai_ci`）；严格对齐 `PROJECT_GOALS.md` §六.3 落实核心十表（`users`、`user_sessions`、`friend_requests`、`friendships`、`groups`、`group_members`、`conversations`、`conversation_members`、`messages` Snowflake 非自增、`message_receipts`）；按冻结契约扩充 `assets`（媒体元数据、内部相对存储键、引用关系与删除拦截）与 `audit_logs`（append-only 不可变审计事件与多维过滤索引）；幂等写入本地开发管理员种子（`admin` / `Admin@123456`，真实 `bcrypt` `cost=10` 哈希） |
| [docs/er-diagram.md](docs/er-diagram.md) | 已完成 | 完整收录 12 张表的 Mermaid `erDiagram`、`client_msg_id` / `msg_id` / `seq` 职责边界、全部 12 表字段/约束/索引详解、业务事务与访问控制不变式、查询优化与分表归档预留策略，以及默认本地开发管理员种子说明 |
| [PROGRESS.md](PROGRESS.md) | 已完成 | 更新第一阶段已合入 `main`（`357340b`）的事实状态，记录第二阶段交付、Commit 清单、真实 MySQL 8.0 导入校验、Mermaid 渲染实测结果与第三阶段衔接事项 |

### 活跃 Worktree / 分支状态

| 项目 | 当前值 |
| --- | --- |
| 主仓库路径 | `/Users/niezidong/Projects/go`（`main` 基线 `357340b`） |
| 当前 Worktree | `/Users/niezidong/.herdr/worktrees/go/agent-stage2-database` |
| 当前分支 | `agent/stage2-database` |
| 职责边界 | 仅交付第二阶段数据库初始化脚本、ER 文档与进度更新；未修改 `PROJECT_GOALS.md`、`docs/architecture.md`、`docs/api.md`、`.gitignore`，未编写 Go 业务代码，未执行 `push` / `merge` |

---

## 三、本阶段 Commit 清单（含第一阶段基线节点）

1. `357340b` (`main`) — `Merge branch 'agent/stage1-architecture'`（第一阶段全部文档与审阅整改已合入 `main`）
2. `fc397bf` — `feat(db): add MySQL 8.0 init schema with 12 tables and bcrypt admin seed`（`deploy/mysql/init.sql`）
3. `89d219a` — `docs(db): add 12-table ER diagram and schema design guide`（`docs/er-diagram.md`）
4. 收口提交（紧随 `89d219a`）— `docs(db): update stage two progress and verification report`（`PROGRESS.md`）

---

## 四、真实环境验收实测结果

> 注：所有数据库校验均在已有 `mysql8`（`mysql:8.0` / `8.0.44`）Docker 容器的隔离临时库 `enterprise_im_stage2_verify` 中完成，未影响容器内其他已有数据库；验收完毕后已删除临时验证库。文档与提交均未包含任何数据库连接口令或环境密钥。

### 1. MySQL 8.0 导入与幂等实测

- **导入方式**：将 `deploy/mysql/init.sql` 目标库名临时映射为隔离库 `enterprise_im_stage2_verify`，通过 `docker exec -i mysql8` 导入，并连续执行两次验证幂等性。
- **执行结果**：两次导入均退出码 `0`、无 SQL 错误；首次导入仅出现 3 条 MySQL 8.0.44 关于 `conversation_members` 中 `TINYINT(1)` 显示宽度的标准弃用提示 `Warning (Code 1681)`（严格保留自 `PROJECT_GOALS.md` §六.3 原文规范），其余 DDL 与种子插入无错误、无重复记录。
- **库级字符集核对**（`information_schema.SCHEMATA`）：
  - `SCHEMA_NAME = enterprise_im_stage2_verify`
  - `DEFAULT_CHARACTER_SET_NAME = utf8mb4`
  - `DEFAULT_COLLATION_NAME = utf8mb4_0900_ai_ci`
- **`SHOW TABLES` 与 `information_schema.TABLES` 核对（共 12 张表，全部为 `InnoDB` + `utf8mb4_0900_ai_ci`）**：
  - `assets`
  - `audit_logs`
  - `conversation_members`
  - `conversations`
  - `friend_requests`
  - `friendships`
  - `group_members`
  - `groups`
  - `message_receipts`
  - `messages`
  - `user_sessions`
  - `users`
- **关键结构核对（`SHOW CREATE TABLE`）**：
  - `messages.id` 为 `bigint unsigned NOT NULL`（无 `AUTO_INCREMENT`），保留 `uk_msg_conv_seq`、 `uk_msg_sender_client`、`idx_msg_conv_created`、`idx_msg_sender_created`；
  - `assets` 具备 `uk_assets_asset_id`、`storage_key` / `thumbnail_key`、`ref_type` / `ref_id` / `ref_conversation_id` / `ref_count` 支撑引用鉴权与引用拦截删除；
  - `audit_logs` 无 `updated_at`，具备 `uk_audit_event_id` 与 `idx_audit_group_created` 等 5 组查询索引，在表结构与注释明确 `append-only` 语义；
  - 中文表/列注释及种子昵称（`HEX(nickname) = E7B3BBE7BB9FE7AEA1E79086E59198`，即 `系统管理员`）以 UTF-8 正确落库，无乱码。
- **默认管理员种子实测**：
  - 查询 `users WHERE username='admin'`：共 `1` 条记录（二次导入仍为 `1` 条），`id=1`、`role='admin'`、`status=1`、`CHAR_LENGTH(password_hash)=60`、前缀 `$2a$10$`；
  - 对数据库实际查出的 `password_hash` 使用本地 Python `bcrypt.checkpw(b"Admin@123456", ...)` 实测校验返回 `True`；
  - 默认开发管理员账号仅限本地开发联调：`username = admin` / `password = Admin@123456`（非生产密钥）。

### 2. Mermaid 渲染与仓库格式检查

- **Mermaid 渲染实测**：使用本地安装的 `mermaid-cli`（`mmdc 12.2.0`）直接渲染 [docs/er-diagram.md](docs/er-diagram.md)，识别 `1` 幅图表并成功生成约 `470K` SVG（`erDiagram`），退出码 `0`。
- **仓库格式校验**：各次提交前执行 `git diff --cached --check` 均通过，无行尾空白或末尾多余空行。

---

## 五、遗留问题与第三阶段衔接事项

1. **审阅与合入主分支**：本阶段交付位于 `agent/stage2-database` 分支，等待审阅通过后由主仓库协调合入 `main`。
2. **Docker Compose 挂载与生产口令替换**：后续部署编排阶段需将 `deploy/mysql/init.sql` 挂载至 MySQL 8.0 容器 `/docker-entrypoint-initdb.d/`；进入非本地环境前务必修改默认管理员 `admin` 的口令。
3. **第三阶段后端模型映射要点**：
   - 在 `backend/internal/model/` 为核心十表与 `assets`、`audit_logs` 建立 GORM 模型；
   - `messages.id` 由服务端 Snowflake 生成并显式写入，禁止依赖数据库自增；
   - 严格落实 `assets` 内部 `storage_key` / `thumbnail_key` 不对外暴露、未引用资产仅所有者可读、已被消息引用返回 `16006` 拒绝删除；
   - 严格落实 `audit_logs` 的 `im-event-audit` 幂等消费写入（`uk_audit_event_id`）及 `append-only` 禁止更新/删除约束。
