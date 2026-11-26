package errcode

import (
	"fmt"
	"net/http"
	"sort"
)

// Error represents a frozen business error code mapped to transport status and safe client message.
type Error struct {
	Code        int    `json:"code"`
	HTTPStatus  int    `json:"-"`
	WSCloseCode int    `json:"-"`
	Message     string `json:"message"`
}

// Error implements the standard error interface without leaking sensitive runtime context.
func (e *Error) Error() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// WithMessage returns a shallow copy with a custom client-safe message while keeping the frozen code and status.
func (e *Error) WithMessage(msg string) *Error {
	if msg == "" {
		return e
	}
	cp := *e
	cp.Message = msg
	return &cp
}

var registry = make(map[int]*Error, 72)

func mustRegister(code, httpStatus, wsCloseCode int, msg string) *Error {
	if _, exists := registry[code]; exists {
		panic(fmt.Sprintf("duplicate errcode registration: %d", code))
	}
	e := &Error{
		Code:        code,
		HTTPStatus:  httpStatus,
		WSCloseCode: wsCloseCode,
		Message:     msg,
	}
	registry[code] = e
	return e
}

// Lookup returns the registered Error definition for a business code.
func Lookup(code int) (*Error, bool) {
	e, ok := registry[code]
	return e, ok
}

// All returns every registered frozen Error ordered by code ascending.
func All() []*Error {
	out := make([]*Error, 0, len(registry))
	for _, e := range registry {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Code < out[j].Code
	})
	return out
}

// Frozen business code table aligned strictly with docs/api.md (0 and 10000-19004).
var (
	// 0: 成功
	OKCode = mustRegister(0, http.StatusOK, 0, "ok")

	// 10xxx: 通用请求与限流错误
	ErrInvalidParams   = mustRegister(10000, http.StatusBadRequest, 0, "invalid request format or missing required field")         // 参数格式错误或必填字段缺失
	ErrInvalidValue    = mustRegister(10001, http.StatusBadRequest, 0, "invalid parameter value, enum, or cursor")              // 参数值、枚举或游标不合法
	ErrPayloadTooLarge = mustRegister(10002, http.StatusRequestEntityTooLarge, 0, "request or frame exceeds size limit")        // HTTP 请求或 WS 帧超过大小限制
	ErrRateLimited     = mustRegister(10003, http.StatusTooManyRequests, 0, "rate limit exceeded")                              // 请求频率超过限制

	// 11xxx: 认证、会话与账号状态错误
	ErrAccessTokenInvalid  = mustRegister(11001, http.StatusUnauthorized, 0, "access token missing, malformed, or expired")     // Access Token 缺失、格式错误或过期
	ErrAccessTokenBlacklisted = mustRegister(11002, http.StatusUnauthorized, 0, "access token revoked")                         // Access Token JTI 已在黑名单
	ErrRefreshTokenInvalid = mustRegister(11003, http.StatusUnauthorized, 0, "refresh token invalid, used, or expired")         // Refresh Token 无效、已使用或已过期
	ErrAccountDisabled     = mustRegister(11004, http.StatusForbidden, 0, "account disabled")                                   // 账号已禁用
	ErrSessionRevoked      = mustRegister(11005, http.StatusForbidden, 0, "session revoked or device mismatch")                 // session 已撤销或设备不匹配
	ErrInvalidCredentials  = mustRegister(11006, http.StatusUnauthorized, 0, "invalid username or password")                    // 用户名或密码错误
	ErrUsernameExists      = mustRegister(11007, http.StatusConflict, 0, "username already exists")                             // 用户名已存在
	ErrRefreshConflict     = mustRegister(11008, http.StatusConflict, 0, "refresh token rotation conflict")                     // Refresh 并发轮换冲突，应使用最新 Token
	ErrSessionLimitReached = mustRegister(11009, http.StatusConflict, 0, "active device session limit exceeded")                // 设备会话数超过配置上限

	// 12xxx: 用户与好友关系错误
	ErrUserNotFound          = mustRegister(12001, http.StatusNotFound, 0, "user not found")                                    // 用户不存在
	ErrFriendRequestSelf     = mustRegister(12002, http.StatusConflict, 0, "cannot send friend request to self")                // 不能向自己发好友申请
	ErrAlreadyFriends        = mustRegister(12003, http.StatusConflict, 0, "already friends")                                   // 已经是好友
	ErrFriendRequestPending  = mustRegister(12004, http.StatusConflict, 0, "pending friend request already exists")             // 已有待处理好友申请
	ErrFriendRequestNotFound = mustRegister(12005, http.StatusNotFound, 0, "friend request not found")                          // 好友申请不存在
	ErrFriendRequestHandled  = mustRegister(12006, http.StatusConflict, 0, "friend request already handled")                    // 好友申请已经处理
	ErrNotFriends            = mustRegister(12007, http.StatusForbidden, 0, "not friends or friendship removed")                // 不是好友或好友关系已删除
	ErrFriendshipNotFound    = mustRegister(12008, http.StatusNotFound, 0, "friendship not found")                              // 好友关系不存在

	// 13xxx: 会话与游标水位错误
	ErrConversationNotFound = mustRegister(13001, http.StatusNotFound, 0, "conversation not found")                             // 会话不存在
	ErrNotConversationMember = mustRegister(13002, http.StatusForbidden, 0, "not a conversation member")                        // 不是会话成员
	ErrConversationReadOnly = mustRegister(13003, http.StatusConflict, 0, "conversation hidden or disbanded")                   // 会话已隐藏或已解散
	ErrInvalidSeqCursor     = mustRegister(13004, http.StatusBadRequest, 0, "invalid seq cursor format")                        // seq 游标格式错误
	ErrWatermarkRollback    = mustRegister(13005, http.StatusConflict, 0, "watermark cannot move backward")                     // delivered/read 水位不能回退
	ErrWatermarkExceedsSeq  = mustRegister(13006, http.StatusBadRequest, 0, "watermark exceeds conversation last_seq")          // 水位超过会话当前最大 seq

	// 14xxx: 消息与幂等错误
	ErrClientMsgIDConflict  = mustRegister(14001, http.StatusConflict, 0, "client_msg_id already used with different payload")  // client_msg_id 已被其他消息内容占用
	ErrMessageNotFound      = mustRegister(14002, http.StatusNotFound, 0, "message not found")                                  // 消息不存在
	ErrMessageAccessDenied  = mustRegister(14003, http.StatusForbidden, 0, "message access denied")                             // 无权发送、撤回或查看消息
	ErrMessageAlreadyRecalled = mustRegister(14004, http.StatusConflict, 0, "message already recalled")                         // 消息已经撤回
	ErrRecallWindowExpired  = mustRegister(14005, http.StatusConflict, 0, "message recall window expired")                      // 已超过撤回时间窗口
	ErrInvalidMessagePayload = mustRegister(14006, http.StatusBadRequest, 0, "invalid message type, content, or attachment")    // 消息类型、内容或附件不匹配
	ErrMessageInProgress    = mustRegister(14007, http.StatusConflict, 0, "message is being processed, retry with same client_msg_id") // 消息仍在处理，按原 client_msg_id 重试
	ErrStreamUnavailable    = mustRegister(14008, http.StatusServiceUnavailable, 0, "message persisted but event stream temporarily unavailable") // 消息已持久化但事件流暂不可用，可安全重试

	// 15xxx: 群聊与群成员错误
	ErrGroupNotFound        = mustRegister(15001, http.StatusNotFound, 0, "group not found")                                    // 群不存在
	ErrNotGroupMember       = mustRegister(15002, http.StatusForbidden, 0, "not a group member")                                // 不是群成员
	ErrGroupRoleDenied      = mustRegister(15003, http.StatusForbidden, 0, "group role permission denied")                      // 群角色无权执行操作
	ErrGroupDisbanded       = mustRegister(15004, http.StatusConflict, 0, "group disbanded")                                    // 群已解散
	ErrGroupFull            = mustRegister(15005, http.StatusConflict, 0, "group member limit reached")                         // 群已满
	ErrAlreadyGroupMember   = mustRegister(15006, http.StatusConflict, 0, "user already a group member")                        // 用户已经是群成员
	ErrGroupMemberNotFound  = mustRegister(15007, http.StatusNotFound, 0, "group member not found")                             // 群成员不存在
	ErrOwnerLeaveForbidden  = mustRegister(15008, http.StatusConflict, 0, "owner must transfer ownership or disband group first") // 群主必须先转让或解散
	ErrCannotDemoteOwner    = mustRegister(15009, http.StatusBadRequest, 0, "cannot demote group owner")                        // 不能把群主降为普通成员
	ErrTransferIneligible   = mustRegister(15010, http.StatusConflict, 0, "target user ineligible for group transfer")          // 目标用户不满足转让条件

	// 16xxx: 上传与媒体资产错误
	ErrUploadInvalidFile    = mustRegister(16001, http.StatusBadRequest, 0, "missing file field or disallowed mime type")       // 文件字段缺失或 MIME 不允许
	ErrUploadTooLarge       = mustRegister(16002, http.StatusRequestEntityTooLarge, 0, "file exceeds size or dimension limit")  // 文件超过大小或像素限制
	ErrUploadTypeMismatch   = mustRegister(16003, http.StatusUnprocessableEntity, 0, "file content does not match declared type") // 文件内容与声明类型不符
	ErrAssetAccessDenied    = mustRegister(16004, http.StatusForbidden, 0, "asset download access denied")                      // 没有资产或消息下载权限
	ErrAssetNotFound        = mustRegister(16005, http.StatusNotFound, 0, "asset not found or cleaned up")                      // 资产不存在或已清理
	ErrAssetReferenced      = mustRegister(16006, http.StatusConflict, 0, "asset referenced by message cannot be deleted")      // 资产已经被消息引用，不能删除

	// 17xxx: 平台管理与审计错误
	ErrAdminRequired        = mustRegister(17001, http.StatusForbidden, 0, "platform admin role required")                      // 需要平台管理员角色
	ErrAdminTargetNotFound  = mustRegister(17002, http.StatusNotFound, 0, "target resource not found")                          // 管理对象不存在
	ErrAdminStateConflict   = mustRegister(17003, http.StatusConflict, 0, "target already in requested state")                  // 目标账号或群已经是目标状态
	ErrCannotDisableOnlyAdmin = mustRegister(17004, http.StatusConflict, 0, "cannot disable the only remaining admin")          // 不能禁用当前唯一管理员
	ErrInvalidAuditFilter   = mustRegister(17005, http.StatusBadRequest, 0, "invalid audit filter or time range")               // 审计过滤器或时间范围不合法
	ErrMetricsUnavailable   = mustRegister(17006, http.StatusServiceUnavailable, 0, "monitoring dependency unavailable")          // 监控依赖不可用

	// 18xxx: WebSocket 握手与信令错误
	ErrWSAuthFailed         = mustRegister(18001, http.StatusUnauthorized, 1008, "websocket authentication failed")              // WS 握手鉴权失败
	ErrWSInvalidEnvelope    = mustRegister(18002, http.StatusBadRequest, 0, "invalid websocket envelope, req_id, or timestamp") // WS Envelope、req_id 或 timestamp 无效
	ErrWSUnknownType        = mustRegister(18003, http.StatusBadRequest, 0, "unknown websocket frame type")                     // 未知 WS 帧类型
	ErrWSStateConflict      = mustRegister(18004, http.StatusConflict, 0, "websocket frame conflicts with current state")       // WS 帧违反当前消息状态
	ErrWSConvAccessDenied   = mustRegister(18005, http.StatusForbidden, 0, "websocket conversation access denied")              // WS 连接无权访问会话
	ErrWSSessionRevoked     = mustRegister(18006, http.StatusForbidden, 1008, "websocket session revoked, account disabled, or kicked") // WS session 被撤销、账号被禁用或被踢
	ErrWSInternalError      = mustRegister(18007, http.StatusInternalServerError, 1011, "websocket internal error, sync via cursor") // WS 内部处理失败，客户端按游标补偿

	// 19xxx: 基础设施与数据一致性错误
	ErrMySQLUnavailable     = mustRegister(19001, http.StatusServiceUnavailable, 0, "mysql unavailable")                        // MySQL 不可用
	ErrRedisUnavailable     = mustRegister(19002, http.StatusServiceUnavailable, 0, "redis unavailable")                        // Redis 不可用
	ErrStreamPendingAlert   = mustRegister(19003, http.StatusServiceUnavailable, 0, "redis stream pending count exceeds threshold") // Redis Stream Pending 超过告警阈值
	ErrInternalConsistency  = mustRegister(19004, http.StatusInternalServerError, 0, "internal consistency error, retry with request_id") // 数据一致性约束冲突，记录 request_id 后重试
)
