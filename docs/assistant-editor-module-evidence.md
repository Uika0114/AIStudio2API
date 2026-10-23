# Assistant 官方编辑器模块证据（`CodeAssistantOffline` 真实请求构造）

Status: evidence addendum（追加，2026-10-19）  
范围：公开静态 GET，无登录态、无 Cookie/token；**真实模型请求 0**。

本文补上 `docs/assistant-tool-wire-evidence.md` 2.2 节留下的缺口：基础 bundle 不构造 `CodeAssistantOffline` 请求，真正的构造在按路由懒加载的编辑器模块里。该模块可以**不需要登录**直接从公开 CDN 取得。

## 1. 取得方式

基础 bundle 的路由表包含懒加载模块名（Closure `goog.require`）：

- `apps/:id`、`apps/:source/:id` → `(0,_.$n)("Jn51xb")`（Applet 编辑器）
- 应用外壳 → `tbJjMb`；Build Agent → `Eyfuqf`

用基础 bundle 的公开参数直接 GET：

```
https://www.gstatic.com/_/mss/boq-makersuite/_/js/k=boq-makersuite.MakerSuite.en_US.bfCRZFNeqro.2018.O/am=gRQAmAM/d=1/excm=_b/ed=1/dg=0/wt=2/ujg=1/rs=AMOXD2-CrtsW2olEkTfV71FweLeMJEd2Lg/dti=1/m=Jn51xb
```

- 7,577,004 bytes；SHA-256 `92d5a9949f4e70bdf613420fa4eb3a24886cec81f5949dbb1d7b3819b5f04003`
- 同参数下 `m=tbJjMb`（2,270,613 bytes，SHA-256 `1dfe8179a519b849…`）、`m=Eyfuqf`（6,328,005 bytes，SHA-256 `50793bd44cbbd77b…`）也可取得；`Jn51xb` 是唯一包含 `new _.tYa` 的模块。

## 2. 真实请求构造 `_.Onc`

`_.Onc` 构造 `CodeAssistantOffline` 请求类 `_.tYa`，`_.Pnc` 调用它并 POST 到 `MakerSuiteService/CodeAssistantOffline`：

```js
p = new _.tYa;
e = _.yc(p,_.llc,14,e);      // field14 = llc{1:2, 3:framework, 4:bool, 5:bool}
e = _.vl(e,1,_.tt,f);        // field1  = contents
e = _.eo(e,6,c);             // field6  = mode
e = _.l(e,8,g);              // field8  = model
e = _.yc(e,_.Os,19,f);       // field19 = thinking config (field4 = level)
u = _.l(e,12,f);             // field12 = appletId
u = _.qo(u,16,!0);           // field16 = true
u = _.eo(u,20,k);            // field20 = int
if (d.sT) _.Ql(u,23,_.$0b,!0);
else { ... && _.Pl(u,21,_.$0b,k); }   // field21 = _.$0b
d.apiKey && u.setApiKey(d.apiKey);    // field13 = apiKey
d.uSb && _.l(u,29,d.uSb);             // field29 = request id
d.UQ!==void 0 && _.l(u,10,d.UQ);      // field10 = int
(w=yield a.Zz()) && (a.H.Pt()? _.l(u,15,w):_.l(u,9,w));  // system field15/9
_.bL && (w=a.aa(), _.l(u,7,w));       // field7
// field26 = true（有附件）；field22 = 文件 map（path → content）
B=yield Imc(u); _.l(u,2,B);           // field2 = WAA proof
// field28 = repeated artifactComments
return u;
```

字段映射（`_.tYa`）：

| field | 官方客户端写入 | 与上游 `a784c519` 对照 |
| ---: | --- | --- |
| 1 | contents（`Ijc`） | 一致 |
| 2 | WAA proof | 一致 |
| 6 | mode | 一致（上游 1） |
| 7 | `a.aa()`（条件） | 上游未用 |
| 8 | model | 一致 |
| 9 / 15 | system（按 `Pt()` 二选一） | 上游用 9 |
| 10 | `d.UQ`（条件） | 上游未用 |
| 12 | appletId | 一致 |
| 13 | API key | 基础 bundle 的 `setApiKey` |
| 14 | `_.llc{1:2, 3:framework, 4, 5}` | 与 `[2,null,2]` 同形 |
| 16 | `true` | 上游 1 |
| 19 | thinking（field4 = level） | 一致 |
| 20 | 模型默认/选项（int） | 上游未用 |
| 21 | `_.$0b` | 上游未用 |
| 22 | 文件 map | 上游未用 |
| 23 | `true`（条件） | 上游未用 |
| 26 | `true`（有附件） | 上游未用 |
| 28 | artifact comments | 上游未用 |
| 29 | 请求 ID | 一致 |

**没有任何函数声明、工具选择、allowed function names 或 parallel 字段。** 模块里 `new _.tYa` 只出现一次（`_.Onc`）；`functionDeclarations` 的 20 处全部属于 Playground 配置/对话框与 `GenerateContent` 编码器（`aya`/`Eya`），没有一处进入 assistant 请求。

## 3. 助手工具是服务端工具

- 流式响应由 `jpc` → `_.sjc` 映射；`_.sjc` 的 oneof 分支只有 1/10/3/18/9/19/5/6（文本、应用事件、`ask_question`、`show_aistudio_ui`、恢复、错误等），没有客户端 `functionCall` 分支。
- 助手内置系统提示（`gkc`）明确要求用 `ask_question` 工具提问；`_.wjc` 解析 `ask_question`、`_.vjc` 解析 `show_aistudio_ui`。这些都是服务端事件。
- 编辑器里的 `functionCall`/`functionResponse` 处理（`Bnc`/`Dnc`/`Ykb`/`dlb`）属于 **Playground 历史与 Build Agent 事件**渲染，不是 `CodeAssistantOffline` 的客户端工具 envelope。

### 3.1 官方助手的服务端工具名（可枚举）

同一模块里有一个工具名分类表 `_.Yjb` 与集合 `Zjb`（偏移约 810578，均在 `m=Jn51xb` 的 SHA-256 版本内）。这些名字是**服务端执行**的工具，不是调用方声明的函数；列表用于回答“官方助手自己有哪些工具”，不代表 Assistant 路由接受客户端工具。

- 终端类：`bash`、`code_execution`、`run_command`、`shell_exec`
- 读文件：`read`、`read_file`、`view_file`
- 列目录：`find_by_name`、`list_dir`、`list_files`
- 写文件：`create_file`、`write`、`write_file`、`write_to_file`
- 搜索：`google_search`、`google:search`、`google_search_call`、`search_web`
- 其它（`Zjb` 第二组）：`browser_subagent`、`call_mcp_tool`、`cloudsql_execute_sql`、`cloudsql_update_schema`、`code_search`、`command_status`、`compile_applet`、`delete_file`、`deploy_firebase`、`edit`、`edit_file`、`file_search_call`、`generate_image`、`google_maps_call`、`grep_search`、`install_applet_dependencies`、`install_applet_package`、`lint_applet`、`manage_task`、`mcp_server_tool_call`、`moma_search`、`multi_edit_file`、`multi_replace_file_content`、`notebook_edit`、`read_url_content`、`remove_oauth`、`replace_file_content`、`restart_dev_server`、`rpc_action`、`send_command_input`、`set_up_cloud_sql`、`set_up_firebase`、`set_up_oauth`、`show_aistudio_ui`、`url_context_call`、`workspace_api`
- 提案类（`gxa`）：`propose_agent`、`propose_system_instruction`、`propose_skills`、`propose_skill_update`、`propose_connectors`、`propose_trigger`、`ask_clarifying_question`、`propose_topic_deck`（另有 `functions.` 前缀的同名变体）
- 交互元数据 tag：`ask_question`；`show_aistudio_ui` 的 ui_type 集合为 `paid_model_flow`、`cloudsql_setup`、`firebase_setup`、`google_maps_setup`

注意：这些名字里既有离线代码助手的工具，也有 Build Agent 的工具；它们都通过流式事件返回，客户端不声明、不执行。


## 4. 结论与边界

- **官方客户端没有向 `CodeAssistantOffline` 发送客户端函数声明或工具选择的路径**；助手的能力是服务端工具。
- 这仍不是“服务端不可能接受某个未使用字段”的数学证明：理论上存在官方客户端未使用的字段。但该字段无法通过逆向官方客户端得到。
- 继续逆向官方客户端不会再产出声明字段；能改变结论的只剩：服务端 proto/descriptor（未公开）、或一次明确授权的真实 canary 实验（本契约禁止靠猜字段实现）。

## 5. 未做与不声称

- 未使用登录态、Cookie、token、账号控制台或真实生成请求；**真实模型请求 0**。
- 未修改任何运行配置、账号、出口或 Pi。
- 不声称“Google 不支持 Assistant 客户端工具”，只声称“官方客户端没有这条路径，且公开证据没有字段号”。
