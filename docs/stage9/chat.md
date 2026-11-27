# 第九阶段联调报告：单聊 / 群聊 / 上传 / 撤回 / 回执（stage9-chat）

## 1. 隔离联调环境

本次联调在分支 `agent/stage9-chat`（基线 `b2e4cf1`）上使用完全隔离的本地运行时环境执行，未修改冻结契约文件（`PROJECT_GOALS.md`、`docs/architecture.md`、`docs/api.md`、`docs/er-diagram.md`、`deploy/mysql/init.sql`）：

- **后端服务**：Go + Gin 二进制（`SERVER_ADDR=:8082`，`SNOWFLAKE_NODE_ID=12`）
- **前端服务**：Vite 开发服务器（`IM_VITE_PORT=5175`，`IM_BACKEND_ORIGIN=http://127.0.0.1:8082`，CORS 允许 `http://localhost:5175` 与 `http://127.0.0.1:5175`）
- **持久化隔离**：MySQL 8.0 独立库 `im_stage9_chat`（由 `deploy/mysql/init.sql` 临时替换库名后导入 12 张表），Redis 7 独立逻辑库 `DB 2`，上传卷目录 `/tmp/im_stage9_chat_uploads`（口令通过进程环境变量注入，未写入配置文件、日志或报告）。
- **测试工具**：Playwright `1.65.1`（真实浏览器上下文：Chromium 159.0 + Firefox 159.0），全程直连真实 `/api/v1` 与 `/ws`，未使用任何 API/WS Mock。

## 2. 联调覆盖场景与验证结果

| 序号 | 联调场景 | 覆盖浏览器 | 关键验证点与结果 |
| --- | --- | --- | --- |
| 1 | 注册、登录与互加好友后实时单聊 | Chromium、Firefox | 两个独立 BrowserContext 通过 `/register`、`/login` 与 `/contacts` 完成注册、登录、好友申请与同意；实时单聊文本消息两端可见，`GET /api/v1/conversations/:id/messages` 返回 `seq` 严格递增且与 DOM 渲染顺序一致。 |
| 2 | 真实多媒体上传与鉴权打开/下载 | Chromium、Firefox | 使用真实小文件（`sample.png`、`sample-doc.txt`、`sample-voice.wav`）经 `POST /api/v1/uploads` 上传并发送；接收端图片/缩略图与语音通过 Axios 携带 Bearer Token 拉取并渲染为 `blob:` URL，图片灯箱预览正常，TXT 附件通过鉴权下载且内容与原文件逐字节一致，语音时长展示 `1"`；验证未携带 Token 访问 `/api/v1/uploads/{asset_id}/download` 返回 HTTP 401（`code=11001`）。 |
| 3 | 消息撤回与异常分支 | Chromium、Firefox + Live HTTP/WS | 窗口内点击「撤回」并确认后，发送端显示「你撤回了一条消息」与「该消息已撤回」，接收端实时折叠为「该消息已撤回」并展示撤回通知；非发送者撤回返回 HTTP 403（`code=14003`），重复撤回返回 HTTP 409（`code=14004`）；在 `CHAT_RECALL_WINDOW=2s` 实测超窗撤回时，HTTP `POST .../recall` 返回 HTTP 409（`code=14005`），WS `msg.recall` 返回 `error` 帧（`code=14005`）。 |
| 4 | 四阶段送达与已读回执 | Chromium、Firefox | 接收方离线时发送消息，发送方气泡保持「已发送（`sent`）」；接收方上线停留会话列表后自动发送 `msg.delivered_ack`，发送方转为「已送达（`delivered`）」且接收方显示未读红点；接收方进入会话发送 `msg.read_ack` 后，发送方转为「已读（`read`）」；验证 `GET .../receipts`、`POST .../read`（`unread_count=0`）及超最大 `seq` 的 `POST .../delivered`（HTTP 400 / `code=13006`）与 `docs/api.md` 一致。 |
| 5 | 建群、邀请、群聊广播与群管理 | Chromium（三端并发） | 3 个真实账号同时在线：创建群聊、邀请第三人入群、修改群名称、群内实时广播文本与图片、打开「消息回执详情」弹窗核对实时统计（`应接收 2 / 已读 1 / 未读 1` → 全员打开后 `已读 2 / 未读 0`）；群内设为管理员、移出成员后，被移出成员再向该群发消息被后端拦截（返回成员权限/只读错误码）。 |
| 6 | 刷新页面持久化 | Chromium、Firefox | 单聊与群聊窗口执行 `page.reload()` 后，会话标题、文本/图片/文件/语音及撤回态完整恢复，无空白页或 `pageerror`。 |

## 3. 本归属修复的联调缺陷

1. **新会话实时到达时占位标题未自动水合与切换回 `/chat` 未刷新会话列表**：
   - **现象**：当用户位于 `/chat` 或其他页面时，若对端新建群聊、邀请入群或建立新会话并推送消息/系统通知，`conversationStore.applyIncomingMessage` 会创建 `{ title: "会话 <id>", type: "single" }` 占位项，且未触发详情拉取；同时 `ChatView.vue` 在 `conversations.length > 0` 时重新挂载不会调用 `fetchList()`，导致需要用户手动点击“刷新会话”才能看到正确群名或新会话。
   - **修复文件**：
     - [conversation.ts](../../frontend/src/stores/conversation.ts)：新增 `placeholderConvIds`、`hydratingConvIds` 与 `hydrateConversation(conversationId)`，在 `applyIncomingMessage` 创建/命中占位会话以及 `applySystemNotice` 收到 `group.created`、`group.member_added`、`group.updated`、`friend.accepted` 时自动拉取 `fetchDetail` 补全会话元数据，并在 `fetchDetail` 增加空值保护。
     - [ChatView.vue](../../frontend/src/views/chat/ChatView.vue)：`onMounted` 中在已登录时统一触发 `conversationStore.fetchList()` 刷新会话列表。

## 4. 跨归属问题

1. **`frontend/vite.config.ts` 端口与代理目标硬编码（归 `admin-qa` 提交）**：
   - 当前基线 `frontend/vite.config.ts` 写死 `port: 5173` 与 `http://127.0.0.1:8080`；隔离联调期间已在本地临时切换为读取 `IM_VITE_PORT` / `IM_BACKEND_ORIGIN`，并在交付前按约定执行 `git checkout -- frontend/vite.config.ts` 还原，未擅自提交。
2. **`AppLayout.vue` 未显式传入 `trackChatVisibility: false`（第八阶段遗留 NIT）**：
   - `frontend/src/layouts/AppLayout.vue` 调用 `useChatRealtime()` 未传 `{ trackChatVisibility: false }`，在 `/chat` 外页面监听 `visibilitychange`/`focus`（当 `activeConversationId` 为 `null` 时即早返回，未阻断本阶段联调路径），按约束未越界修改。

## 5. 自检命令与退出码

| 检查项 | 命令 | 退出码 | 结果摘要 |
| --- | --- | --- | --- |
| 前端 TypeScript 检查 | `cd frontend && npm run typecheck` | `0` | 零类型错误 |
| 前端现有单元测试 | `cd frontend && npm test` | `0` | 4 个测试文件，25/25 全部通过 |
| 后端关联包单元测试 | `cd backend && go test -p 1 ./internal/controller ./internal/service ./internal/api` | `0` | `controller` / `service` / `api` 全部通过 |
| Playwright E2E 全链路 | `/tmp/pw-runner/node_modules/.bin/playwright test -c frontend/e2e/chat/playwright.config.ts` | `0` | `3 passed (27.2s)`：Chromium 单聊+上传+撤回+回执+刷新、Chromium 群聊广播+管理+回执弹窗、Firefox 核心单聊+上传+撤回+刷新 |
| 超窗撤回（`14005`）实测 | 临时以 `CHAT_RECALL_WINDOW=2s` 启动隔离后端并执行真实 HTTP/WS 撤回校验 | `0` | HTTP 409 / `code=14005`，WS `error` 帧 `code=14005` |

> 复跑提示：需确保隔离后端（`:8082`、`im_stage9_chat`、Redis DB `2`）与支持 `IM_VITE_PORT=5175` / `IM_BACKEND_ORIGIN=http://127.0.0.1:8082` 的 Vite 已启动，并在 `frontend` 安装 `@playwright/test`（或由 `admin-qa` 合入后直接使用 `npx playwright test -c frontend/e2e/chat/playwright.config.ts`）。

## 6. 联调实测截图

- 单聊发送端（四阶段已读、图文/文件/语音与撤回）：[single-chat-sender.png](assets/chat/single-chat-sender.png)
- 单聊接收端（鉴权图片/附件/语音与对端撤回态）：[single-chat-receiver.png](assets/chat/single-chat-receiver.png)
- 群聊消息回执详情弹窗（`应接收 2 / 已送达 2 / 已读 2 / 未读 0`）：[group-chat-receipts-dialog.png](assets/chat/group-chat-receipts-dialog.png)
