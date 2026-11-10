#!/usr/bin/env bash
# 为压测准备数据：直接向本地测试库写入 2 个分组（anthropic / openai）、
# 指向 mock 上游的 apikey 账号、普通用户及其 API Key。
# 只用于一次性的本地压测库，不要对生产库执行。
# 写完后需要重启 sub2api（或等缓存过期）让调度快照加载到新账号。
set -euo pipefail

MOCK=${MOCK:-http://127.0.0.1:9100}
ACCOUNTS=${ACCOUNTS:-50}          # 每个平台的账号数
ACCOUNT_CONC=${ACCOUNT_CONC:-200} # 每个账号的并发上限
USERS=${USERS:-50}
USER_CONC=${USER_CONC:-2000}
OUT=${OUT:-.}
PSQL=${PSQL:-psql -h 127.0.0.1 -U sub2api sub2api}

$PSQL -v ON_ERROR_STOP=1 -q <<SQL
BEGIN;
INSERT INTO groups (name, platform) VALUES ('loadtest-anthropic','anthropic'), ('loadtest-openai','openai');

INSERT INTO accounts (name, platform, type, credentials, concurrency, priority)
SELECT 'lt-'||p||'-'||i, p, 'apikey',
       jsonb_build_object('api_key','sk-mock-'||p||'-'||i,'base_url','$MOCK'),
       $ACCOUNT_CONC, 1
FROM unnest(ARRAY['anthropic','openai']) p, generate_series(1,$ACCOUNTS) i;

INSERT INTO account_groups (account_id, group_id)
SELECT a.id, g.id FROM accounts a JOIN groups g ON g.name = 'loadtest-'||a.platform
WHERE a.name LIKE 'lt-%';

INSERT INTO users (email, password_hash, balance, concurrency)
SELECT 'lt-user-'||i||'@loadtest.local', 'x-no-login', 1000000, $USER_CONC
FROM generate_series(1,$USERS) i;

INSERT INTO api_keys (user_id, key, name, group_id)
SELECT u.id, 'sk-lt-'||g.platform||'-'||md5(u.email||g.platform), 'lt-'||g.platform, g.id
FROM users u, groups g
WHERE u.email LIKE 'lt-user-%' AND g.name LIKE 'loadtest-%';
COMMIT;
SQL

for p in anthropic openai; do
  $PSQL -Atc "SELECT k.key FROM api_keys k JOIN groups g ON g.id=k.group_id WHERE g.name='loadtest-$p' ORDER BY k.id" >"$OUT/keys-$p.txt"
done
wc -l "$OUT"/keys-*.txt
