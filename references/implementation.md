# Go 监控器参考实现

实现位于 [assets/go-monitor](../assets/go-monitor/README.md)，使用 Go 1.24+、Unix 文件锁和固定版本的 gorilla/websocket，目标系统为 macOS/Linux。

## 接入项目

先定位已有监控的配置和状态。新项目复制整个模板目录到用户工作区，不直接在技能安装目录运行，避免升级替换文件或把运行状态随技能分发。

源码分工：main.go 为 CLI，engine.go 为规则，store.go 为状态锁和原子写盘，market.go 为行情，codex.go 为命令探测和提交，monitor.go 为常驻协调。测试与配置随模板一起复制。

示例 config/monitor.json 使用牛来USDT，包含六个观察规则；币种与价位都需重新核验。换币同步修改 symbol、base_asset、quote_asset、阈值和消息模板，使用独立状态文件。codex.thread_id 留空，填入用户已有本机 Codex 任务的真实 UUID，不得使用 ChatGPT 对话 ID 或猜测 ID。

codex.binary 默认 codex，需要现场验证。PATH 命令不可用时检查实际安装的 CLI 并配置绝对路径；不要把一个开发环境的应用路径作为跨平台默认值。

## 操作与验证

在复制后的目录编译，再执行 rules、template、validate、doctor 和 status。完整命令见 [模板说明](../assets/go-monitor/README.md)。配置与完整消息以 CLI 输出为准，修改后校验、停止旧进程并重启。

已配置有效 CLI 和任务 UUID 后，用合成价格 test 检查规则；默认不发送。run --dry-run --duration 40s 检查真实行情。已获准验证提交时，给合成测试加 --send，只发送一条带 synthetic 标记、要求 ACK 的消息。正式持续运行使用 run --send，Ctrl-C 停止；默认不安装开机服务。

运行 go test -race ./...、go vet ./... 和 go build -o bin/niu-monitor .。自动测试使用虚构 UUID 和模拟 CLI，不提交真实消息。用户环境的真实行情和提交验证另行执行；保留事件 ID、CLI 回执及目标任务 ACK，区分每一步的证据。

## 规则和数据语义

- 价格阈值按报价币填写十进制字符串；pct_drop / pct_rise 单位为百分数，8 表示 8%。confirm_ticks 与 confirm_for 同时满足才触发。
- 首次已处于观察区默认不报警，需退出迟滞边界再进入；穿越规则始终需要在本次连续观测中先看到另一侧。保持在区内不会逢 cooldown 到期重复唤醒。
- 同规则有 pending、dispatching 或未解决 unknown 时阻止再次提交。修改规则保留同 ID 的冷却，不通过改 ID 或删除状态绕过防重。
- 持续确认和涨跌幅只用连续有效 WS。重启、断流、成交 ID 跳号或切到 REST 会重新积累时间型数据，可能保守漏报，不能把缺口算作一直站稳。
- 窗口变化使用每秒最后成交，参考窗口起点之前最近有效样本，是近似观测而非逐笔回测；窗口最长 1 小时。
- REST 使用带成交 ID 和交易所时间的 aggTrades，拒绝陈旧、重复、乱序行情。点价规则可用 REST 观测，但会漏掉轮询间的短暂穿越。
- Binance JSON 同时含字符串 e 与数值 E。Go encoding/json 有不区分大小写的后备匹配，结构体须显式映射两者；回归测试覆盖此问题。

## 防重、预算和恢复

单个行情连接、规则状态所有者和发送工作者；接收缓冲 1024 条。默认待发上限 32，已完成历史约 200 条，每分钟最多提交一次，每 UTC 日最多 20 次尝试，待发事件 5 分钟后过期。限额可配置。

普通状态每秒检查点，触发及发送意图立即持久化。写盘失败或队列满时报错停止；状态锁防止同文件的双写进程。dry-run 和测试状态独立，损坏状态不自动清空。

queue 成功记为 queued，exec-resume 成功记为 completed。超时、非零退出或发送中崩溃记为 unknown，不自动重试，也不切换接口重发。核对目标任务后，用 resolve --send --event ID --note '核对结果' 解除未知状态阻塞；不会重发旧事件。

queue 保持目标任务原有权限与模型；消息中的研究边界不等同于工具权限隔离。exec-resume 使用只读沙箱。监控不要接入具备不受控交易能力的任务。

## 官方核对入口

- [Binance WebSocket](https://github.com/binance/binance-spot-api-docs/blob/master/web-socket-streams.md)
- [Binance REST](https://github.com/binance/binance-spot-api-docs/blob/master/rest-api.md)
- [Codex 非交互模式](https://learn.chatgpt.com/docs/non-interactive-mode)

以本机实际帮助为准，不把旧版本探测视为永久能力。
