# Go 价格监控模板

持续监听 Binance 现货成交，由本地规则决定是否提交 Codex 消息。仅查询公开行情，不需要 Binance API Key，没有下单功能。

先将本目录复制到自己的工作区，再配置、编译和运行；不要在技能安装目录保存状态。环境要求：Go 1.24+、macOS/Linux，以及可用的 Codex CLI。

## 编译与配置

```sh
mkdir -p bin
go build -o bin/niu-monitor .
./bin/niu-monitor rules
./bin/niu-monitor validate
```

编辑 `config/monitor.json`：

- `symbol` / `base_asset` / `quote_asset`：必须与交易所现货元数据匹配。示例为 `牛来USDT` / `牛来` / `USDT`。
- `rules`：调整观察价位、类型、连续确认、迟滞、冷却和规则关注点。示例价位未回测，不是当前投资建议。
- `codex.binary`：本机可执行的 Codex 命令或绝对路径。
- `codex.thread_id`：填写已有本机 Codex 任务的真实 UUID。空值会阻止监听和手动触发测试；不要填 ChatGPT 对话 ID。
- `codex.dry_run`：默认 true；`run --send` 可显式开启实际提交。

可把定制配置保存为 `config/monitor.local.json`，在每条命令加 `--config config/monitor.local.json`。配置内相对路径按配置文件所在目录解析；`--state` 按当前工作目录解析。

## 查询和测试

```sh
# 只读探测：CLI 帮助及交易所元数据，不提交 AI 消息
./bin/niu-monitor doctor --out evidence/doctor.json
./bin/niu-monitor price

# 全部规则 / 单条规则 / 完整消息模板
./bin/niu-monitor rules
./bin/niu-monitor rules --rule watch-0118
./bin/niu-monitor template --rule watch-0118

# 合成测试，默认不发送；先填写有效 CLI 和目标 UUID
./bin/niu-monitor test --rule watch-0118 --prices 0.120,0.118,0.117,0.116

# 真实行情短测，仅模拟提交
./bin/niu-monitor run --dry-run --duration 40s
```

`doctor` 探测版本及 `--help`、`queue --help`、`resume --help`、`exec --help`、`exec resume --help`。它不证明目标任务存在或收到了消息；语法校验也不等于连接可用。

需验证真实提交时，给合成测试加 `--send`：

```sh
./bin/niu-monitor test --rule watch-0118 --prices 0.120,0.118,0.117,0.116 --send --state runtime/receipt-test.json
```

这会提交一条明确标记为 synthetic、只要求 `NIU_MONITOR_ACK <event_id>` 的消息。每次手动执行都是新测试；未知结果应先核对目标任务，不盲目重复测试。

## 持续运行与停止

```sh
# 持续监听，模拟提交
./bin/niu-monitor run --dry-run

# 正式监听，命中后实际调用 Codex
./bin/niu-monitor run --send
```

Ctrl-C 停止，不会自动安装系统服务。配置修改后先校验，再停止并重启；不要使用不同状态文件启动同一规则和目标的多个进程，绕过防重。

## 状态与恢复

```sh
./bin/niu-monitor status
./bin/niu-monitor status --send
./bin/niu-monitor resolve --send --event EVENT_ID --note '已核对目标任务收件记录，说明实际结果'
```

| 文件 | 用途 |
|---|---|
| config/monitor.json | 可复制修改的示例规则与完整模板 |
| runtime/state.json | 正式状态，首次运行创建 |
| runtime/state.json.dry-run.json | 独立 dry-run 状态 |
| runtime/state.json.test.json | 默认测试状态，每次新测试重新生成 |
| evidence/doctor.json | 自行保存的命令探测结果 |

不要提交运行状态、日志、私人任务 UUID 或凭据到公开仓库。`resolve` 只解除 unknown 阻塞，不重发旧事件。

## 规则与可靠性

支持上下限、双向穿越、双向持续确认、窗口涨跌幅共 8 种类型。价格用精确小数；连续确认、迟滞和 cooldown 同时生效。首次启动默认不对已经处于观察区的价格报警，一直停在区内也不会因冷却到期重复提交。

WS 断开会退避重连；默认 15 秒无有效 WS 新成交时，REST 每 5 秒查一次最新成交。持续站稳和涨跌幅要求连续 WS，REST 只供可用点价规则使用；重复、乱序和过期行情会被拒绝。每 5 分钟重新核验交易状态，失败则暂停事件生成与提交。

发送与行情接收分离。默认一分钟最多一次提交、每 UTC 日最多 20 次尝试；队列有上限且事件会过期。事件和发送意图先写盘再调用 CLI。失败或超时可能已经入队，因此标记 unknown、阻止同规则再次提交，等待核对，不自动重试。详细语义见[实现说明](../../references/implementation.md)。

## 验证源码

```sh
go test -race ./...
go vet ./...
```

测试使用模拟 CLI 和虚构 UUID，不发送真实消息。用户环境还需核验行情、CLI 回执和任务 ACK。短时联通不能证明长期稳定，规则测试也不能证明策略收益。

## 只读网页与持仓台账

网页由同一个 Go 程序提供，支持多个独立币种监控的总览、完整规则/消息模板、触发记录、已确认持仓和未成交挂单。网页只有查看入口，也没有写入 API；用户在对话中告知 AI，AI 更新台账后自动推送到页面。

首次使用公开模板时：

```sh
cp config/portfolio.example.json config/portfolio.local.json
cp config/dashboard.example.json config/dashboard.json
./bin/niu-monitor dashboard --config config/dashboard.json --listen 127.0.0.1:8788
```

运行后打开 **http://127.0.0.1:8788**（仅当前电脑）。`dashboard.json` 的每个 `config_file` 指向一个币种的监控配置，各币必须使用不同的 `state_file`；dry-run 配置读取独立的 `.dry-run.json` 状态。网页本身不启动行情监控和模型调用；需按已有授权分别启动各币监控。部署 AI 必须实际打开页面验证，再把真实运行网址交给用户，不以示例地址代替验收。

持仓字段未知时保留 null，记录比例分母、确认时间及更新历史；未成交订单不能算入持仓，也不能当成不占资金。不要把账户比例推导成缺失的币数量。台账、规则和状态每秒统一读取一次；多开页面共享采集，最多 100 个 SSE 连接，慢连接只保留最新状态，隐藏页面停止订阅，每币最多显示最近 30 条事件。登记文件增删币种后重启网页服务，单币监控规则修改后须重启对应监控服务并保留原状态。

回环监听、Host/Origin 检查、只读方法限制适用于本机访问；需要远程访问时另行设计认证，勿直接公开用户持仓。台账更新与前端部署规范见技能的 [portfolio-dashboard.md](../../references/portfolio-dashboard.md)。
