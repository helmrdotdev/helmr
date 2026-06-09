package schedule

// Schedule Redis messages are keyed by schedule instance. The payload identifies
// the specific fire via generation and scheduled_at, so stale lease finish and
// reclaim paths must only mutate the live message when payloads match.
const scheduleEnqueueScript = `
local ready = KEYS[1]
local prefix = ARGV[1]
local message_id = ARGV[2]
local payload = ARGV[3]
local score = tonumber(ARGV[4])

local message_key = prefix .. ":message:" .. message_id
if redis.call("EXISTS", message_key) == 0 then
  redis.call("HSET", message_key,
    "payload", payload,
    "score", score,
    "attempt", 0
  )
else
  local existing_payload = redis.call("HGET", message_key, "payload")
  redis.call("HSET", message_key,
    "payload", payload,
    "score", score
  )
  if existing_payload ~= payload then
    redis.call("HSET", message_key, "attempt", 0)
  else
    redis.call("HSETNX", message_key, "attempt", 0)
  end
end
redis.call("ZADD", ready, score, message_id)
return 1
`

const scheduleDequeueScript = `
local ready = KEYS[1]
local active = KEYS[2]
local prefix = ARGV[1]
local now_ms = tonumber(ARGV[2])
local lease_ms = tonumber(ARGV[3])
local max_messages = tonumber(ARGV[4])
local reclaim_limit = tonumber(ARGV[5])
local worker_id = ARGV[6]

local function retry_delay_ms(attempt)
  if attempt < 1 then
    attempt = 1
  end
  local delay = attempt * attempt * 60000
  if delay > 3600000 then
    return 3600000
  end
  return delay
end

local expired = redis.call("ZRANGEBYSCORE", active, "-inf", now_ms, "LIMIT", 0, reclaim_limit)
for _, lease_id in ipairs(expired) do
  local lease_key = prefix .. ":lease:" .. lease_id
  local message_id = redis.call("HGET", lease_key, "message_id")
  if message_id then
    local message_key = prefix .. ":message:" .. message_id
    local active_message_key = prefix .. ":message_active:" .. message_id
    local lease_payload = redis.call("HGET", lease_key, "payload")
    local message_payload = redis.call("HGET", message_key, "payload")
    if message_payload and lease_payload == message_payload then
      local attempt = tonumber(redis.call("HGET", message_key, "attempt") or "1")
      local score = now_ms + retry_delay_ms(attempt)
      redis.call("HSET", message_key, "score", score)
      redis.call("ZADD", ready, score, message_id)
    end
    if redis.call("GET", active_message_key) == lease_id then
      redis.call("DEL", active_message_key)
    end
  end
  redis.call("DEL", lease_key)
  redis.call("ZREM", active, lease_id)
end

local due = redis.call("ZRANGEBYSCORE", ready, "-inf", now_ms, "LIMIT", 0, max_messages)
local result = {}
for _, message_id in ipairs(due) do
  redis.call("ZREM", ready, message_id)
  local message_key = prefix .. ":message:" .. message_id
  local active_message_key = prefix .. ":message_active:" .. message_id
  if redis.call("EXISTS", active_message_key) == 1 then
    local score = tonumber(redis.call("HGET", message_key, "score") or "0")
    if score > 0 then
      redis.call("ZADD", ready, score, message_id)
    end
  elseif redis.call("EXISTS", message_key) == 1 then
    local fields = redis.call("HMGET", message_key, "payload", "attempt")
    local payload = fields[1]
    local attempt = redis.call("HINCRBY", message_key, "attempt", 1)
    local lease_id = message_id .. ":lease:" .. tostring(attempt)
    local lease_key = prefix .. ":lease:" .. lease_id
    local expires_at = now_ms + lease_ms
    redis.call("HSET", lease_key,
      "message_id", message_id,
      "payload", payload,
      "worker_id", worker_id,
      "expires_at", expires_at
    )
    redis.call("ZADD", active, expires_at, lease_id)
    redis.call("SET", active_message_key, lease_id, "PX", lease_ms)
    table.insert(result, {lease_id, message_id, payload, attempt})
  end
end
return result
`

const scheduleFinishScript = `
local prefix = ARGV[1]
local lease_id = ARGV[2]
local worker_id = ARGV[3]
local now_ms = tonumber(ARGV[4])
local action = ARGV[5]
local retry_at_ms = tonumber(ARGV[6])
local lease_payload_arg = ARGV[7]

local lease_key = prefix .. ":lease:" .. lease_id
if redis.call("EXISTS", lease_key) == 0 then
  return -1
end
if redis.call("HGET", lease_key, "worker_id") ~= worker_id then
  return -2
end
local message_id = redis.call("HGET", lease_key, "message_id")
local active = prefix .. ":active"
local ready = prefix .. ":ready"
local message_key = prefix .. ":message:" .. message_id
local active_message_key = prefix .. ":message_active:" .. message_id
local lease_payload = redis.call("HGET", lease_key, "payload") or lease_payload_arg
local message_payload = redis.call("HGET", message_key, "payload")
local lease_matches_message = message_payload and lease_payload == message_payload
redis.call("ZREM", active, lease_id)
redis.call("DEL", lease_key)
if redis.call("GET", active_message_key) == lease_id then
  redis.call("DEL", active_message_key)
end
if action == "ack" then
  if lease_matches_message then
    redis.call("DEL", message_key)
    redis.call("ZREM", ready, message_id)
  end
  return 1
end
if lease_matches_message then
  local score = retry_at_ms
  if score == nil or score <= 0 then
    score = tonumber(redis.call("HGET", message_key, "score") or "0")
  end
  redis.call("HSET", message_key, "score", score)
  redis.call("ZADD", ready, score, message_id)
end
return 1
`
