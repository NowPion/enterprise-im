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
| `Asset` | `asset_id, purpose, url, filename, mime, size_bytes`。图片与头像另含 `width, height, thumbnail_url`，语音另含 `duration_ms`。url 是同源受权资源路径，size_bytes 表示原件字节数。 |

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
| GET | `/api/v1/conversations/{conversation_id}/members` | 会话成员 | 分页 | 成员 User、群角色、群昵称、online |
| GET | `/api/v1/conversations/{conversation_id}/messages` | 会话成员 | before_seq 或 after_seq 二选一、limit | Message 页。before 按 seq 倒查后以升序返回 |
| GET | `/api/v1/conversations/{conversation_id}/sync` | 会话成员 | after_seq、limit | `{items, from_seq, to_seq, has_more}`，只取 seq > after_seq |
| POST | `/api/v1/conversations/{conversation_id}/messages` | 会话成员 | client_msg_id、msg_type、content、extra | 与 WS msg.ack 相同的 Message，状态为 sent |
| POST | `/api/v1/conversations/{conversation_id}/messages/{msg_id}/recall` | 原发送者 | 无 | 更新后的原 Message、recall Message |
| POST | `/api/v1/conversations/{conversation_id}/read` | 会话成员 | last_read_seq | last_read_seq、unread_count=0、read_at |
| POST | `/api/v1/conversations/{conversation_id}/delivered` | 会话成员 | last_delivered_seq | 更新后的送达水位，供离线补偿使用 |

会话列表从本人 conversation_members 联表取得，单聊按 direct_key 唯一，群按 group_id 唯一。服务端用 IN 批量装配对端、群资料和在线状态，不在循环中查询。消息历史使用 `(conversation_id, seq)` 索引，禁止 OFFSET 深分页。`before_seq` 查询 `seq < before_seq`，`after_seq` 和 sync 查询 `seq > after_seq`。服务端拒绝超过会话当前末 seq 的水位，重复或较小水位只返回当前值。

HTTP 消息发送和 WS `msg.send` 共用 `(sender_id, client_msg_id)` 唯一约束。重试必须使用原 client_msg_id，服务端返回已有 msg_id、seq 和当前 delivery_status，不产生第二条消息。撤回只允许发送者在配置的时间窗内执行。`extra` 只接受与 msg_type 匹配的已上传资产引用。

### 上传与媒体

| 方法 | 路径 | 鉴权 | 请求 | 成功 data |
| --- | --- | --- | --- | --- |
| POST | `/api/v1/uploads` | 用户 | multipart 的 file、purpose=image\|file\|voice\|avatar\|group_avatar | Asset，包含 asset_id、url、download_url、filename、mime、size_bytes 和媒体元数据 |
| GET | `/api/v1/uploads/{asset_id}` | 资产所有者或消息成员 | 无 | Asset 和短期 download_url |
| GET | `/api/v1/uploads/{asset_id}/download` | 资产所有者或消息成员 | 无 | 二进制文件，Content-Disposition 使用清洗后的文件名 |
| DELETE | `/api/v1/uploads/{asset_id}` | 资产所有者 | 无 | `{asset_id, deleted}`。已被消息引用时拒绝删除 |

上传写入临时文件后检查真实 MIME、内容、大小、像素、音频时长和路径规则，再原子移动到 `uploads/` 卷。图片生成缩略图，语音保存 duration_ms。只有消息、头像或群资料引用资产后才允许相关资源读取，下载不泄露本地路径。删除资产不会删除已提交消息。

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
| GET | `/api/v1/admin/users` | 平台管理员 | q、status、role、分页 | User 页，含 session_count、online |
| GET | `/api/v1/admin/users/{user_id}` | 平台管理员 | 无 | 用户详情、设备会话、好友/群计数和审计摘要 |
| PATCH | `/api/v1/admin/users/{user_id}/status` | 平台管理员 | status=active\|disabled、reason | 用户状态、revoked_session_count、kicked_connection_count |
| POST | `/api/v1/admin/users/{user_id}/kick` | 平台管理员 | 可选 session_id、reason | 撤销 session 数和关闭连接数 |
| GET | `/api/v1/admin/groups` | 平台管理员 | q、status、分页 | 群摘要页，含 owner、member_count、last_seq |
| GET | `/api/v1/admin/groups/{group_id}` | 平台管理员 | 无 | 群详情、成员页和系统事件摘要 |
| DELETE | `/api/v1/admin/groups/{group_id}` | 平台管理员 | reason | status=disbanded 和审计记录 |
| GET | `/api/v1/admin/messages/audit` | 平台管理员 | conversation_id、sender_id、keyword、from、to、分页 | Message 审计页，含 msg_id、seq、类型、状态、撤回信息 |
| GET | `/api/v1/admin/metrics/overview` | 平台管理员 | 无 | online_users、ws_connections、messages_per_minute、streams、goroutines、heap_bytes |
| GET | `/api/v1/admin/metrics/streams` | 平台管理员 | 无 | stream、group、consumer、last_delivered_id、pending_count、oldest_pending_age |
| GET | `/api/v1/admin/health/dependencies` | 平台管理员 | 无 | MySQL/Redis 状态、延迟、版本和最近错误时间 |

后台接口统一产生不可变审计事件。禁用用户在事务内写 users.status、撤销全部 user_sessions 并写 Token 黑名单，提交后通过 `system.notice` 的 account.disabled 事件关闭所有连接。启用只恢复登录资格。后台审计按会话、发送者、关键词和时间范围分页，超过允许范围返回业务错误。

## HTTP 错误处理

错误仍使用 `{code, message, data, request_id}`。HTTP 状态表示传输层结果，客户端按业务 code 分支。可恢复消息错误必须保留原 client_msg_id，并在重连后按会话 seq 校验。
