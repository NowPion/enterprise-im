-- =============================================================================
-- Enterprise IM (企业级即时通讯系统) MySQL 8.0 初始化建表脚本
-- 规范基线: PROJECT_GOALS.md §六、docs/architecture.md、docs/api.md
-- 存储引擎: InnoDB | 默认字符集: utf8mb4 | 默认排序规则: utf8mb4_0900_ai_ci
--
-- 包含表清单（共 12 张表）:
--   核心十表:
--     1.  users                 用户表
--     2.  user_sessions         用户多端登录会话表
--     3.  friend_requests       好友申请记录表
--     4.  friendships           好友关系表
--     5.  groups                群组表
--     6.  group_members         群成员表
--     7.  conversations         会话表
--     8.  conversation_members  会话成员表
--     9.  messages              消息表（Snowflake 非自增主键）
--     10. message_receipts      消息送达和已读回执表
--   第二阶段支撑表:
--     11. assets                上传资产元数据与引用关系表
--     12. audit_logs            不可变审计日志表（append-only）
--
-- 默认本地开发管理员种子（仅限本地开发与联调使用，非生产密钥）:
--   username: admin
--   password: Admin@123456  (12 UTF-8 字节，满足 API 8-72 字节密码约定)
--   password_hash: 使用真实 bcrypt (prefix=2a, cost=10) 生成
-- =============================================================================

SET NAMES utf8mb4;

CREATE DATABASE IF NOT EXISTS `enterprise_im`
  DEFAULT CHARACTER SET utf8mb4
  DEFAULT COLLATE utf8mb4_0900_ai_ci;

USE `enterprise_im`;

-- 1. 用户表
CREATE TABLE IF NOT EXISTS `users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '用户ID',
  `username` VARCHAR(64) NOT NULL COMMENT '登录用户名（唯一）',
  `password_hash` VARCHAR(255) NOT NULL COMMENT 'bcrypt 密码哈希',
  `nickname` VARCHAR(64) NOT NULL COMMENT '展示昵称',
  `avatar_url` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '头像地址',
  `signature` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '个性签名',
  `role` ENUM('user', 'admin') NOT NULL DEFAULT 'user' COMMENT '用户角色',
  `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态：1正常 0禁用',
  `last_online_at` DATETIME(3) NULL DEFAULT NULL COMMENT '最后在线时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_users_username` (`username`),
  KEY `idx_users_nickname` (`nickname`),
  KEY `idx_users_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='用户表';

-- 2. 用户多端会话表（Refresh Token 与多端设备管理）
CREATE TABLE IF NOT EXISTS `user_sessions` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '会话记录ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
  `device_id` VARCHAR(128) NOT NULL COMMENT '客户端设备唯一标识',
  `device_type` ENUM('web', 'desktop', 'mobile') NOT NULL DEFAULT 'web' COMMENT '终端类型',
  `refresh_token_hash` CHAR(64) NOT NULL COMMENT 'Refresh Token SHA-256 哈希',
  `access_jti` VARCHAR(64) NOT NULL COMMENT '当前有效 Access Token JTI',
  `ip_address` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '登录IP',
  `user_agent` VARCHAR(512) NOT NULL DEFAULT '' COMMENT 'User-Agent',
  `expires_at` DATETIME(3) NOT NULL COMMENT 'Refresh Token 过期时间',
  `revoked_at` DATETIME(3) NULL DEFAULT NULL COMMENT '主动注销或强制下线时间',
  `last_active_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_sessions_refresh_hash` (`refresh_token_hash`),
  KEY `idx_sessions_user_device` (`user_id`, `device_id`),
  KEY `idx_sessions_user_revoked_exp` (`user_id`, `revoked_at`, `expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='用户多端登录会话表';

-- 3. 好友申请表
CREATE TABLE IF NOT EXISTS `friend_requests` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '申请ID',
  `from_user_id` BIGINT UNSIGNED NOT NULL COMMENT '申请人ID',
  `to_user_id` BIGINT UNSIGNED NOT NULL COMMENT '目标用户ID',
  `message` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '验证消息',
  `status` ENUM('pending', 'accepted', 'rejected', 'canceled') NOT NULL DEFAULT 'pending' COMMENT '处理状态',
  `handled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '处理时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_fr_to_status_created` (`to_user_id`, `status`, `created_at` DESC),
  KEY `idx_fr_from_to_status` (`from_user_id`, `to_user_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='好友申请记录表';

-- 4. 好友关系表（双向两条记录存储，独立支持备注且避免 OR 扫描）
CREATE TABLE IF NOT EXISTS `friendships` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '关系ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '归属用户ID',
  `friend_id` BIGINT UNSIGNED NOT NULL COMMENT '好友用户ID',
  `remark` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '好友备注',
  `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1正常好友 0已删除',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_friendships_pair` (`user_id`, `friend_id`),
  KEY `idx_friendships_user_status` (`user_id`, `status`),
  KEY `idx_friendships_friend_status` (`friend_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='好友关系表';

-- 5. 群组表
CREATE TABLE IF NOT EXISTS `groups` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '群组ID',
  `name` VARCHAR(100) NOT NULL COMMENT '群名称',
  `avatar_url` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '群头像',
  `owner_id` BIGINT UNSIGNED NOT NULL COMMENT '群主用户ID',
  `announcement` TEXT NULL COMMENT '群公告',
  `max_members` INT UNSIGNED NOT NULL DEFAULT 500 COMMENT '群人数上限',
  `status` ENUM('active', 'disbanded') NOT NULL DEFAULT 'active' COMMENT '群状态',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_groups_owner` (`owner_id`),
  KEY `idx_groups_status_created` (`status`, `created_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='群组表';

-- 6. 群成员表
CREATE TABLE IF NOT EXISTS `group_members` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '记录ID',
  `group_id` BIGINT UNSIGNED NOT NULL COMMENT '群组ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '成员用户ID',
  `role` ENUM('owner', 'admin', 'member') NOT NULL DEFAULT 'member' COMMENT '群内角色',
  `nickname_in_group` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '群内昵称',
  `joined_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_gm_group_user` (`group_id`, `user_id`),
  KEY `idx_gm_user_group` (`user_id`, `group_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='群成员表';

-- 7. 会话表（统一抽象单聊与群聊）
CREATE TABLE IF NOT EXISTS `conversations` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '会话ID',
  `type` ENUM('single', 'group') NOT NULL COMMENT '会话类型',
  `direct_key` VARCHAR(64) NULL DEFAULT NULL COMMENT '单聊唯一键：min(u1,u2):max(u1,u2)，群聊为NULL',
  `group_id` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '群聊关联群ID，单聊为NULL',
  `last_msg_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最后一条消息Snowflake ID',
  `last_seq` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '当前会话最大序号seq',
  `last_msg_preview` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最后一条消息预览文案',
  `last_msg_at` DATETIME(3) NULL DEFAULT NULL COMMENT '最后一条消息时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_conv_direct_key` (`direct_key`),
  UNIQUE KEY `uk_conv_group_id` (`group_id`),
  KEY `idx_conv_last_msg_at` (`last_msg_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='会话表';

-- 8. 会话成员表（承载未读数、送达/已读水位线与会话排序）
CREATE TABLE IF NOT EXISTS `conversation_members` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '记录ID',
  `conversation_id` BIGINT UNSIGNED NOT NULL COMMENT '会话ID',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '用户ID',
  `last_delivered_seq` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '该用户已送达最大seq',
  `last_read_seq` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '该用户已读最大seq',
  `unread_count` INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '未读消息数',
  `is_pinned` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否置顶',
  `is_muted` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否免打扰',
  `is_visible` TINYINT(1) NOT NULL DEFAULT 1 COMMENT '会话列表是否可见',
  `joined_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_cm_conv_user` (`conversation_id`, `user_id`),
  KEY `idx_cm_user_visible_updated` (`user_id`, `is_visible`, `is_pinned` DESC, `updated_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='会话成员表';

-- 9. 消息表（核心大表：按 conversation_id + seq 强索引优化）
CREATE TABLE IF NOT EXISTS `messages` (
  `id` BIGINT UNSIGNED NOT NULL COMMENT '服务端生成的 Snowflake 全局唯一消息ID',
  `conversation_id` BIGINT UNSIGNED NOT NULL COMMENT '所属会话ID',
  `seq` BIGINT UNSIGNED NOT NULL COMMENT '会话内单调递增序号',
  `sender_id` BIGINT UNSIGNED NOT NULL COMMENT '发送者用户ID（系统消息为0）',
  `client_msg_id` VARCHAR(64) NOT NULL COMMENT '客户端幂等消息ID',
  `msg_type` ENUM('text', 'image', 'file', 'voice', 'recall', 'system') NOT NULL COMMENT '消息类型',
  `content` TEXT NOT NULL COMMENT '消息文本内容或展示摘要',
  `extra` JSON NULL COMMENT '扩展元数据（文件URL/大小/时长/被撤回msg_id等）',
  `status` ENUM('normal', 'recalled') NOT NULL DEFAULT 'normal' COMMENT '消息状态',
  `recalled_at` DATETIME(3) NULL DEFAULT NULL COMMENT '撤回时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_msg_conv_seq` (`conversation_id`, `seq`),
  UNIQUE KEY `uk_msg_sender_client` (`sender_id`, `client_msg_id`),
  KEY `idx_msg_conv_created` (`conversation_id`, `created_at` DESC),
  KEY `idx_msg_sender_created` (`sender_id`, `created_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='消息表';

-- 10. 消息送达与已读回执表
CREATE TABLE IF NOT EXISTS `message_receipts` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '回执ID',
  `message_id` BIGINT UNSIGNED NOT NULL COMMENT '消息Snowflake ID',
  `conversation_id` BIGINT UNSIGNED NOT NULL COMMENT '所属会话ID',
  `seq` BIGINT UNSIGNED NOT NULL COMMENT '对应消息seq',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '接收方用户ID',
  `status` ENUM('sent', 'delivered', 'read') NOT NULL DEFAULT 'sent' COMMENT '回执状态',
  `delivered_at` DATETIME(3) NULL DEFAULT NULL COMMENT '送达时间',
  `read_at` DATETIME(3) NULL DEFAULT NULL COMMENT '已读时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_receipt_msg_user` (`message_id`, `user_id`),
  KEY `idx_receipt_conv_user_seq` (`conversation_id`, `user_id`, `seq`),
  KEY `idx_receipt_conv_user_status` (`conversation_id`, `user_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='消息送达和已读回执表';

-- 11. 上传资产表（支撑媒体元数据持久化、受权访问控制与消息引用删除拦截）
CREATE TABLE IF NOT EXISTS `assets` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '内部自增主键ID',
  `asset_id` CHAR(36) NOT NULL COMMENT '上传服务生成的对外资产UUID',
  `owner_id` BIGINT UNSIGNED NOT NULL COMMENT '上传所有者用户ID',
  `purpose` ENUM('image', 'file', 'voice', 'avatar', 'group_avatar') NOT NULL COMMENT '资产用途',
  `filename` VARCHAR(255) NOT NULL COMMENT '清洗后的安全原始文件名',
  `mime` VARCHAR(128) NOT NULL COMMENT '服务端校验后的真实MIME类型',
  `size_bytes` BIGINT UNSIGNED NOT NULL COMMENT '原件字节大小',
  `sha256` CHAR(64) NOT NULL DEFAULT '' COMMENT '原件内容SHA-256摘要',
  `width` INT UNSIGNED NULL DEFAULT NULL COMMENT '图片或头像宽度（px）',
  `height` INT UNSIGNED NULL DEFAULT NULL COMMENT '图片或头像高度（px）',
  `duration_ms` INT UNSIGNED NULL DEFAULT NULL COMMENT '语音时长（ms）',
  `storage_key` VARCHAR(512) NOT NULL COMMENT '内部相对存储定位键（禁止作为对外API字段暴露）',
  `thumbnail_key` VARCHAR(512) NULL DEFAULT NULL COMMENT '缩略图内部相对存储定位键（禁止作为对外API字段暴露）',
  `thumbnail_mime` VARCHAR(128) NULL DEFAULT NULL COMMENT '缩略图MIME类型',
  `thumbnail_size_bytes` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '缩略图字节大小',
  `ref_type` ENUM('none', 'message', 'user_avatar', 'group_avatar') NOT NULL DEFAULT 'none' COMMENT '业务引用类型：none未引用 message消息引用 user_avatar用户头像 group_avatar群头像',
  `ref_id` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '主引用对象ID（messages.id / users.id / groups.id）',
  `ref_conversation_id` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '当ref_type=message时记录所属会话ID，用于成员下载权限校验',
  `ref_count` INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '当前有效业务引用计数（>0且为消息引用时拒绝删除）',
  `bound_at` DATETIME(3) NULL DEFAULT NULL COMMENT '首次被业务引用时间',
  `status` ENUM('active', 'deleted') NOT NULL DEFAULT 'active' COMMENT '资产状态：active可用 deleted已清理',
  `deleted_at` DATETIME(3) NULL DEFAULT NULL COMMENT '清理或删除时间',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_assets_asset_id` (`asset_id`),
  KEY `idx_assets_owner_status_created` (`owner_id`, `status`, `created_at` DESC),
  KEY `idx_assets_ref` (`ref_type`, `ref_id`),
  KEY `idx_assets_ref_conv` (`ref_conversation_id`),
  KEY `idx_assets_status_ref_created` (`status`, `ref_type`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='上传资产元数据与引用关系表';

-- 12. 不可变审计日志表（append-only：仅允许追加写入，禁止业务UPDATE或DELETE）
CREATE TABLE IF NOT EXISTS `audit_logs` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '审计事件自增主键（append-only顺序键）',
  `event_id` VARCHAR(64) NOT NULL COMMENT '全局唯一审计事件ID（用于im-event-audit消费者幂等去重）',
  `scope` ENUM('admin', 'group', 'system') NOT NULL COMMENT '审计域：admin平台管理 group群管理 system系统事件',
  `action` VARCHAR(64) NOT NULL COMMENT '审计动作编码，如user.disable/group.disband/group.member_remove',
  `actor_id` BIGINT UNSIGNED NOT NULL COMMENT '操作者用户ID（系统触发事件为0）',
  `actor_role` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '操作者当时角色快照（admin/owner/group_admin/user/system）',
  `target_type` ENUM('user', 'session', 'group', 'conversation', 'message', 'asset', 'system') NOT NULL COMMENT '目标对象类型',
  `target_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '目标对象数字ID',
  `group_id` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '关联群组ID（支撑群管理/audit分页过滤）',
  `conversation_id` BIGINT UNSIGNED NULL DEFAULT NULL COMMENT '关联会话ID（支撑会话/消息审计过滤）',
  `reason` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '操作原因或备注',
  `detail` JSON NULL COMMENT '结构化事件详情（变更前后摘要、踢线数量、检索过滤参数等）',
  `request_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '关联HTTP请求或WS链路追踪ID',
  `ip_address` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '操作来源IP',
  `user_agent` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '操作来源User-Agent',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '事件创建时间（不可变，无updated_at）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_audit_event_id` (`event_id`),
  KEY `idx_audit_group_created` (`group_id`, `created_at` DESC, `id` DESC),
  KEY `idx_audit_scope_action_created` (`scope`, `action`, `created_at` DESC),
  KEY `idx_audit_actor_created` (`actor_id`, `created_at` DESC),
  KEY `idx_audit_target_created` (`target_type`, `target_id`, `created_at` DESC),
  KEY `idx_audit_conv_created` (`conversation_id`, `created_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='不可变审计日志表（append-only：禁止业务更新或删除）';

-- 默认本地开发管理员种子（username=admin, password=Admin@123456, bcrypt cost=10）
INSERT IGNORE INTO `users` (
  `username`,
  `password_hash`,
  `nickname`,
  `avatar_url`,
  `signature`,
  `role`,
  `status`
) VALUES (
  'admin',
  '$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82',
  '系统管理员',
  '',
  'Enterprise IM 默认本地开发管理员',
  'admin',
  1
);
