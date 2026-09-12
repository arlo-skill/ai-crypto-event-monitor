# AI 虚拟币事件监控

让本地程序持续监听行情，在规则命中后才调用 Codex 开展研究。无事件时不调用 AI，避免通过固定时间唤醒模型来检查价格。

本技能涵盖监控程序开发、可查询规则、持久化防重、Codex CLI 事件触发，以及市场、链上、资金流和新闻热度核验。附带可编译的 Go 模板，默认 dry-run，没有交易执行功能。

技能入口：[SKILL.md](SKILL.md)。技能名称：`ai-crypto-event-monitor`。

## 一行安装

准备 Node.js 22.20 或更高版本（包含 npm/npx）和 Git，然后运行：

```bash
npx --yes skills@latest add arlo-skill/ai-crypto-event-monitor --skill ai-crypto-event-monitor --agent codex --global --yes
```

安装使用 [skills CLI](https://github.com/vercel-labs/skills)，包含技能入口、参考文档和 Go 模板。两个 `--yes` 分别跳过 npx 下载确认和技能安装确认。

- **只在当前项目使用**：在项目根目录执行，去掉 `--global`。
- **安装给其他 AI 工具**：将 `--agent codex` 换成受支持的目标，例如 `claude-code` 或 `cursor`。监控器的事件接收端仍是 Codex CLI；切换接收端需要适配。
- **已有本地修改**：安装可能更新同名技能，先保留仍需要的定制内容。

查看安装结果：

```bash
npx --yes skills@latest list --global --agent codex
```

安装后使用 `$ai-crypto-event-monitor`。**安装技能不会启动监控、创建定时任务或提交交易。** 运行 Go 模板还需要 Go 1.24+、macOS/Linux、可用的 Codex CLI，以及接收消息的已有任务 UUID。

## 适用场景

- **事件驱动盯盘**：价格进入观察区、向上/向下穿越、持续站稳、指定窗口快速涨跌。
- **规则管理**：查询规则、阈值、确认条件、冷却时间及完整消息模板。
- **运行排查**：分析 WebSocket 断流、REST 回退、陈旧行情、重复事件、CLI 提交失败和未知发送状态。
- **触发后研究**：核验市场条件、同合约 DEX 流动性、链上转账与巨鲸、成交方向、新闻和热度，说明事实、反证及缺口。

## 使用方式

创建监控时：

> 使用 $ai-crypto-event-monitor，在当前工作区为我关注的 Binance 现货交易对创建 Go 监控器。先核验准确 symbol 和本机 Codex CLI，提供规则查询、完整模板与合成测试，先验证 dry-run，再按已授权范围验证一次真实消息提交。

维护已有监控时：

> 使用 $ai-crypto-event-monitor，先读取规则和状态，检查目标任务、冷却与未决事件，再调整观察阈值并验证。保留已有防重状态，不因重启重复唤醒 AI。

收到市场事件后：

> 使用 $ai-crypto-event-monitor 核验事件现在是否仍成立，再检查市场、链上、资金流和新闻热度。区分事实与推断，给出来源、数据缺口及条件式情景；仅做研究。

## 附带的 Go 模板

将 [assets/go-monitor](assets/go-monitor) 复制到自己的工作区，再配置与运行，避免在技能安装目录产生状态或影响升级。

| 能力 | 实现 |
|---|---|
| 行情 | Binance 现货 aggTrade WebSocket、ping/pong、断线退避、REST 新成交回退 |
| 规则 | below、above、cross_below、cross_above、hold_below、hold_above、pct_drop、pct_rise |
| 确认与防重 | 精确小数、连续成交/持续时间、迟滞、cooldown、状态锁、原子写入 |
| CLI | 规则列表、完整模板、配置校验、行情查询、状态、合成测试、dry-run、正式监听 |
| Codex | 现场探测接口；兼容时使用 queue，否则使用经探测的非交互 exec-resume |
| 限额与恢复 | 单发送进程、有界队列、提交频率/每日上限、事件过期、未知结果不自动重发 |

示例使用 `牛来USDT` 的观察价位，**不是当前投资建议**。`codex.thread_id` 留空，`codex.binary` 为待验证的 `codex` 命令，`dry_run` 默认为 `true`。使用前须填写本机有效任务 UUID 和 CLI 路径。配置不包含作者机器路径、会话标识、凭据或运行记录。

从仓库直接试用：

```bash
git clone https://github.com/arlo-skill/ai-crypto-event-monitor.git
cd ai-crypto-event-monitor/assets/go-monitor
mkdir -p bin
go build -o bin/niu-monitor .
./bin/niu-monitor rules
./bin/niu-monitor validate
```

后续步骤见 [Go 模板使用说明](assets/go-monitor/README.md)。一行安装只安装技能文件，不会自动编译 Go 程序。

## 文件导航

| 文件 | 内容 |
|---|---|
| [SKILL.md](SKILL.md) | 工作方法、CLI 探测、事件验证及安全边界 |
| [实现说明](references/implementation.md) | 模板接入、规则语义、恢复与验证 |
| [研究规范](references/research.md) | 市场、链上、资金流、新闻热度的证据口径 |
| [Go 模板](assets/go-monitor/README.md) | 可编译程序、独立配置与测试 |
| [示例配置](assets/go-monitor/config/monitor.json) | 交易对、阈值、限额与完整消息模板 |
| [Codex 展示信息](agents/openai.yaml) | 技能名称、简介及调用示例 |

## 验证与边界

模板有 18 个顶层测试及 8 个触发类型子测试，覆盖规则、数据质量、状态恢复、进程参数、WebSocket、REST 与限流。开发时曾验证真实行情接收、REST 回退，以及合成价格触发后由 Codex 目标任务返回 ACK；使用者仍需在自己的环境重新核验。

```bash
cd assets/go-monitor
go test -race ./...
go vet ./...
```

CLI 接受入队、目标任务收到消息、AI 完成研究是不同状态。发送超时或结果不明时先核对收件，再恢复，避免重复提交。研究提示不授权下单、转账、钱包签名或读取私钥；技术闭环通过不代表策略已回测或能盈利。

欢迎通过 [Issues](https://github.com/arlo-skill/ai-crypto-event-monitor/issues) 提交使用场景与问题。更多技能见 [Arlo Skill](https://github.com/arlo-skill)。
