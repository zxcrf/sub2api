# Sub2API 本地压测工具

用假上游测 Sub2API 网关自身的开销（鉴权、调度、并发槽、计费落库、SSE 转发），不碰任何真实上游账号。

| 文件 | 作用 |
|---|---|
| `mock/` | 假上游：Anthropic Messages、OpenAI Responses、Chat Completions，流式/非流式；首字节延迟、事件数、事件间隔可运行期调整（`GET /__cfg?ttfb_ms=&chunks=&interval_ms=`） |
| `loadgen/` | 闭环压测客户端：输出吞吐、TTFB/总耗时分位数、错误分布，并从 `/proc` 采样被测进程 CPU 与 RSS |
| `gen_bodies.py` | 生成请求体：small（百字节）与 large（约 71KB，带 18 个工具定义和 17 轮历史） |
| `setup.sh` | 向**一次性的本地测试库**直接写入分组、账号（指向 mock）、用户和 API Key |
| `run.sh` | 跑单个场景，结果追加到 `results.jsonl` |

## 用法

```bash
cd tools/loadtest
go build -o mock.bin ./mock && go build -o loadgen.bin ./loadgen
python3 gen_bodies.py .
./mock.bin &                                   # 127.0.0.1:9100
# 启动 sub2api（空库首次启动可用 AUTO_SETUP=true），然后：
PGPASSWORD=... ./setup.sh                      # 写完后重启 sub2api
SERVER_PID=$(pgrep -x server) LOADGEN=./loadgen.bin \
  ./run.sh demo anthropic body-large-anthropic.json 300 25s 800 100 30
```

注意：

- 请求体里的 `__WORKER__` 会按 worker 替换。去掉它，所有请求内容相同，会被粘性会话调度到同一个上游账号，出现 `gateway_queue_full`（429）假象。
- PostgreSQL `max_connections` 要大于 sub2api 的 `database.max_open_conns`。代码默认值是 256，官方 compose 传的是 50，而 PG 默认只有 100；直接跑二进制又不改配置时，高并发下会出现 `too many clients already`，表现为用量记账失败。
- `setup.sh` 绕过管理端直接写库，只能用于本地一次性测试库。

## 一次实测结果（2026-11，v2.8.14 附近）

### 环境与口径

- 机器：4 核 Xeon 2.1GHz / 15GB，全部进程同机。sub2api 用 `taskset` 绑在 2 个核上（`GOMAXPROCS=2`）；mock、loadgen、PostgreSQL 16、Redis 7 绑在另外 2 个核。
- 连接池按官方 compose 默认值：`DATABASE_MAX_OPEN_CONNS=50`、`REDIS_POOL_SIZE=1024`。日志级别为默认。
- 账号：每平台 50 个 apikey 账号、并发上限 200；50 个用户各 1 个 Key。
- **下表都是 2 核实测值。4 核的数字是按 CPU 线性外推，没有实测过。**
- CPU 只算 sub2api 进程的 user+sys；回环网络的软中断（约占每核 5–10%）没算进去。

### 单请求 CPU 成本（上游零延迟，把 sub2api 压满）

| 场景 | 吞吐 (2 核) | sub2api CPU | 每请求 CPU |
|---|---|---|---|
| Anthropic，小请求体，流式 60 事件 | 423 rps | 1.58 核（压测端先到顶） | 3.7 ms |
| Anthropic，71KB，流式 60 事件 | 280 rps | 1.92 核 | 6.9 ms |
| Anthropic，71KB，流式 300 事件 | 187 rps | 1.94 核 | 10.3 ms |
| Anthropic，71KB，非流式 | 298 rps | 1.88 核 | 6.3 ms |
| Responses，71KB，流式 60 事件 | 157 rps | 1.94 核 | 12.4 ms |
| Responses，71KB，流式 300 事件 | 123 rps | 1.92 核 | 15.6 ms |
| Chat Completions，71KB，流式 60 事件 | 218 rps | 1.90 核 | 8.7 ms |
| Chat Completions，71KB，流式 300 事件 | 146 rps | 1.91 核 | 13.1 ms |

这一组上游不间断吐数据，事件是成批转发的，所以每事件成本偏低（约 0.015 ms）。

### 真实形态流式（上游首字节 800ms，每 30ms 一个事件，71KB 请求体）

事件逐个转发、逐个 flush，每事件成本升到约 0.05–0.1 ms，是长回复的主要开销。

| 场景 | 并发流 | 吞吐 | sub2api CPU (共 2 核) | 每请求 CPU | 附加 TTFB p50 / p90 / p99 | RSS 峰值 |
|---|---|---|---|---|---|---|
| Anthropic，100 事件 | 300 | 79 rps | 1.19 | 15.2 ms | +15 / +50 / +167 ms | 273 MB |
| Anthropic，100 事件 | 450 | 约 110 rps | 1.34 | 约 12 ms | +66 / +293 / +557 ms | 429 MB |
| Anthropic，100 事件 | 600 | 约 123 rps | 1.51 | 约 12 ms | +385 / +719 / +1276 ms | 546 MB |
| Anthropic，300 事件 | 300 | 31 rps | 0.80 | 25.7 ms | +12 / +16 / +34 ms | 460 MB |
| Responses，100 事件 | 200 | 53 rps | 1.12 | 21.2 ms | +53 / +95 / +211 ms | 428 MB |
| Responses，100 事件 | 300 | 约 74 rps | 1.41 | 约 19 ms | +150 / +625 / +867 ms | 479 MB |
| Responses，100 事件 | 450 | 约 97 rps | 1.66 | 约 17 ms | +615 / +1205 / +1575 ms | 536 MB |
| Chat Completions，100 事件 | 300 | 75 rps | 1.16 | 15.4 ms | +73 / +314 / +530 ms | 525 MB |

附加 TTFB = 实测 TTFB 减去上游的 800ms。标“约”的吞吐由并发数除以总耗时中位数算出（那几轮是在修正 loadgen 统计口径之前跑的）。

sub2api 的 CPU 占用超过约 60–65% 后，尾延迟开始明显变差。这里有一部分是另外两个核同样被 mock、压测端、PG、Redis 占到 75% 左右造成的，所以高并发档的延迟偏悲观。

### 连接保持与内存

3000 条并发流（71KB 请求体，每秒 1 个事件，单请求约 32 秒，约 95 rps）：sub2api 占 1.03 核，RSS 峰值 1766 MB，无错误。空载 RSS 约 175 MB，折合每个在途的 71KB 请求约 0.5–0.6 MB。

### 长上下文（约 850KB 请求体，60 轮工具调用）

200 条并发流，上游首字节 2s，100 个事件、间隔 100ms（单请求约 12 秒，约 18 rps），sub2api 绑 2 核：

| 链路 | sub2api CPU | 每请求 CPU | RSS 峰值 | 流式期间常驻堆 | 每请求累计分配 |
|---|---|---|---|---|---|
| Anthropic `/v1/messages` | 0.84 核 | 47 ms | 591 MB | 约 0.85 MB/请求（1 份请求体） | 约 2.7 MB（请求体的 3 倍） |
| OpenAI `/v1/responses` | 1.05 核 | 59 ms | 700 MB | 约 0.94 MB/请求（1 份请求体） | 约 18 MB（请求体的 20 倍） |

- 主转发链路不做 tiktoken 计数；`tiktoken-go` 只在 `count_tokens` / `input_tokens` 接口里用。计费用的是上游返回的 usage。
- 流式期间只常驻 1 份原始请求体（`readRequestBodyChunks`）；其余都是解析和改写阶段的瞬时分配，靠 GC 回收。折算 RSS 约为每个在途请求 2.2–2.7 MB（请求体的 2.5–3 倍）。
- Responses 链路的瞬时分配大头是对整个请求体的多次完整 JSON 解码：`normalizeCodexCallOutputBootstrap`（约一半）、`NormalizeCompactionTriggerInputOrder`、`hasUniqueJSONMembers`、`HasCompactionTriggerInInput` 等。
- “每请求累计分配”由堆 profile 的采样差值除以估算的请求数得出，是量级估计。

### 每请求对依赖的压力

| 依赖 | 每请求 |
|---|---|
| Redis | 约 71 条命令 |
| PostgreSQL | 约 10 个事务 |
| 日志 | 约 11 行、5 KB（默认级别） |

在约 430 rps 的小请求压测中，PostgreSQL 约占 0.8 核，Redis 约占 0.25 核。

### 观察到的问题

1. **热路径上有未走缓存的 SQL。** Anthropic 分组每个请求都会执行 3 次 `SELECT ... FROM settings`、1 次 `SELECT rpm_override FROM user_group_rate_multipliers`、1 次 `SELECT ... FROM account_groups`。最后一条来自调度快照回源：Redis 里每个请求都在读 `sched:*:antigravity:forced` 这个桶，该分组下没有 antigravity 账号，疑似空结果没被缓存。
2. **数据库吃紧时 OpenAI 分组直接返回 503。** PG 连接耗尽期间出现过 `basispoints_image_settings_unavailable`：图片中转设置读取失败时，纯文本请求也被拒绝。
3. **日志量大。** 按每请求 5 KB 算，200 rps 时约 1 MB/s。

## 与 new-api 的对比（v1.0.0-rc.49，2026-11-08）

同一台机器、同一套 mock 和请求体、同样的绑核方式（被测进程 2 核）。new-api 按其官方 compose 的环境变量启动（PostgreSQL + Redis、`BATCH_UPDATE_ENABLED=true`、`ERROR_LOG_ENABLED=true`），每平台 50 个渠道、50 个用户令牌，前端用占位页编译。sub2api 列出两种 PG 空闲连接配置：官方 compose 默认的 10，以及调到 50。

### 饱和吞吐（上游零延迟，71KB 请求体，流式 60 事件，48 并发）

| 链路 | new-api | sub2api（空闲连接 50） | sub2api（compose 默认 10） |
|---|---|---|---|
| Anthropic `/v1/messages` | 196 rps，10.0 ms/请求 | 283 rps，6.8 ms/请求 | 157 rps，9.0 ms/请求 |
| OpenAI `/v1/responses` | 131 rps，14.9 ms/请求 | 164 rps，11.7 ms/请求 | 102 rps，15.0 ms/请求 |
| OpenAI `/v1/chat/completions` | 142 rps，13.6 ms/请求 | 227 rps，8.3 ms/请求 | 124 rps，10.9 ms/请求 |

compose 默认配置下 sub2api 的 CPU 只用到 1.4–1.5 核就上不去了：`DATABASE_MAX_IDLE_CONNS=10` 让连接池不停关闭又重建 PG 连接（一次 14 秒的压测里新建了 2059 个会话，调到 50 后是 36 个），PG 的 fork 开销把另外两个核占满。

### 真实形态流式（上游首字节 800ms，100 事件、间隔 30ms，71KB）

| 场景 | 指标 | new-api | sub2api（空闲连接 50） |
|---|---|---|---|
| Anthropic，300 并发流 | CPU / 每请求 | 1.12 核 / 14.2 ms | 1.11 核 / 14.0 ms |
| | 附加 TTFB p50 / p99 | +11 / +32 ms | +13 / +79 ms |
| | RSS 峰值 | 305 MB | 286 MB |
| Responses，200 并发流 | CPU / 每请求 | 1.07 核 / 20.3 ms | 1.12 核 / 21.2 ms |
| | 附加 TTFB p50 / p99 | +16 / +52 ms | +52 / +148 ms |
| | RSS 峰值 | 273 MB | 357 MB |
| Chat Completions，300 并发流 | CPU / 每请求 | 1.40 核 / 17.8 ms | 1.16 核 / 14.6 ms |
| | 附加 TTFB p50 / p99 | +55 / +165 ms | +52 / +188 ms |
| | RSS 峰值 | 330 MB | 461 MB |

### 长上下文（约 850KB，200 并发流，上游首字节 2s）

| 场景 | 指标 | new-api | sub2api（空闲连接 50） |
|---|---|---|---|
| Anthropic | CPU / 每请求 | 0.87 核 / 48.9 ms | 0.84 核 / 47.0 ms |
| | 附加 TTFB p50 / p99 | +54 / +97 ms | +52 / +98 ms |
| | RSS 峰值 | 1273 MB | 700 MB |
| Responses | CPU / 每请求 | 1.44 核 / 98.6 ms | 1.06 核 / 59.2 ms |
| | 附加 TTFB p50 / p99 | +1543 / +5288 ms | +203 / +334 ms |
| | RSS 峰值 | 1246 MB | 780 MB |

new-api 这一档的 CPU profile 里，`tokenkit.Count`（tiktoken）占 CPU 采样的约 60%。它默认 `CountToken=true`，对 OpenAI GPT / o 系列模型做精确计数，其他模型（如 Claude）走估算，所以 Anthropic 链路上看不到这项开销。

### 每请求对依赖的压力（Anthropic，71KB）

| | new-api | sub2api |
|---|---|---|
| PostgreSQL 事务 | 约 2 | 约 10 |
| Redis 命令 | 约 29 | 约 71 |
| 空载 RSS | 约 85 MB | 约 80–160 MB |

### 口径限制

- 两边都只测了“API Key 渠道直转”这一种形态。sub2api 的 OAuth 订阅账号调度、new-api 的多渠道重试和各类可选功能都没有覆盖。
- 两个项目的功能不等价（计费模型、调度策略、日志内容都不同），数字只说明同一负载下各自的开销。
- 每个场景只跑了一轮，没有重复取均值；相差 10% 以内的项不宜当作有差别。
