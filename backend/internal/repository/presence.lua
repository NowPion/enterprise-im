local op, user, session, device, node, conn = unpack(ARGV, 1, 6)
local ttl, routeTTL, deadlineMember, channel = unpack(ARGV, 7, 10)
local member = node .. ':' .. conn
local clock = redis.call('TIME')
local now = clock[1] * 1000 + math.floor(clock[2] / 1000)
local owner = redis.call('HMGET', KEYS[2], 'user_id', 'session_id', 'device_id', 'node_id', 'conn_id')

if owner[1] then
    if owner[1] ~= user or owner[2] ~= session or owner[4] ~= node or owner[5] ~= conn then return -1 end
    if op == 'expire' then return 0 end
    if owner[3] ~= device then return -1 end
elseif op == 'ping' then
    return -1
end

if op == 'open' or op == 'ping' then
    redis.call('HSET', KEYS[2], 'user_id', user, 'session_id', session, 'device_id', device,
               'node_id', node, 'conn_id', conn, 'last_seen_at', now)
    redis.call('PEXPIRE', KEYS[2], ttl)
    redis.call('SADD', KEYS[3], member)
    redis.call('SADD', KEYS[4], member)
    redis.call('PEXPIRE', KEYS[3], routeTTL)
    redis.call('PEXPIRE', KEYS[4], routeTTL)
    redis.call('ZADD', KEYS[5], now + ttl, deadlineMember)
else
    redis.call('DEL', KEYS[2])
    redis.call('SREM', KEYS[3], member)
    redis.call('SREM', KEYS[4], member)
    redis.call('ZREM', KEYS[5], deadlineMember)
end

local devices, count, distinct = {}, 0, 0
for _, route in ipairs(redis.call('SMEMBERS', KEYS[3])) do
    local connID = string.match(route, '([^:]+)$')
    local fields = redis.call('HMGET', 'im:presence:conn:' .. connID, 'user_id', 'device_id', 'node_id')
    if fields[1] == user and fields[2] and fields[3] and fields[3] .. ':' .. connID == route then
        count = count + 1
        if not devices[fields[2]] then
            devices[fields[2]] = true
            distinct = distinct + 1
        end
    end
end

redis.call('SET', KEYS[1], count, 'PX', ttl)
local previous = redis.call('HGET', KEYS[6], user)
local state = count .. ':' .. distinct

local function publish(devicesOnline)
    if devicesOnline == 0 then redis.call('ZADD', KEYS[7], now, user) end
    redis.call('PUBLISH', channel, cjson.encode({user_id=user, device_count=devicesOnline, at=now}))
end

if op == 'open' and not owner[1] and count == 1 and previous then
    redis.call('ZADD', KEYS[7], now, user)
    previous = false
end
if count == 0 then
    redis.call('HDEL', KEYS[6], user)
elseif previous ~= state then
    redis.call('HSET', KEYS[6], user, state)
end
if previous ~= state and (count > 0 or previous) then
    publish(distinct)
    if count == 0 then return now end
end
return 0
