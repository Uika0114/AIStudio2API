# Assistant（CodeAssistantOffline）客户端工具 wire 证据

Status: historical evidence; native-tool route abandoned, current policy is upstream-style text downgrade.
日期：2026-10-19  
范围：公开只读来源与离线源码；**真实模型请求 0**，未部署、未启停现役、未读取凭据。

Current behavior is documented in [assistant-experiment.md](assistant-experiment.md). The refusal rules and native-tool blockers below describe the earlier investigation stage, not the current public Assistant input policy. No tool or anti-truncation capability is claimed.

本文回答一个问题：`CodeAssistantOffline` 是否有一个可核查的字段/扩展，用来承载客户端函数声明与工具选择（`auto`/`none`/`required`/指定名/parallel），以及助手响应中的结构化 `functionCall`/`functionResponse` 投影。

## 1. 结论

已验证：

- `CodeAssistantOffline` 的 field 1 是对话轮，编码与 Playground `GenerateContent` 的 contents 相同；共享 Content Part 的 JSON 索引 10（`functionCall`）和 11（`functionResponse`）可以作为**历史**运输。
- 上游固定适配的请求字段只有 contents(1)、proof(2)、mode(6)、model(8)、system(9)、applet(12)、framework(14)、16、thinking(19)、请求 ID(29)。
- 助手任务流的步骤字段 10 是**应用侧工具执行**（例如编译应用），不是客户端函数调用 envelope；步骤里的 Part 只记录了文本(field 2)与思考(field 13)。

未找到：

- 任何有公开来源的**函数声明**字段。
- 任何有公开来源的**工具选择 / parallel calls / allowed function names** 字段。
- 任何有公开来源的 **Google 工具**字段。
- 任何有公开来源的**助手响应 functionCall/functionResponse Part 投影**。

因此本轮：

- 不写任何猜测的顶层字段，不把 Schema 拼进提示词，不解析文字工具调用。
- 不翻转 `assistantFunctionToolsImplemented`，不放开工具请求。
- 保留一个只使用共享 Part 索引的历史编码原语（`assistantFunctionHistoryPart`），但文本阶段仍在进入编码前拒绝所有工具材料。
- 这是“缺少可核查映射”的实现阻断，不是“Google 底层不支持工具”的实验结论。

## 2. 固定来源

### 2.1 上游适配（固定 commit）

上游：`Mag1cFall/AIStudio2API` v0.2.13 @ `a784c5193b3ba5c4894d536b2f4cd73e87f99475`。

| 来源 | 内容 |
| --- | --- |
| [`internal/aistudio/assistant.go:94-119`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/internal/aistudio/assistant.go#L94-L119) | `EncodeAssistantRequest`：field 1 contents、6=1、8 model、9 system、12 applet、14 `[2,null,2]`、16=1、19 thinking、29 request ID；field 2 由 WAA proof 写入。没有任何声明/选择字段。 |
| [`docs/build.md:558-584`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/docs/build.md#L558-L584) | `#### CodeAssistantOffline` 的请求示例与字段表；与上一行一致。 |
| [`docs/build.md:566`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/docs/build.md#L566) | field 1 “编码与 Playground `GenerateContent` 的 contents 相同：`[[Part...], role]`”。这是历史 Part 可复用的直接来源。 |
| [`docs/build.md:503`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/docs/build.md#L503) | “`-build` 条目没有函数与 Google 工具能力：工具声明在发送前丢弃，历史中的函数调用与结果按协议规范改写为文本”。这是上游的适配选择，不是协议能力证明。 |
| [`docs/build.md:608-635`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/docs/build.md#L608-L635) | 任务流字段表：步骤 field 10 是“工具执行，例如编译应用”；Part 的 field 2 是文本、field 13 是思考。没有客户端 function call 的响应字段。 |
| [`internal/aistudio/tool_validation.go:9-57`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/internal/aistudio/tool_validation.go#L9-L57) | `supportedTools` 在模型缺 `function_declarations` 能力时清空声明与 allowed names；全部声明被丢弃时把 `ToolConfig` 清零。 |
| [`internal/aistudio/tool_validation.go:60-101`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/internal/aistudio/tool_validation.go#L60-L101) | `transcribeFunctionParts` / `toolPartTranscript` 把调用与结果改写为 JSON 文本。 |
| [`internal/aistudio/generate.go:674-688`](https://github.com/Mag1cFall/AIStudio2API/blob/a784c5193b3ba5c4894d536b2f4cd73e87f99475/internal/aistudio/generate.go#L674-L688) | 通用生成入口先做工具过滤，再对缺能力的模型转写函数历史；assistant 模型条目只声明 `assistant_route`/`chat_model`/`thinking`/`thinking_level`，不含 `function_declarations`。 |

### 2.2 AI Studio 前端基础 bundle（公开静态资源）

抓取时间：2026-10-19（本机）。公开 GET，无登录态、Cookie 或 token。

- URL：`https://www.gstatic.com/_/mss/boq-makersuite/_/js/k=boq-makersuite.MakerSuite.en_US.bfCRZFNeqro.2018.O/am=gRQAmAM/d=1/excm=_b/ed=1/dg=0/wt=2/ujg=1/rs=AMOXD2-CrtsW2olEkTfV71FweLeMJEd2Lg/dti=1/m=_b`
- 大小：2,270,446 bytes
- SHA-256：`ed9c710d462f392d6d28fabcb0f338152d88d3460a82ce42b283f61bf2d24137`

可核查位置（解压后字节偏移 / 行号）：

| 偏移 | 行 | 内容 |
| ---: | ---: | --- |
| 532617 | 1173 | `_.tYa=class extends _.h{...Dg(){return _.m(this,13)}setApiKey(a){return _.l(this,13,a)}zm(){return _.m(this,27)}}`：`CodeAssistantOffline` 的请求类。基础 bundle 只暴露 apiKey(field 13) 与 field 27 两个访问器。 |
| 532927 | 1173 | `_.vYa=new _.Us(".../MakerSuiteService/CodeAssistantOffline",_.tYa,_.uYa,...)`：RPC 定义。 |
| 623021 / 623194 | 1179 | `StreamCodeAssistantOfflineGeneration` 的请求/响应类与 RPC 定义。 |
| 229828 | 628 | `functionDeclarations` 出现在 **Playground GenerateContent** 的配置编码器（`aya`/`Eya`）中，不在 CodeAssistantOffline 请求类上。 |
| 680907 | 1274 | 助手设置项：`codeAssistantMode`、`codeAssistantModel`、`codeAssistantThinkingLevel` 等；没有函数声明/工具选择设置。 |

这组证据能证明：

- 基础模块里 `CodeAssistantOffline` 的请求类没有声明/选择访问器；助手设置也没有工具 UI。
- Playground 的 `functionDeclarations` 编码路径与 CodeAssistantOffline 是分开的。

这组证据**不能**证明：

- `CodeAssistantOffline` 请求没有声明/选择字段。请求类只暴露基础模块用到的访问器；真正的请求构造在按路由懒加载的编辑器模块里，未包含在这个公开基础 bundle 中。空闲字段号不能据此推断。

### 2.3 其它公开检索

| 检索 | 结果 |
| --- | --- |
| Sourcegraph `CodeAssistantOffline` | 22 个命中，全部在 `Mag1cFall/AIStudio2API`；没有第二个公开实现或 proto。 |
| Sourcegraph `alkalimakersuite-pa.clients6.google.com` | 只有上游、规则列表、浏览器扩展的 prompt CRUD、以及基于浏览器拦截的 `aistudio-api`；没有 CodeAssistant 请求构造。 |
| Sourcegraph `x-aistudio-visit-id` | 同上；没有请求体样例。 |
| Sourcegraph `applications.makersuite.v1` + proto 名称变体 | 没有公开 proto/descriptor。 |
| GitHub code search API | 401，需要认证；未使用 token。 |
| grep.app | Vercel 安全校验页，未取得结果。 |
| DuckDuckGo / Bing | `CodeAssistantOffline` 无相关结果；`StreamCodeAssistantOfflineGeneration` 仅一个 AI Studio 论坛故障帖。 |
| Wayback CDX（`gstatic.com/_/mss/boq-makersuite/_/js/*`） | 2,444 条捕获、29 个唯一模块；只有基础 `_b` 与首页/营销模块，没有编辑器模块。`aistudio.google.com/app/*` 的 200 捕获只有 welcome/case-studies/vibe-code，app 路由均为 302。 |
| 2026-03-12 的 5.18MB `_b`（`k=boq-makersuite.MakerSuite.de.9jsDuX-rXds.2018.O`，SHA-256 `74f33be6ee87fd1eb6f063f30f7989a1381d1f9aeba8ac0513cc91a4e8391891`） | 含应用服务层（`CreateApplet` builder、`GetCodeAssistantSnapshot` builder），但没有 `new <CodeAssistantOffline request>`，也没有任何 assistant 工具声明构造；所有 `functionDeclarations` 用法都属于 Playground 配置/对话框。 |
| 2024-10-15 的 `_b`（SHA-256 `d5a9d4fd282921369ad82a9be3aa4041830feb416164a8ffcbeaa9b1a4059def`） | 没有 `CodeAssistantOffline`/`CreateApplet`，是更早的应用拆分，不能补足字段表。 |

结论：在上述取得与检索的公开静态资料中，未找到 CodeAssistantOffline 的完整请求字段表，也未找到带客户端工具声明的请求样例；这不是对所有公开资料或底层能力的穷尽性判断。

## 3. 已核对字段

本节“已验证”仅指固定来源与本地静态编码已复核，不代表 Google 已接受本机请求。尤其是共享历史 Part 可编码，不能提升为 Assistant 已支持原生工具历史。

`CodeAssistantOffline` 顶层（JSON 索引 = field - 1）：

| field | 含义 | 来源 | 状态 |
| ---: | --- | --- | --- |
| 1 | contents | 上游 encoder/docs：566 | 已验证 |
| 2 | WAA proof | 上游 encoder、`docs/protocol.md:216` | 已验证（transport 写入） |
| 6 | `1` | 上游 encoder | 已验证（语义未知） |
| 8 | `models/<base model>` | 上游 encoder | 已验证 |
| 9 | system 文本 | 上游 encoder | 已验证 |
| 12 | applet ID | 上游 encoder | 已验证 |
| 14 | `[2,null,2]` | 上游 encoder | 已验证（语义未知） |
| 16 | `1` | 上游 encoder | 已验证（语义未知） |
| 19 | thinking `[1,null,null,level]` | 上游 encoder | 已验证 |
| 29 | 请求 UUID | 上游 encoder | 已验证 |
| 其它 | — | 无公开来源 | **不写、不猜** |

共享 Content Part（field 1 内部）：

| JSON 索引 | 含义 | 来源 |
| ---: | --- | --- |
| 1 | 文本 | `docs/protocol.md:593`、本地 `encodePart` |
| 10 | functionCall `[name, Struct, id?]` | `docs/protocol.md:595`、本地 `encodePart` |
| 11 | functionResponse `[name, Struct, id?]` | `docs/protocol.md:596`、本地 `encodePart` |
| 12 | thought 标记 | `docs/protocol.md:596`、`docs/build.md:635` |
| 14 | thought signature | `docs/protocol.md:597`、本地 `setPartThoughtSignature` |

注意：这些索引只证明 **contents 内部**的共享编码；不证明 CodeAssistantOffline 顶层存在声明/选择字段，也不证明助手响应会返回这些 Part。

## 4. 为什么历史编码不足以支持客户端工具

1. `required` 或指定函数名需要声明与选择 envelope；只有历史时，模型不知道客户端声明了哪些函数，更不能被约束必须调用。
2. 结构化调用需要助手响应中的 `functionCall` Part 投影；当前没有来源证明该路线会返回共享 Part 索引 10，解码器也不能凭空假设。
3. 调用 ID、参数与结果的关联需要两端都承认同一 envelope；只回传历史不能完成闭环。
4. 助手任务流步骤 field 10 是应用侧工具执行（编译等），不是客户端函数调用；把它当客户端工具会制造假成功。
5. 在声明缺失时把历史塞给一个带服务端工具的助手，可能让服务端工具与客户端历史混用，属于跨语义污染，不能静默尝试。

## 5. 需要什么证据才能继续

按价值排序，全部要求**不含凭据**：

1. AI Studio Build 编辑器模块的公开/用户提供的静态 JS 片段：定义 `CodeAssistantOffline` 请求消息或构造该请求的函数，包含字段号与值类型（尤其是 repeated Tool / ToolConfig）。只取该片段，不需要 Cookie、token 或账号数据。
2. 一次真实 Build 代码助手请求的**脱敏请求体**，其中包含客户端函数声明（如果产品 UI 能产生这种请求）。请求体本身不含凭据；不要提供 Cookie/Authorization。
3. 一次真实响应中出现**客户端自定义函数名**的 `functionCall` Part；这能证明该路线接受声明，但仍需字段号才能实现。
4. Google 公开的 proto/discovery/schema（目前不存在于公开检索）。

以下证据不足，不能据此实现：

- 猜测空闲字段号或写入未知 JSON 字段。
- 把 JSON Schema 拼进 system/contents 提示词。
- 从自然语言回复里解析“我要调用工具”。
- 删除 `required`、把工具改成 `auto`/`none`、或静默删工具来让请求通过。
- 仅凭 Playground 有某个字段，就外推到 Assistant 顶层。

## 6. 独立模块与接线

新增文件：

- `internal/aistudio/assistant_tools.go`
  - `assistantClientToolCapabilitiesForWire()`：唯一的能力声明；当前只有 `FunctionHistory=true`。
  - `assistantClientToolRefusal(request)`：在创建 Applet 前拒绝声明、选择、parallel、Google 工具、抗截断和原生函数/代码执行历史，返回 `ErrInvalidArgument` 且错误文本包含 `assistant tool protocol mapping not implemented`。
  - `assistantFunctionHistoryPart(part)`：用共享 `encodePart` 编码函数调用/结果历史，并核对只落在 Part 索引 10/11（签名允许在 14）。
  - `assistantResponseToolSlot(part)` / `assistantResponseToolEvidenceError(path,label)`：识别助手响应中共享函数槽位并构造“输出映射未实现”的协议证据错误。
- `internal/aistudio/assistant_tools_test.go`：覆盖上述能力、拒绝矩阵、历史 Part 形状、共享 contents 运输、响应槽位识别，以及文本阶段顶层槽位锁定。

主 Session 接线建议（不要求改公共类型）：

1. 在 `validateAssistantRequest` 中用 `assistantClientToolRefusal(request)` 替换现有工具/历史判断；它会额外覆盖 `ToolConfig.ParallelCalls`（现有校验漏掉了这一项）。
2. 保持 `assistantFunctionToolsImplemented()` 为 false，直到拿到第 5 节的证据；能力位不要只看“历史可编码”。
3. 将来拿到声明/选择字段后：在 `EncodeAssistantRequest` 里写入新字段，并用 `assistantFunctionHistoryPart` 产出 field 1 内的历史 Part；不要另造文本运输。
4. 在 `decodePart` 中用 `assistantResponseToolSlot` 区分 `functionCall`/`functionResponse` 与其它未知 Part，并用 `assistantResponseToolEvidenceError` 报精确错误；不要把它们当空文本。
5. 若最终确认只能文本生成，再单独裁决“严格拒绝”还是“显式文本降级”；不要在本轮默认降级。

最小接线形态：

```go
// validateAssistantRequest
if err := assistantClientToolRefusal(request); err != nil {
	return err
}

// decodePart：先于通用未知 Part 检查
if label, ok := assistantResponseToolSlot(part); ok {
	return nil, assistantResponseToolEvidenceError(path, label)
}
```

## 7. 离线验证

通过 `D:/dev/AIStudio2API/Invoke-Dev.ps1`（共享 Go 1.26.9、GOTOOLCHAIN=local、GOPROXY/GOSUMDB=off）：

| 动作 | 日志前缀 | 结果 |
| --- | --- | --- |
| `go test -count=1 -timeout=2m ./internal/aistudio -run AssistantTool` | `D:/dev/AIStudio2API/artifacts/logs/20261019-093931-238-d6b15fe0-go` | EXIT=0 |
| `go test -count=1 -timeout=3m ./internal/aistudio` | `D:/dev/AIStudio2API/artifacts/logs/20261019-093942-274-292fc137-go` | 仅既有环境项 `TestUserCacheRootWithoutEnvironment` 失败（隔离 HOME 未生效），其余通过 |
| `go test -count=1 -timeout=3m ./internal/aistudio -skip ^TestUserCacheRootWithoutEnvironment$` | `D:/dev/AIStudio2API/artifacts/logs/20261019-094220-400-ae12d3b5-go` | EXIT=0 |
| `go vet ./internal/aistudio` | `D:/dev/AIStudio2API/artifacts/logs/20261019-093954-717-f19b3a9e-go` | EXIT=0 |

所有 fixture 为内存合成数据；未运行产品 EXE、WAA/Chrome、未创建真实 Applet、未发 Google 生成/额度查询。**真实模型请求 0。**

## 8. 主会话复核与接线（同日）

主会话重新 GET 并保存同一公开基础 bundle，2,270,446 bytes 与 SHA-256 `ed9c710d462f392d6d28fabcb0f338152d88d3460a82ce42b283f61bf2d24137` 均匹配；复核了上列 RPC/请求类和偏移。快照位于开发产物目录，不包含登录态。

已把 `assistantClientToolRefusal` 接入核心校验，并在公开 `EncodeAssistantRequest` 再次校验，避免未来直接调用编码器时静默丢掉未映射的工具/预算。`decodePart` 对占据共享函数槽位的 Part 明确报未知投影错误，不把它当已验证的 Assistant 函数事件；错误仍保留已知 usage，不返回正常完成、不重发生成。能力位仍为 false。

新增 `assistant_tool_boundary_test.go` 覆盖 parallel=true/false 在资源创建前拒绝、直接编码器不能旁路、共享函数槽位遇错不泄露文本/不伪造成功/不重发；`internal/api/assistant_tool_contract_test.go` 覆盖三协议入口的 Schema、auto/none/required/指定函数以及 OpenAI parallel=false 保留。它们不构成上游支持证明。

主会话最终验证：全量 Go 测试（仅将已审计的 Windows 原生缓存路径项分开）`20261019-095123-266-773cba42-go`；原生缓存路径单项 `20261019-095146-340-f9835fda-go`；vet `20261019-095150-699-a3925a86-go`；前端 typecheck/lint/build 分别 `20261019-095123-269-669c1253-npm` / `20261019-095131-103-9523761e-npm` / `20261019-095152-716-32e7b9b2-npm`，均 EXIT=0。前端仍有现存 bundle 体积/插件耗时提示，不是工具能力验证。原 Worker 的原生路径环境失败没有删记，主会话使用 `Invoke-Dev.ps1 -NativeCachePathTest` 单独通过该项。

**交付仍为文本阶段的边界修订，不是原生工具闭环或工具抗截断实现；真实模型请求仍为 0，部署及真实 canary 留给用户另行操作。**

## 9. 官方编辑器模块追加证据（同日，Worker）

已从当前基础 bundle 的路由表取出 Applet 编辑器的懒加载模块名 `Jn51xb`，并**不需要登录**直接取得官方真实请求构造：`https://www.gstatic.com/_/mss/boq-makersuite/_/js/k=boq-makersuite.MakerSuite.en_US.bfCRZFNeqro.2018.O/am=gRQAmAM/d=1/excm=_b/ed=1/dg=0/wt=2/ujg=1/rs=AMOXD2-CrtsW2olEkTfV71FweLeMJEd2Lg/dti=1/m=Jn51xb`（7,577,004 bytes，SHA-256 `92d5a9949f4e70bdf613420fa4eb3a24886cec81f5949dbb1d7b3819b5f04003`）。

结论：`_.Onc` 构造 `CodeAssistantOffline` 请求时只写 contents(1)、proof(2)、mode(6)、system(7/9/15)、model(8)、`d.UQ`(10)、applet(12)、apiKey(13)、config(14)、16、thinking(19)、20、`_.$0b`(21)、文件 map(22)、23、26、artifact comments(28)、请求 ID(29)，**没有任何函数声明/工具选择/parallel 字段**；`new _.tYa` 只出现一次；`functionDeclarations` 全部属于 Playground。助手工具（`ask_question`、`show_aistudio_ui` 等）是服务端事件，由 `_.sjc` 的分支 1/10/3/18/9/19/5/6 处理，没有客户端 `functionCall` 分支。

完整字段表、偏移与边界见 [`assistant-editor-module-evidence.md`](assistant-editor-module-evidence.md)。该证据不改变本文件第 1 节的能力位结论，只把“找不到声明字段”从“基础 bundle 未覆盖”升级为“官方客户端没有这条路径”。
