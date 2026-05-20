package redis

const enqueueScript = `
local ready = KEYS[1]
local prefix = ARGV[1]
local scope = ARGV[2]
local org_run_scope = ARGV[3]
local run_id = ARGV[4]
local payload = ARGV[5]
local score = ARGV[6]
local milli_cpu = ARGV[7]
local memory_mib = ARGV[8]
local disk_mib = ARGV[9]
local slots = ARGV[10]
local runtime_arch = ARGV[11]
local runtime_abi = ARGV[12]
local kernel_digest = ARGV[13]
local rootfs_digest = ARGV[14]
local cni_profile = ARGV[15]
local placement_region = ARGV[16]
local placement_labels = ARGV[17]
local placement_dedicated_key = ARGV[18]
local placement_snapshot_key = ARGV[19]
local generation_ttl_ms = tonumber(ARGV[20])

local run_generation_key = org_run_scope .. ":run:" .. run_id .. ":generation"
local generation = redis.call("INCR", run_generation_key)
local message_id = scope .. ":run:" .. run_id .. ":" .. tostring(generation)
local message_key = prefix .. ":message:" .. message_id
local active_message_key = prefix .. ":message_active:" .. message_id

redis.call("HSET", message_key,
  "payload", payload,
  "score", score,
  "milli_cpu", milli_cpu,
  "memory_mib", memory_mib,
  "disk_mib", disk_mib,
  "slots", slots,
  "runtime_arch", runtime_arch,
  "runtime_abi", runtime_abi,
  "kernel_digest", kernel_digest,
  "rootfs_digest", rootfs_digest,
  "cni_profile", cni_profile,
  "placement_region", placement_region,
  "placement_labels", placement_labels,
  "placement_dedicated_key", placement_dedicated_key,
  "placement_snapshot_key", placement_snapshot_key,
  "attempt", 0,
  "generation", generation,
  "run_generation_key", run_generation_key
)
redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
redis.call("DEL", active_message_key)
redis.call("ZADD", ready, tonumber(score), message_id)
return {message_id, redis.call("ZCARD", ready)}
`

const dequeueScript = `
local ready = KEYS[1]
local active = KEYS[2]
local prefix = ARGV[1]
local now_ms = tonumber(ARGV[2])
local lease_ms = tonumber(ARGV[3])
local max_messages = tonumber(ARGV[4])
local worker_host_id = ARGV[5]
local available_milli_cpu = tonumber(ARGV[6])
local available_memory_mib = tonumber(ARGV[7])
local available_disk_mib = tonumber(ARGV[8])
local available_execution_slots = tonumber(ARGV[9])
local reclaim_limit = tonumber(ARGV[10])
local scan_limit = tonumber(ARGV[11])
local generation_ttl_ms = tonumber(ARGV[12])
local worker_runtime_arch = ARGV[13]
local worker_runtime_abi = ARGV[14]
local worker_kernel_digest = ARGV[15]
local worker_rootfs_digest = ARGV[16]
local worker_cni_profile = ARGV[17]
local worker_region = ARGV[18]
local worker_labels = ARGV[19]

local function optional_match(requirement, value)
  return not requirement or requirement == "" or requirement == value
end

local function labels_match(requirements_json, labels_json)
  if not requirements_json or requirements_json == "" then
    return true
  end
  local ok_requirements, requirements = pcall(cjson.decode, requirements_json)
  if not ok_requirements or type(requirements) ~= "table" then
    return false
  end
  local labels = {}
  if labels_json and labels_json ~= "" then
    local ok_labels, decoded = pcall(cjson.decode, labels_json)
    if ok_labels and type(decoded) == "table" then
      labels = decoded
    end
  end
  for key, value in pairs(requirements) do
    if labels[key] ~= value then
      return false
    end
  end
  return true
end

local function label_value(labels_json, key)
  if not labels_json or labels_json == "" then
    return ""
  end
  local ok_labels, labels = pcall(cjson.decode, labels_json)
  if ok_labels and type(labels) == "table" and labels[key] then
    return labels[key]
  end
  return ""
end

local function compatible(fields)
  return optional_match(fields[8], worker_runtime_arch)
     and optional_match(fields[9], worker_runtime_abi)
     and optional_match(fields[10], worker_kernel_digest)
     and optional_match(fields[11], worker_rootfs_digest)
     and optional_match(fields[12], worker_cni_profile)
     and optional_match(fields[13], worker_region)
     and labels_match(fields[14], worker_labels)
     and optional_match(fields[15], label_value(worker_labels, "dedicated_key"))
     and optional_match(fields[16], label_value(worker_labels, "snapshot_key"))
end

local expired = redis.call("ZRANGEBYSCORE", active, "-inf", now_ms, "LIMIT", 0, reclaim_limit)
for _, lease_id in ipairs(expired) do
  local lease_key = prefix .. ":lease:" .. lease_id
  local message_id = redis.call("HGET", lease_key, "message_id")
  if message_id then
    local message_key = prefix .. ":message:" .. message_id
    local active_message_key = prefix .. ":message_active:" .. message_id
    if redis.call("EXISTS", message_key) == 1 then
      local metadata = redis.call("HMGET", message_key, "score", "run_generation_key", "generation")
      if metadata[2] and metadata[3] and redis.call("GET", metadata[2]) == tostring(metadata[3]) then
        local score = tonumber(metadata[1] or "0")
        redis.call("PEXPIRE", metadata[2], generation_ttl_ms)
        redis.call("ZADD", ready, score, message_id)
      else
        redis.call("DEL", message_key)
      end
    end
    if redis.call("GET", active_message_key) == lease_id then
      redis.call("DEL", active_message_key)
    end
  end
  redis.call("DEL", lease_key)
  redis.call("ZREM", active, lease_id)
end

local result = {}
local skipped = {}
for _ = 1, max_messages do
  local leased = false
  for _ = 1, scan_limit do
    local popped = redis.call("ZPOPMIN", ready, 1)
    if #popped == 0 then
      break
    end
    local message_id = popped[1]
    local score = tonumber(popped[2])
    local message_key = prefix .. ":message:" .. message_id
    if redis.call("EXISTS", message_key) == 1 then
      local fields = redis.call("HMGET", message_key, "milli_cpu", "memory_mib", "disk_mib", "slots", "payload", "run_generation_key", "generation", "runtime_arch", "runtime_abi", "kernel_digest", "rootfs_digest", "cni_profile", "placement_region", "placement_labels", "placement_dedicated_key", "placement_snapshot_key")
      local milli_cpu = tonumber(fields[1] or "0")
      local memory_mib = tonumber(fields[2] or "0")
      local disk_mib = tonumber(fields[3] or "0")
      local slots = tonumber(fields[4] or "0")
      local payload = fields[5]
      local run_generation_key = fields[6]
      local generation = fields[7]
      if not run_generation_key or not generation or redis.call("GET", run_generation_key) ~= tostring(generation) then
        redis.call("DEL", message_key)
      elseif not compatible(fields) then
        redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
        table.insert(skipped, {score, message_id})
      elseif milli_cpu > available_milli_cpu or memory_mib > available_memory_mib or disk_mib > available_disk_mib or slots > available_execution_slots then
        redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
        table.insert(skipped, {score, message_id})
      else
        available_milli_cpu = available_milli_cpu - milli_cpu
        available_memory_mib = available_memory_mib - memory_mib
        available_disk_mib = available_disk_mib - disk_mib
        available_execution_slots = available_execution_slots - slots
        local attempt = redis.call("HINCRBY", message_key, "attempt", 1)
        local lease_id = message_id .. ":" .. tostring(attempt)
        local lease_key = prefix .. ":lease:" .. lease_id
        local active_message_key = prefix .. ":message_active:" .. message_id
        local expires_at = now_ms + lease_ms
        redis.call("HSET", lease_key, "message_id", message_id, "worker_host_id", worker_host_id, "expires_at", expires_at, "active_key", active, "run_generation_key", run_generation_key, "generation", generation)
        redis.call("ZADD", active, expires_at, lease_id)
        redis.call("SET", active_message_key, lease_id, "PX", generation_ttl_ms)
        redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
        table.insert(result, {lease_id, message_id, payload, attempt})
        leased = true
        break
      end
    end
  end
  if not leased then
    break
  end
end

for _, item in ipairs(skipped) do
  redis.call("ZADD", ready, item[1], item[2])
end

return result
`

const readyMessageExistsScript = `
local prefix = ARGV[1]
local message_id = ARGV[2]
local now_ms = tonumber(ARGV[3])
local generation_ttl_ms = tonumber(ARGV[4])
local split_at = string.find(message_id, ":run:", 1, true)
if not split_at then
  return 0
end
local scope = string.sub(message_id, 1, split_at - 1)
local ready = prefix .. ":" .. scope .. ":ready"
local message_key = prefix .. ":message:" .. message_id
local active_message_key = prefix .. ":message_active:" .. message_id

local function cleanup_active_index()
  local lease_id = redis.call("GET", active_message_key)
  if lease_id then
    local lease_key = prefix .. ":lease:" .. lease_id
    local active = redis.call("HGET", lease_key, "active_key")
    if active then
      redis.call("ZREM", active, lease_id)
    end
    redis.call("DEL", lease_key)
    redis.call("DEL", active_message_key)
  end
end

if redis.call("EXISTS", message_key) == 0 then
  redis.call("ZREM", ready, message_id)
  cleanup_active_index()
  return 0
end
local metadata = redis.call("HMGET", message_key, "run_generation_key", "generation", "score")
if not metadata[1] or not metadata[2] or redis.call("GET", metadata[1]) ~= tostring(metadata[2]) then
  redis.call("DEL", message_key)
  redis.call("ZREM", ready, message_id)
  cleanup_active_index()
  return 0
end
if redis.call("ZSCORE", ready, message_id) then
  redis.call("PEXPIRE", metadata[1], generation_ttl_ms)
  return 1
end
local lease_id = redis.call("GET", active_message_key)
if lease_id then
  local lease_key = prefix .. ":lease:" .. lease_id
  if redis.call("EXISTS", lease_key) == 1 then
    local lease_fields = redis.call("HMGET", lease_key, "message_id", "active_key", "expires_at")
    if lease_fields[1] == message_id then
      local active = lease_fields[2]
      local expires_at = tonumber(lease_fields[3] or "0")
      if expires_at <= now_ms then
        redis.call("DEL", lease_key)
        redis.call("DEL", active_message_key)
        if active then
          redis.call("ZREM", active, lease_id)
        end
        redis.call("ZADD", ready, tonumber(metadata[3] or "0"), message_id)
      else
        if active then
          redis.call("ZADD", active, expires_at, lease_id)
        end
      end
      redis.call("PEXPIRE", metadata[1], generation_ttl_ms)
      return 1
    end
  end
  redis.call("DEL", active_message_key)
end
redis.call("DEL", message_key)
return 0
`

const renewScript = `
local prefix = ARGV[1]
local lease_id = ARGV[2]
local worker_host_id = ARGV[3]
local now_ms = tonumber(ARGV[4])
local expires_at = tonumber(ARGV[5])
local generation_ttl_ms = tonumber(ARGV[6])
local lease_key = prefix .. ":lease:" .. lease_id

if redis.call("EXISTS", lease_key) == 0 then
  return -1
end
if redis.call("HGET", lease_key, "worker_host_id") ~= worker_host_id then
  return -2
end
local current_expiry = tonumber(redis.call("HGET", lease_key, "expires_at") or "0")
if current_expiry <= now_ms then
  return -3
end
local message_id = redis.call("HGET", lease_key, "message_id")
local active = redis.call("HGET", lease_key, "active_key")
local run_generation_key = redis.call("HGET", lease_key, "run_generation_key")
local generation = redis.call("HGET", lease_key, "generation")
local message_key = prefix .. ":message:" .. message_id
local active_message_key = prefix .. ":message_active:" .. message_id
if redis.call("EXISTS", message_key) == 0 then
  return -1
end
if not run_generation_key or not generation or redis.call("GET", run_generation_key) ~= tostring(generation) then
  redis.call("ZREM", active, lease_id)
  redis.call("DEL", lease_key)
  if redis.call("GET", active_message_key) == lease_id then
    redis.call("DEL", active_message_key)
  end
  redis.call("DEL", message_key)
  return -2
end
redis.call("HSET", lease_key, "expires_at", expires_at)
redis.call("ZADD", active, expires_at, lease_id)
redis.call("SET", active_message_key, lease_id, "PX", generation_ttl_ms)
redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
return 1
`

const finishScript = `
local prefix = ARGV[1]
local lease_id = ARGV[2]
local worker_host_id = ARGV[3]
local now_ms = tonumber(ARGV[4])
local action = ARGV[5]
local reason = ARGV[6]
local generation_ttl_ms = tonumber(ARGV[7])
local lease_key = prefix .. ":lease:" .. lease_id

if redis.call("EXISTS", lease_key) == 0 then
  return -1
end
if redis.call("HGET", lease_key, "worker_host_id") ~= worker_host_id then
  return -2
end
local current_expiry = tonumber(redis.call("HGET", lease_key, "expires_at") or "0")
if current_expiry <= now_ms then
  return -3
end
local message_id = redis.call("HGET", lease_key, "message_id")
local active = redis.call("HGET", lease_key, "active_key")
local run_generation_key = redis.call("HGET", lease_key, "run_generation_key")
local generation = redis.call("HGET", lease_key, "generation")
local message_key = prefix .. ":message:" .. message_id
local active_message_key = prefix .. ":message_active:" .. message_id
if redis.call("EXISTS", message_key) == 0 then
  return -1
end
local ready = string.gsub(active, ":active$", ":ready")
redis.call("ZREM", active, lease_id)
redis.call("DEL", lease_key)
if redis.call("GET", active_message_key) == lease_id then
  redis.call("DEL", active_message_key)
end

if not run_generation_key or not generation or redis.call("GET", run_generation_key) ~= tostring(generation) then
  redis.call("DEL", message_key)
  return -2
end

if action == "ack" or reason == "invalid" then
  redis.call("DEL", message_key)
  redis.call("DEL", run_generation_key)
  return 1
end

local score = tonumber(redis.call("HGET", message_key, "score") or "0")
redis.call("ZADD", ready, score, message_id)
redis.call("PEXPIRE", run_generation_key, generation_ttl_ms)
return 1
`
