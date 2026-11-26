# 企业 IM 接口与实时协议

REST 管理账号、好友、会话、文件与群。发送消息使用 WebSocket，历史拉取和实时推送共享同一组消息 ID、序号与成员权限。产品范围来自[项目目标](../PROJECT_GOALS.md)，事务边界、Redis Key 和 Consumer Group 见[架构设计](architecture.md)。

## HTTP 通则

下表列出完整路径。生产入口使用同源 HTTPS/WSS。除文件字节和 WebSocket 升级外，HTTP 成功与失败均返回 `application/json`，并在 `X-Request-ID` 返回与信封一致的服务端追踪 ID。创建资源返回 201，其余成功写入和查询返回 200，有效的空结果使用 `data: null` 或空数组。

```json
{
  "code": 0,
  "message": "ok",
  "data": {"conversation_id": "84", "last_read_seq": "42", "last_delivered_seq": "42", "unread_count": 0},
  "request_id": "r-28642"
}
```

| 公共约定 | 线上的表示与含义 |
| --- | --- |
| 鉴权 | “用户”表示有效的 `Authorization: Bearer <access_token>`。成员和角色列均包含用户鉴权。身份取自服务端验证的用户及设备会话。后台的“平台管理员”指 `users.role=admin`。 |
| 标识 | 数据库 ID 和会话 `seq` 用十进制字符串传输，空会话末序号为 `"0"`。`client_msg_id` 为 UUID v4 或 ULID，`asset_id` 为上传服务生成的 UUID。客户端按整数值比较序号。计数和大小使用 JSON 整数。 |
| 时间 | 资源时间采用带毫秒的 UTC RFC 3339。WS 信封时间使用 Unix 毫秒整数。撤回期限、鉴权和已读时间均以服务端时钟为准。 |
| 资源分页 | 标注“页”的接口接受可选 `cursor` 和 `limit`（默认 20，范围 1–100），返回 `{items, next_cursor, has_more}`。首屏省略 cursor，末页 next_cursor 为 null。游标绑定用户、过滤条件和排序键。会话列表按本人置顶、更新时间、ID 降序，其他资源列表默认按 ID 降序。消息分页单独使用 seq。 |
| 写入 | 默认 JSON 请求体。PATCH 至少带一个列出的可写字段，省略的字段保留原值。带事务的操作在成功提交后才响应。账号密码、Token、文件磁盘路径均不进入日志。 |

### 跨接口对象

| 对象 | 字段与使用方 |
| --- | --- |
| `User` | `id, username, nickname, avatar_url, signature, online, last_online_at`。无头像用空字符串，从未在线的时间为 null。本人详情另含 `role, status`，status 取 1（启用）或 0（禁用）。 |
| `Tokens` | `access_token, refresh_token, token_type, expires_in, refresh_expires_in, session_id, device_id`。token_type 为 Bearer，有效期以秒计。每个设备使用稳定的 device_id 及 web、desktop、mobile 之一的 device_type。 |
| `FriendRequest` | `id, from_user_id, to_user_id, message, status, created_at, handled_at`。状态取 pending、accepted、rejected、canceled，待办记录的 handled_at 为 null。 |
| `Conversation` | `id, type, peer, group_id, title, avatar_url, last_msg_id, last_seq, last_msg_preview, last_msg_at, last_delivered_seq, last_read_seq, unread_count, is_pinned, is_muted, is_visible, can_send`。single 填对端 User，group 填 group_id，另一项为 null。末消息字段描述全会话，两个水位和偏好属于当前用户。 |
| `Group / GroupMember` | 群含 `id, conversation_id, name, avatar_url, announcement, owner_id, status, member_count, max_members`，status 取 active 或 disbanded。成员含 `user_id, nickname, avatar_url, nickname_in_group, role, online, joined_at`，群角色取 owner、admin、member。 |
| `Message` | `msg_id, client_msg_id, conversation_id, seq, sender_id, msg_type, content, extra, status, created_at, recalled_at`。普通行 status 为 normal，撤回后的原行为 recalled，recalled_at 为服务端时间。单聊中发送者的历史消息另带 `delivery_status`，可取 sent、delivered、read。群按成员水位查询回执。 |
| `Asset` | `asset_id, purpose, url, filename, mime, size_bytes`。图片与头像另含 `width, height, thumbnail_url`，语音另含 `duration_ms`。`url` 为同源受权资源路径（`/api/v1/uploads/{asset_id}/download` 或经后端鉴权反代的 `/uploads/...` 路径），`size_bytes` 表示原件字节数。 |

## REST 接口

### 账号、设备与资料

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/auth/register` | 公开 | username、password、nickname | `{user: User}`。新账号 role=user，登录接口随后创建设备会话。 |
| POST | `/api/v1/auth/login` | 公开 | username、password、device_id、device_type | `{user: User, tokens: Tokens}`。同设备替换旧会话，其他设备继续使用各自会话。 |
| POST | `/api/v1/auth/refresh` | Refresh 凭证 | refresh_token、device_id | `{tokens: Tokens}`。锁定所属会话、比对旧摘要、轮换两枚凭证并提交。 |
| POST | `/api/v1/auth/logout` | 用户 | scope 取 current 或 all，默认 current | `{scope, revoked_count}`。当前设备或本人所有设备的会话撤销后关闭对应 WS。 |
| PUT | `/api/v1/me/password` | 用户 | old_password、new_password | `{revoked_count}`。事务内同时更换哈希并撤销全部设备，用户重新登录。 |
| GET | `/api/v1/me` | 用户 | 无 | `{user: User, session_id, device_id}`。页面恢复时读取。 |
| PATCH | `/api/v1/me` | 用户 | nickname、signature 中至少一项 | `{user: User}`。signature 传空串即清除。 |
| POST | `/api/v1/me/avatar` | 用户 | multipart 字段 file | `{user: User, asset: Asset}`。校验并持久化原图和缩略图，再更新 avatar_url。 |
| GET | `/api/v1/users/search` | 用户 | q、field（username 或 nickname）、match（exact 或 prefix）、分页 | User 页。按所选字段做精确或字面前缀匹配。 |

username 和 nickname 各为 1–64 个字符，signature 最多 255 个字符。密码为 8–72 个 UTF-8 字节，服务端保存 bcrypt 哈希。Access JWT 绑定 sub、jti、session_id、exp。Refresh 仅保存 SHA-256 摘要，已撤销、过期、设备不匹配或已消费的 Refresh 都无法轮换。服务端每次校验用户状态、会话和当前 access_jti，已吊销的 JTI 写入 `im:token:blacklist:{jti}` 直至到期。

Axios 收到 Access 过期错误时，同一设备共享一次刷新（含使用该凭证的浏览器标签页），等待者取得新凭证后各自重放原请求一次。WSClient 新建连接也使用新 Access。若轮换响应丢失，先读取同设备已经保存的最新凭证；服务端已消费旧 Refresh 且客户端无法恢复时，重新登录。

### 好友与申请

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/friend-requests` | 用户 | to_user_id、message（可空，最多 255 字） | `{request: FriendRequest}`。双方任一方向已有 pending 或有效好友关系均拒绝重复创建。 |
| GET | `/api/v1/friend-requests` | 用户 | direction 取 received 或 sent，可选 status、分页 | FriendRequest 页，附对端 User。 |
| POST | `/api/v1/friend-requests/{id}/accept` | 收件人 | 路径为申请 ID | `{request, friend_id, conversation_id, system_msg_id}`。提交即建立双向关系和唯一单聊。 |
| POST | `/api/v1/friend-requests/{id}/reject` | 收件人 | 路径为申请 ID | `{request: FriendRequest}`，status=rejected。 |
| POST | `/api/v1/friend-requests/{id}/cancel` | 申请人 | 路径为申请 ID | `{request: FriendRequest}`，status=canceled。 |
| GET | `/api/v1/friends` | 用户 | 分页 | 页项含 `friend_id, user, remark, online, conversation_id`。在线状态合并所有设备。 |
| PATCH | `/api/v1/friends/{friend_id}` | 本人有效好友 | remark（0–64 字） | `{friend_id, remark}`。仅写本人这一侧。 |
| DELETE | `/api/v1/friends/{friend_id}` | 本人有效好友 | 路径为对端用户 ID | `{friend_id, conversation_id, is_visible}`，双方会话隐藏。 |

接受申请在同一事务更新申请、两条好友关系、唯一单聊和双方会话成员，再追加一条 system 消息。同一申请只处理一次。删除好友后双方仍可凭会话 ID 读取既有历史，重新成为好友时恢复原会话。

### 会话、历史消息与回执

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| GET | `/api/v1/conversations` | 用户 | 分页，可选 include_hidden | Conversation 页，包含对端或群摘要、本人水位和偏好 |
| GET | `/api/v1/conversations/{conversation_id}` | 会话成员 | 无 | 完整 Conversation、本人角色、对端或群资料 |
| PATCH | `/api/v1/conversations/{conversation_id}/settings` | 会话成员 | 任意 is_pinned、is_muted、is_visible | 当前用户的会话设置 |
| GET | `/api/v1/conversations/{conversation_id}/members` | 会话成员 | 分页 | 页项含 `user, role, nickname_in_group, online, joined_at, last_delivered_seq, last_read_seq` |
| GET | `/api/v1/conversations/{conversation_id}/messages` | 会话成员 | before_seq 或 after_seq 二选一、limit | Message 页。before 按 seq 倒查后以升序返回 |
| GET | `/api/v1/conversations/{conversation_id}/sync` | 会话成员 | after_seq、limit | `{items, from_seq, to_seq, has_more}`，只取 seq > after_seq |
| POST | `/api/v1/conversations/{conversation_id}/messages` | 会话成员 | client_msg_id、msg_type、content、extra | `{message: Message}`，返回完整 `Message` 对象（其中 `status="normal"`、`delivery_status="sent"`；幂等重试返回原 `msg_id`、`seq` 与当前最新 `status`/`delivery_status`） |
| POST | `/api/v1/conversations/{conversation_id}/messages/{msg_id}/recall` | 原发送者 | 无 | 更新后的原 Message、recall Message |
| GET | `/api/v1/conversations/{conversation_id}/messages/{msg_id}/receipts` | 会话成员 | 可选 `status=read\|unread\|delivered\|pending`、分页 | `{msg_id, conversation_id, seq, total_members, delivered_count, read_count, unread_count, items}`；每项含 `user, role, delivery_status, last_delivered_seq, last_read_seq, delivered_at, read_at` |
| POST | `/api/v1/conversations/{conversation_id}/read` | 会话成员 | last_read_seq | last_read_seq、unread_count=0、read_at |
| POST | `/api/v1/conversations/{conversation_id}/delivered` | 会话成员 | last_delivered_seq | 更新后的送达水位，供离线补偿使用 |

会话列表从本人 conversation_members 联表取得，单聊按 direct_key 唯一，群按 group_id 唯一。服务端用 IN 批量装配对端、群资料和在线状态，不在循环中查询。消息历史使用 `(conversation_id, seq)` 索引，禁止 OFFSET 深分页。`before_seq` 查询 `seq < before_seq`，`after_seq` 和 sync 查询 `seq > after_seq`。服务端拒绝超过会话当前末 seq 的水位，重复或较小水位只返回当前值。群聊回执明细接口依据 `conversation_members` 的 `last_delivered_seq` / `last_read_seq` 水位线统计并分页返回已读/未读成员，单聊则结合 `message_receipts` 返回逐条时间与状态。

HTTP 消息发送和 WS `msg.send` 共用 `(sender_id, client_msg_id)` 唯一约束。重试必须使用原 client_msg_id，服务端返回已有 msg_id、seq 和当前 delivery_status，不产生第二条消息。撤回只允许发送者在配置的时间窗内执行。`extra` 只接受与 msg_type 匹配的已上传资产引用。

### 上传与媒体

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/uploads` | 用户 | multipart 的 file、purpose=image\|file\|voice\|avatar\|group_avatar | Asset，包含 asset_id、url、download_url、filename、mime、size_bytes 和媒体元数据 |
| GET | `/api/v1/uploads/{asset_id}` | 资产所有者或消息成员 | 无 | Asset 和短期 download_url |
| GET | `/api/v1/uploads/{asset_id}/download` | 资产所有者或消息成员 | 无 | 二进制文件，Content-Disposition 使用清洗后的文件名 |
| DELETE | `/api/v1/uploads/{asset_id}` | 资产所有者 | 无 | `{asset_id, deleted}`。已被消息引用时拒绝删除 |

上传写入临时文件后检查真实 MIME、内容、大小、像素、音频时长和路径规则，再原子移动到 `uploads/` 卷。图片生成缩略图，语音保存 duration_ms。资产元数据与引用关系由服务端持久记录（第二阶段建表设计时在核心十表之外按需扩充 `assets` 支撑表）；只有消息、头像或群资料引用资产后才允许读取，`/api/v1/uploads/{asset_id}/download` 为受权附件统一下载入口，`/uploads/...` 仅用于经 Nginx/后端鉴权反代的受权静态媒体输出，下载不泄露本地磁盘路径。删除资产不会删除已提交消息。

### 群聊管理

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/groups` | 用户 | name、member_ids，可选 avatar_asset_id、announcement | Group、Conversation、成员列表和创建 system Message |
| GET | `/api/v1/groups` | 用户 | 分页，可选 status | Group 摘要页，含本人 role 和 conversation_id |
| GET | `/api/v1/groups/{group_id}` | 群成员 | 无 | Group、Conversation、本人 role、member_count |
| PATCH | `/api/v1/groups/{group_id}` | owner 或 admin | name、avatar_asset_id、announcement 任意组合 | 更新后的 Group 和 system Message |
| DELETE | `/api/v1/groups/{group_id}` | owner | 无 | status=disbanded、解散 system Message |
| GET | `/api/v1/groups/{group_id}/members` | 群成员 | 分页，可选 role、online_only | GroupMember 页 |
| POST | `/api/v1/groups/{group_id}/members` | owner 或 admin | user_ids | 新增成员、conversation_members 和 system Message |
| DELETE | `/api/v1/groups/{group_id}/members/{user_id}` | owner 或 admin | 路径用户 ID | removed_at 和 system Message |
| POST | `/api/v1/groups/{group_id}/leave` | 群成员 | 无 | left_at 和 system Message。owner 必须先转让或解散 |
| POST | `/api/v1/groups/{group_id}/transfer` | owner | target_user_id | 新 owner_id 和转让 system Message |
| PATCH | `/api/v1/groups/{group_id}/members/{user_id}/role` | owner | role=admin\|member | 更新后的成员 role 和 system Message |
| PATCH | `/api/v1/groups/{group_id}/members/me` | 群成员 | nickname_in_group | 更新后的 GroupMember |
| GET | `/api/v1/groups/{group_id}/audit` | owner 或 admin | 分页，可选 from、to | 群管理事件分页 |

建群请求的 member_ids 去重后必须包含创建者之外至少一名用户。群容量默认 500，成员数量在事务内检查。创建、邀请、移除、转让、角色变化、公告变化、退出和解散都同步写 group_members、conversation_members 和有序 system Message。移出或退出后不能收到新正文或附件。重新加入产生新的成员记录，历史访问遵循成员加入和退出水位。

### 管理后台

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| GET | `/api/v1/admin/users` | 平台管理员 | q、status（1 或 0）、role、分页 | User 页，含 session_count、online |
| GET | `/api/v1/admin/users/{user_id}` | 平台管理员 | 无 | 用户详情、设备会话、好友/群计数和审计摘要 |
| PATCH | `/api/v1/admin/users/{user_id}/status` | 平台管理员 | `status=1\|0`（`1` 启用、`0` 禁用）、reason | 用户状态（`1` 或 `0`）、revoked_session_count、kicked_connection_count |
| POST | `/api/v1/admin/users/{user_id}/kick` | 平台管理员 | 可选 session_id、reason | 撤销 session 数和关闭连接数 |
| GET | `/api/v1/admin/groups` | 平台管理员 | q、status、分页 | 群摘要页，含 owner、member_count、last_seq |
| GET | `/api/v1/admin/groups/{group_id}` | 平台管理员 | 无 | 群详情、成员页和系统事件摘要 |
| DELETE | `/api/v1/admin/groups/{group_id}` | 平台管理员 | reason | status=disbanded 和审计记录 |
| GET | `/api/v1/admin/messages/audit` | 平台管理员 | conversation_id、sender_id、keyword、from、to、分页 | Message 审计页，含 msg_id、seq、类型、状态、撤回信息 |
| GET | `/api/v1/admin/metrics/overview` | 平台管理员 | 无 | online_users、ws_connections、messages_per_minute、streams、goroutines、heap_bytes |
| GET | `/api/v1/admin/metrics/streams` | 平台管理员 | 无 | stream、group、consumer、last_delivered_id、pending_count、oldest_pending_age |
| GET | `/api/v1/admin/health/dependencies` | 平台管理员 | 无 | MySQL/Redis 状态、延迟、版本和最近错误时间 |

后台接口统一产生不可变审计事件（经由业务事务与 `im:stream:events` / `im-event-audit` 消费组落库；第二阶段将在核心十表之外按需扩充 `audit_logs` 支撑表）。禁用用户在事务内写 `users.status=0`、撤销全部 `user_sessions` 并写 Token 黑名单，提交后通过 `im:route:session:{session_id}` 集合关闭对应会话下全部标签页连接并下发 `system.notice` 的 `account.disabled` 事件。启用（`users.status=1`）只恢复登录资格。后台审计按会话、发送者、关键词和时间范围分页，超过允许范围返回业务错误。

### 系统健康检查（免鉴权）

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| GET | `/healthz` | 公开（免鉴权，供 Docker `healthcheck` 与探针调用） | 无 | `{status: "ok", mysql: "ok", redis: "ok", timestamp}`；MySQL 或 Redis 探活失败时返回 HTTP 503 与业务错误码 `19001` / `19002` |

## HTTP 错误处理

错误仍使用 `{code, message, data, request_id}`。HTTP 状态表示传输层结果，客户端按业务 code 分支。可恢复消息错误必须保留原 client_msg_id，并在重连后按会话 seq 校验。

## WebSocket JSON 协议

### Envelope

客户端与服务端的每一帧都必须使用同一 JSON Envelope，服务端不接受裸数据、未知 type 或缺少 data 的帧。

~~~json
{
  "type": "msg.send",
  "req_id": "req_01J8K3Y6G2N8",
  "timestamp": 1795687200123,
  "data": {}
}
~~~

`type` 是下表中的固定字符串。请求方生成 `req_id`，服务端在应答和错误中原样返回相关 ID。`timestamp` 为发送方 Unix 毫秒时间，服务端以接收时间进行鉴权和排序，不能用它分配会话 seq。单帧大小超过配置上限返回错误码 10002。握手地址为 `/ws?access_token=<access_token>`，非浏览器客户端也可使用 Authorization 头。服务端在握手和每个入站帧检查用户、session、黑名单、成员身份和账号状态。

### 11 类帧

| type | 方向 | data 示例 | 处理契约 |
| --- | --- | --- | --- |
| `msg.send` | 客户端 → 服务端 | `{"conversation_id":"90001","client_msg_id":"01J8K3Y6G2N8B6QWZ8Y9Q5A4C1","msg_type":"text","content":"hello","extra":{}}` | 客户端先将消息置为 `sending`。服务端校验成员、内容和附件，按 `(sender_id, client_msg_id)` 去重，分配 Snowflake `msg_id` 和会话 `seq`，事务提交后回 `msg.ack`。 |
| `msg.ack` | 服务端 → 发送端 | `{"client_msg_id":"01J8K3Y6G2N8B6QWZ8Y9Q5A4C1","msg_id":"188900000000001","conversation_id":"90001","seq":"42","status":"normal","delivery_status":"sent"}` | 只表示消息已经写入 MySQL。`status` 恒为 `Message.status`（`normal` 或 `recalled`），`delivery_status` 表示四阶段进度（`sent`、`delivered` 或 `read`）。重发同一 client_msg_id 必须返回同一 msg_id、seq 与当前最新 `status`/`delivery_status`。 |
| `msg.push` | 服务端 → 会话成员端 / 发送端 | 新消息：`{"push_type":"message","message":{"msg_id":"188900000000001","conversation_id":"90001","seq":"42","sender_id":"10001","client_msg_id":"01J8K3Y6G2N8B6QWZ8Y9Q5A4C1","msg_type":"text","content":"hello","extra":{},"status":"normal","created_at":"2026-11-26T04:00:00.123Z","recalled_at":null},"delivery_status":"sent"}`；回执/水位：`{"push_type":"receipt","conversation_id":"90001","user_id":"10002","delivery_status":"read","msg_id":null,"seq":null,"last_delivered_seq":"42","last_read_seq":"42","updated_at":"2026-11-26T04:00:05.000Z"}` | `push_type=message` 由 `im:stream:messages` 投递完整 `Message`（含 `created_at`、`recalled_at`）到在线端，接收端写入本地 Store 后回送 `msg.delivered_ack`。`push_type=receipt` 由 `im:stream:events` 推送回执/水位变更给发送端：单条 `delivered_ack` 可附带 `msg_id` 与 `seq`，会话级 `read_ack` 触发时 `msg_id`/`seq` 为 `null`，按 `last_delivered_seq`/`last_read_seq` 批量推进本地状态。 |
| `msg.delivered_ack` | 接收端 → 服务端 | `{"msg_id":"188900000000001","conversation_id":"90001","seq":"42"}` | 仅允许该消息的会话成员推进自己的送达回执。服务端单调推进 `last_delivered_seq`，并由 `im:stream:events` 通知发送端。 |
| `msg.read_ack` | 接收端 → 服务端 | `{"conversation_id":"90001","last_read_seq":"42"}` | 仅允许成员把本人的已读水位向前推进。服务端批量更新范围内回执、清零未读并广播已读事件。 |
| `msg.recall` | 客户端 → 服务端，随后回发操作端 | 入站：`{"conversation_id":"90001","msg_id":"188900000000001"}`；响应：`{"conversation_id":"90001","recalled_msg_id":"188900000000001","recall_msg_id":"188900000000002","seq":"43","recalled_at":"2026-11-26T04:01:00.000Z"}` | 只允许原发送者在撤回窗口内操作。事务中更新原消息为 `recalled` 并追加新 `seq` 的 `recall` 消息记录，提交后回发 `msg.recall` 响应；随后经 `im:stream:messages` 向在线成员推送该 `recall` 记录（`msg.push`），并经 `im:stream:events` 广播 `system.notice(event="message.recalled")` 触发各端就地更新原消息。 |
| `presence.update` | 服务端 → 订阅端 | `{"user_id":"10002","online":true,"device_count":2,"last_online_at":null}` | 好友或群成员的合并在线状态。只有该用户所有活跃连接的 `im:presence:conn:{conn_id}` 租约失效且活跃连接数归零后才发 offline，客户端不能根据单个连接关闭自行判定。 |
| `ping` | 客户端 → 服务端 | `{"nonce":"p_01J8K3Y6","last_seq":{"90001":"42"}}` | 刷新当前连接的 `im:presence:conn:{conn_id}`、`im:presence:user:{user_id}` 及路由集合 TTL。`last_seq` 仅作为补偿提示，不改变业务状态。 |
| `pong` | 服务端 → 客户端 | `{"nonce":"p_01J8K3Y6","server_time":1795687200123,"session_expires_at":"2026-11-26T12:00:00Z"}` | 确认心跳并返回服务端时间和当前 session 到期时间。 |
| `error` | 双向 | `{"code":18005,"message":"conversation access denied","retryable":false,"related_type":"msg.send","related_req_id":"req_01J8K3Y6"}` | 单帧失败响应。错误不会改变业务状态，`related_req_id` 指出失败请求。鉴权或策略失败可以随后发送关闭通知并关闭连接。 |
| `system.notice` | 服务端 → 用户或会话成员 | `{"event":"friend.requested","notice_id":"n_01J8K3Y6","conversation_id":null,"actor_id":"10002","target_user_id":"10001","payload":{"request_id":"70001","message":"hello"}}` | 第 11 类系统通知。覆盖好友申请与处理、群成员变更、撤回通知（`message.recalled`）、会话吊销（`session.revoked`）、账号禁用、被踢和停服。payload 只引用 REST 资源或系统消息，不伪造正文。 |

`system.notice.event` 至少包括 `friend.requested`、`friend.accepted`、`friend.rejected`、`group.member_added`、`group.member_removed`、`group.role_changed`、`group.disbanded`、`message.recalled`、`session.revoked`、`account.kicked`、`account.disabled` 和 `server.draining`。其中 `message.recalled` 仅作实时就地折叠通知（`payload` 含 `recalled_msg_id`、`recall_msg_id`、`recall_seq`、`recalled_at`），持久顺序历史以 `msg.push(push_type="message")` 与 `/sync` 返回的 `recall` 记录为准。`msg.push` 的 `delivery_status` 可为 `sent`、`delivered` 或 `read`，收到 `msg.ack` 绝不等同于送达。

### 状态迁移、断线与 Stream

消息状态只能沿 `sending → sent → delivered → read` 前进。

1. 客户端保留稳定的 `client_msg_id`，把 `msg.send` 放入待确认队列。服务端在会话锁内校准并 `INCR conv:seq:{conversation_id}`，使用 Snowflake 生成 `msg_id`，在一个 GORM 事务中写 messages、会话摘要、成员未读和回执。提交后回 `msg.ack`，客户端进入 `sent`。
2. 事务提交后 `XADD im:stream:messages`。在线成员通过 Consumer Group 得到 `msg.push`，离线成员用 `/api/v1/conversations/{conversation_id}/sync?after_seq=...` 补齐。接收端持久化本地消息后发 `msg.delivered_ack`，服务端推进 `delivered` 并经 `im:stream:events` 通知发送端。
3. 接收端打开会话时发 `msg.read_ack`，也可调用已定义的 HTTP read 接口。服务端只接受不超过会话末 seq 的单调水位，批量更新 receipts、`last_read_seq` 和 `unread_count=0`，再经 events stream 通知发送端，进入 `read`。
4. ACK、Stream 消费和 Hub 投递都必须幂等。重复 ACK 不回退水位，Stream 重投不增加未读。断线重连先用原 client_msg_id 重发未获 ack 的请求，再按各会话 last_seq 增量补偿，最后恢复实时订阅。
5. Redis 去重缓存是快速路径，MySQL `(sender_id, client_msg_id)` 和 `(conversation_id, seq)` 唯一约束是最终事实。`im:dedupe:*`、`im:token:blacklist:*`、`im:presence:user:*`、`im:presence:conn:*`、`im:route:user:*`、`im:route:session:*`（Set，支持多标签页共享 `session_id`）、`im:stream:messages`、`im:stream:events`、Consumer Group、`XACK` 和 `XAUTOCLAIM` 的 TTL、命名与 Pending 补偿语义以 architecture.md 为准。

服务端 close code 使用 1000 表示正常停机，1008 表示策略或鉴权失败，1011 表示内部失败。单连接有界发送队列满时关闭慢端，客户端按 last_seq 补偿，不能阻塞其他连接。

## 统一业务错误码

所有 HTTP 错误和 WS `error` 帧都使用下表中的 code。HTTP 状态码只表示传输层结果，响应信封仍为 `{ "code": 0, "message": "ok", "data": ..., "request_id": "..." }` 的同形结构。`message` 面向调用方，不能泄露密码、Token、磁盘路径或其他用户隐私。

| code | HTTP / WS | 含义 |
| ---: | ---: | --- |
| 0 | 2xx | 成功 |
| 10000 | 400 | 参数格式错误或必填字段缺失 |
| 10001 | 400 | 参数值、枚举或游标不合法 |
| 10002 | 413 | HTTP 请求或 WS 帧超过大小限制 |
| 10003 | 429 | 请求频率超过限制 |
| 11001 | 401 | Access Token 缺失、格式错误或过期 |
| 11002 | 401 | Access Token JTI 已在黑名单 |
| 11003 | 401 | Refresh Token 无效、已使用或已过期 |
| 11004 | 403 | 账号已禁用 |
| 11005 | 403 | session 已撤销或设备不匹配 |
| 11006 | 401 | 用户名或密码错误 |
| 11007 | 409 | 用户名已存在 |
| 11008 | 409 | Refresh 并发轮换冲突，应使用最新 Token |
| 11009 | 409 | 设备会话数超过配置上限 |
| 12001 | 404 | 用户不存在 |
| 12002 | 409 | 不能向自己发好友申请 |
| 12003 | 409 | 已经是好友 |
| 12004 | 409 | 已有待处理好友申请 |
| 12005 | 404 | 好友申请不存在 |
| 12006 | 409 | 好友申请已经处理 |
| 12007 | 403 | 不是好友或好友关系已删除 |
| 12008 | 404 | 好友关系不存在 |
| 13001 | 404 | 会话不存在 |
| 13002 | 403 | 不是会话成员 |
| 13003 | 409 | 会话已隐藏或已解散（仅阻断发送等写入操作，不影响原成员按会话 ID 补拉历史与已读同步） |
| 13004 | 400 | seq 游标格式错误 |
| 13005 | 409 | delivered/read 水位不能回退 |
| 13006 | 400 | 水位超过会话当前最大 seq |
| 14001 | 409 | client_msg_id 已被其他消息内容占用 |
| 14002 | 404 | 消息不存在 |
| 14003 | 403 | 无权发送、撤回或查看消息 |
| 14004 | 409 | 消息已经撤回 |
| 14005 | 409 | 已超过撤回时间窗口 |
| 14006 | 400 | 消息类型、内容或附件不匹配 |
| 14007 | 409 | 消息仍在处理，按原 client_msg_id 重试 |
| 14008 | 503 | 消息已持久化但事件流暂不可用，可安全重试 |
| 15001 | 404 | 群不存在 |
| 15002 | 403 | 不是群成员 |
| 15003 | 403 | 群角色无权执行操作 |
| 15004 | 409 | 群已解散 |
| 15005 | 409 | 群已满 |
| 15006 | 409 | 用户已经是群成员 |
| 15007 | 404 | 群成员不存在 |
| 15008 | 409 | 群主必须先转让或解散 |
| 15009 | 400 | 不能把群主降为普通成员 |
| 15010 | 409 | 目标用户不满足转让条件 |
| 16001 | 400 | 文件字段缺失或 MIME 不允许 |
| 16002 | 413 | 文件超过大小或像素限制 |
| 16003 | 422 | 文件内容与声明类型不符 |
| 16004 | 403 | 没有资产或消息下载权限 |
| 16005 | 404 | 资产不存在或已清理 |
| 16006 | 409 | 资产已经被消息引用，不能删除 |
| 17001 | 403 | 需要平台管理员角色 |
| 17002 | 404 | 管理对象不存在 |
| 17003 | 409 | 目标账号或群已经是目标状态 |
| 17004 | 409 | 不能禁用当前唯一管理员 |
| 17005 | 400 | 审计过滤器或时间范围不合法 |
| 17006 | 503 | 监控依赖不可用 |
| 18001 | 401 | WS 握手鉴权失败 |
| 18002 | 400 | WS Envelope、req_id 或 timestamp 无效 |
| 18003 | 400 | 未知 WS 帧类型 |
| 18004 | 409 | WS 帧违反当前消息状态 |
| 18005 | 403 | WS 连接无权访问会话 |
| 18006 | 1008 | WS session 被撤销、账号被禁用或被踢 |
| 18007 | 1011 | WS 内部处理失败，客户端按游标补偿 |
| 19001 | 503 | MySQL 不可用 |
| 19002 | 503 | Redis 不可用 |
| 19003 | 503 | Redis Stream Pending 超过告警阈值 |
| 19004 | 500 | 数据一致性约束冲突，记录 request_id 后重试 |

客户端只能对明确可恢复的 14007、14008、19001、19002、18007 重试，并且必须保留原 client_msg_id。18006 需要重新登录或等待账号解禁，13005、13006 和 14001 不可盲目重试。
