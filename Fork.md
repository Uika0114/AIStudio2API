# AIStudio2API Fork

更新：2026-11-21。本文对比 Fork 已提交源码与官方最新发布，记录保留的本地能力、上游已具备的能力和升级边界。安装与发布记录另有维护入口。

## 基线与边界

| 对象 | 版本或提交 | 含义 |
|---|---|---|
| 官方分叉点 | [`2cbe310`](https://github.com/Mag1cFall/AIStudio2API/commit/2cbe310ce6ebab093bf83e5078de8296f03770a9) | 历史共同基线；早期安装过的 Release 版本不等于分叉点 |
| Fork | [`main@45b32f3`](https://github.com/Uika0114/AIStudio2API/commit/45b32f3e2431cf2cf12c694b3b6d4a2036bb6e64) | 写本文前，本地 `main` 与远端 `main` 同指；未提交的产品改动不计入 |
| 官方最新 | [`v0.3.13`](https://github.com/Mag1cFall/AIStudio2API/releases/tag/v0.3.13) / [`c77dd89`](https://github.com/Mag1cFall/AIStudio2API/commit/c77dd89d4ede5cbe55534db75fea864206716de8) | 核对时官方 `main` 与此 tag 同指；此版本未合入 Fork |

Fork 源码沿用上游 [MIT 许可证](LICENSE)。当前源码检出是浅克隆；用两端 Git 对象核实，共同祖先为 `2cbe310`；从共同祖先算起，官方侧独有 111 笔、Fork 侧独有 38 笔（其中 1 笔合并）。分叉点到 Fork 基线的 Git 总差异是 323 个文件、+44,475 / −1,143 行，其中按路径识别的测试及夹具为 +27,674 / −59 行。统计还包含协议文件与文档，不是净产品代码量，也不是两棵最新源码树的直接差。以下按实际行为比较。

## Fork 保留的能力

| 能力 | Fork 的行为 | 和官方 v0.3.13 的区别 |
|---|---|---|
| 严格通道与输出抗截断 | 在兼容裸模型名的同时，给 Chat Completions、Responses、Gemini 文本生成提供 Playground／Build 显式通道，失败不跨通道。完整参数抗截断是显式选择的输出策略，保留客户端工具和正常终态；不保证消除上游截断。 | 官方也有 Playground 与 Build。官方旧 Build **生成**默认关闭且上游可拒绝；新版另有 `<model>-build` **对话助手**，带其系统提示并使用不同额度。它不等同于 Fork 的严格 Build 生成；合入时不能视为别名。 |
| Assistant 文本 | 保留独立显式 Assistant 文本路由，不加入裸名自动选号；文本输出预算、合法采样参数有专门的兼容边界。默认只开 Playground。 | 不把独立 Assistant 路由与官方新版 Build 对话助手画等号。是否能用仍取决于通道和账号，目录列出不等于生成已验。 |
| 工具历史可移植 | 在显式 Playground／Build 保留工具调用与工具结果语义，按模型和通道判定历史是否可用；已知冲突、非工具的不透明状态及跨严格边界不静默放行。响应 Schema 别名、工具 Schema 空枚举也各有独立兼容逻辑。 | 工具调用和多协议续接属于双方共同能力；Fork 的差异是显式路由的来源判断、降级范围和行为合同，不能把所有工具支持算成独有。 |
| 额度、用量与价格台账 | 管理面分开展示 Argon、Carbon、Hydrogen 及发现的 Gemini 4 共享池，区分历史、实时观测和未知余额。Hydrogen 估算包含 Pro；被明确要求验证而停派的账号不计入当前供给。价格单可校验热载入，生成 TPS 基于完整上游尝试；本地展示不会增加上游额度，也不作为放行依据。 | 官方也有多账号权益、模型目录、用量和费用展示；Fork 的差异是上述独立池估算、归属、状态口径和价格／TPS 规则。 |
| 账号故障隔离与恢复 | 失败账号先退出普通调度，按失败类型区分有限重试、人工接管和已有工单恢复；未决状态不盲目重放，重登记保留旧工单，恢复后通过持久回执交接。 | 官方也会隔离不可用账号并改善预热、账号页和 Worker 恢复。Fork 的差异主要是独立 Windows／服务端角色及跨实例恢复状态的边界。 |
| 运行开销 | 保留当前纯 Go 与 Camoufox 两种 WAA 后端，以及正文累积、工具历史存储等局部内存优化。 | 官方新版默认改为进程内 SpiderMonkey，Camoufox 作为另一选择；这是后端和构建依赖变化，不能把新版部署当作同配置替换。 |

源码入口：[严格通道](docs/channel-anti-truncation.md)、[Assistant](docs/assistant-experiment.md)、[工具历史](docs/portable-tool-history.md)、[价格单](docs/model-prices.md)、[账号恢复](docs/remote-auth.md)。关键提交：通道 `6761bde` / `28a0a42`，工具历史 `74b77de` / `3a1f5ed`，Schema `d9f92e4` / `da5a646`，额度 `96cba3a` / `6405f60` / `ba25668` / `45b32f3`，账号 `e113026` / `2c3df66` / `14db361` / `36d4ed6`。

## 上游领先项与升级方式

- OpenAI Chat／Responses、Anthropic、Gemini、文件、原生工具和已有多媒体接口属于继承的基础能力。不要按 README 上的项目特性条目反推都是 Fork 新增。 官方也已支持长回答自动续写、验证类异常账号隔离和 TPS 展示；Fork 的差异分别在完整参数输出合同、可恢复工单与额度排除，以及按完整尝试计量的口径。
- 官方新版已加入桌面托盘、在线更新、更多界面语言、API 试用页、按 API Key 限定账号范围、Build 对话助手、Deep Research、决策 API 和 Anthropic 原生上下文管理；这些不应仅因 Fork 已有 Responses 压缩或相似入口就视为同步。官方 `v0.3.13` 还补了停用账号不阻断启动、后台预热和首批目录就绪等待等修复，需按本 Fork 的纯 Go／Camoufox 与账号隔离合同单独评估。见[官方 README](https://github.com/Mag1cFall/AIStudio2API/blob/c77dd89d4ede5cbe55534db75fea864206716de8/README.md)及[发布说明](https://github.com/Mag1cFall/AIStudio2API/releases/tag/v0.3.13)。
- 上游升级先固定 tag 并核实共同历史，然后逐块对照通道／工具历史、额度账、账号恢复、后端与打包。不整目录覆盖，也不为保留旧代码而重复实现上游已有功能。先验本 Fork 对外合同，再验证变更涉及的构建和发布角色。

## 源码、部署与验证

- `45b32f3` 是用于本次比较的已提交快照。Windows 和独立服务端的现役制品另有发布记录，包含各自单独冻结的后续补丁；不能从 `main` 或工作树直接推定各运行角色的版本、配置和健康。例：11 月 20 日单独发布的工具历史补丁允许同一显式通道切换基础模型时保留工具语义；跨通道、裸名和 Assistant 边界未放开。这条装包修订不应倒填为 `45b32f3` 的行为。
- 运行维护 Owner 的 `README.local.md` 和同轮发布记录负责现役身份、备份与回退；源仓不记录生产账号、认证状态或本机数据路径。合入上游或修改本文都不触发发布。
- 未提交的手工模型目录、部分调度和流式内存改动不算入本文已交付能力。本次只核对 Git、源码和已有发布证据，没有新跑产品测试，也未发真实模型请求。历史离线测试、构建、运行健康和真实业务验收按其原记录分开解读。
