-- KEYS: upload hash, expiry index, active part lease index.
local upload, expiry, parts = KEYS[1], KEYS[2], KEYS[3]
local action, id = ARGV[1], ARGV[2]
local function phase()
    return redis.call('HGET', upload, 'phase')
end
local function now_ms()
    local time = redis.call('TIME')
    return tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
end
local function exclusive_until()
    return tonumber(redis.call('HGET', upload, 'exclusive_until')) or 0
end
local function owns_exclusive(token, now)
    return redis.call('HGET', upload, 'exclusive_token') == token and exclusive_until() > now
end

if action == 'create' then
    if redis.call('EXISTS', upload) == 1 then return 'conflict' end
    local record, phase_value, purge_at = ARGV[3], ARGV[4], ARGV[5]
    redis.call('ZADD', expiry, purge_at, id)
    local field = phase_value == 'starting' and 'reservation' or 'record'
    redis.call('HSET', upload, field, record, 'phase', phase_value)
    return 'ok'
end

local state = phase()
if not state then return 'missing' end
if action == 'publish' then
    if state ~= 'starting' then return 'conflict' end
    redis.call('HSET', upload, 'record', ARGV[3], 'phase', 'active')
    redis.call('HDEL', upload, 'reservation')
    return 'ok'
end
if action == 'discard-start' then
    if state ~= 'starting' then return 'ok' end
    redis.call('DEL', upload)
    redis.call('ZREM', expiry, id)
    return 'ok'
end
local now = now_ms()

if action == 'lock' then
    local kind, token, part, digest, duration = ARGV[3], ARGV[4], ARGV[5], ARGV[6], tonumber(ARGV[7])
    if exclusive_until() > now then return 'busy' end
    if kind == 'read' then return 'ok:' .. state end
    if kind == 'exclusive' then
        if redis.call('ZCOUNT', parts, '(' .. now, '+inf') > 0 then return 'busy' end
        redis.call('HSET', upload, 'exclusive_token', token, 'exclusive_until', now + duration)
        return 'ok:' .. state
    end
    if state ~= 'active' then return 'conflict' end
    local part_until = tonumber(redis.call('ZSCORE', parts, part)) or 0
    if part_until > now then return 'busy' end
    local previous = redis.call('HGET', upload, 'digest:' .. part)
    if previous and previous ~= digest then return 'conflict' end
    redis.call('HSET', upload, 'digest:' .. part, digest, 'token:' .. part, token)
    redis.call('ZADD', parts, now + duration, part)
    redis.call('PEXPIRE', parts, duration * 2)
    return 'ok:' .. state
end

if action == 'renew' or action == 'release' then
    local kind, token, part = ARGV[3], ARGV[4], ARGV[5]
    local duration = tonumber(ARGV[6])
    if kind == 'part' then
        if redis.call('HGET', upload, 'token:' .. part) ~= token then return 'busy' end
        if action == 'release' then
            redis.call('HDEL', upload, 'token:' .. part)
            redis.call('ZREM', parts, part)
        else
            local part_until = tonumber(redis.call('ZSCORE', parts, part)) or 0
            if part_until <= now or state ~= 'active' then return 'busy' end
            redis.call('ZADD', parts, now + duration, part)
            redis.call('PEXPIRE', parts, duration * 2)
        end
    else
        if redis.call('HGET', upload, 'exclusive_token') ~= token then return 'busy' end
        if action == 'release' then
            redis.call('HDEL', upload, 'exclusive_token', 'exclusive_until')
        else
            if not owns_exclusive(token, now) then return 'busy' end
            redis.call('HSET', upload, 'exclusive_until', now + duration)
        end
    end
    return 'ok'
end

local token = ARGV[3]
if not owns_exclusive(token, now) then return 'busy' end

if action == 'prepare' then
    local next_state, manifest = ARGV[4], ARGV[5]
    if state == next_state then
        if redis.call('HGET', upload, 'manifest') == manifest then return 'ok' end
        return 'conflict'
    end
    if state ~= 'active' then return 'conflict' end
    redis.call('HSET', upload, 'phase', next_state, 'manifest', manifest)
    return 'ok'
end

if action == 'save' then
    if state == 'aborting' then return 'conflict' end
    local record, purge_at = ARGV[4], ARGV[5]
    -- Completed records need neither digests nor leases. Their result must
    -- survive a complete whose HTTP response was lost.
    redis.call('ZADD', expiry, purge_at, id)
    redis.call('HSET', upload, 'record', record, 'phase', 'completed')
    redis.call('DEL', parts)
    for _, field in ipairs(redis.call('HKEYS', upload)) do
        if field ~= 'record' and field ~= 'phase' then
            redis.call('HDEL', upload, field)
        end
    end
    return 'ok'
end

if action == 'remove' then
    if state == 'active' then return 'conflict' end
    redis.call('DEL', upload, parts)
    redis.call('ZREM', expiry, id)
    return 'ok'
end

return 'invalid'
