-- GCRA with burst tolerance 0. Redis TIME is the only clock.
-- KEYS[1] = limiter key, ARGV[1] = emission interval in microseconds,
-- ARGV[2] = key TTL in milliseconds.
-- Returns {1, slot_us} when admitted, {0, retry_after_us} when not.
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = tonumber(ARGV[1])
local tat = tonumber(redis.call('GET', KEYS[1]))
if tat == nil or tat < now then
  tat = now
end
if now < tat then
  return {0, tat - now}
end
redis.call('SET', KEYS[1], string.format('%d', tat + interval), 'PX', tonumber(ARGV[2]))
return {1, now}
