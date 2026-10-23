# 独立 Chrome User Data 根与设备绑定续签

本 Fork 支持在 Windows 上从明确指定的 Chrome User Data 根导入账号，并保存 `chrome_root + Profile + Gaia` 来源。这里沿用项目对设备绑定认证恢复的 DBSC 称呼；不表示实现了另一套通用浏览器会话协议。

这不是 Profile 搬迁器，不复制日常 Chrome，不修改 Chrome／ABE／Sync 策略，也不增加 Linux 本地续签或 SSH 远程续签。指定 Profile 是否实际产生可用的绑定材料，仍需原生账号验证；离线 fixture 通过不等于所有账号均可导入。

## 使用边界

- 根是 `User Data` 目录，不是 `chrome.exe` 所在目录，也不是其下的 `Default`／`Profile 1`。
- 首次导入前正常退出对应 Chrome。本期不承诺活跃数据库在线导入；检测到非空 WAL 时拒绝，而不是用过期主库继续。
- 只支持明确的本地绝对根；相对路径、逃逸、网络根、重解析点和歧义别名失败，不尝试其他目录。
- `v10` 使用所选根的原生 DPAPI 密钥，不调用 v20 helper、不启动 Chrome；`v20` 保留现有 APPB/helper 路线。
- 缺设备绑定、未知密文格式、未支持的 mTLS token binding、解密或身份核对失败均报错，不降级成 cookies-only 成功。
- 发现页面的 `importable` 是结构提示，不证明密钥可解、Google 接受绑定或模型有额度。不要把网页登录、Chrome 登录或 Sync 状态当作设备绑定成功的证明。

## 管理页

1. 打开“Chrome 批量导入”，不会自动发现或扫描默认 Chrome。
2. 选择“专用 Chrome 实例”，填写绝对 User Data 根；确实要用日常实例时，明确选择“默认 Chrome”。
3. 点击“读取所选目录”，核对返回的规范根和身份，再勾选导入。
4. 切换目录会清空旧选择；旧请求迟到也不会覆盖新目录的结果。

已有同邮箱账号不会被普通新增覆盖。需要更换或补齐来源时，从该账号的“绑定 Chrome 来源”进入，选择一个身份并明确确认。账号忙时拒绝，竞态租约等待有界；不会强制取消生成。绑定沿用原账号 ID、启用状态、代理、语言、时区、模型和冷却记录，仅更新认证状态与来源。绑定后仍可能因原冷却／停用状态而不能调度。

## 管理 API 合同

新客户端使用独立的来源路由，避免旧二进制忽略 root 后扫描默认 Chrome：

- `GET /api/accounts/import/chrome/source?chrome_root=<URL编码的绝对根>`
- `POST /api/accounts/import/chrome/source`

GET 必须显式携带 `chrome_root`；明确的空值表示选择默认 Chrome。响应包含已解析的 `chrome_root` 和 `profiles`。POST 则必须带发现结果中的非空规范根及不透明 `account_ids`，不能自行拼接、删掉根上下文或复用另一根的选择。

新增的请求结构示例（占位 ID 必须替换为同次发现返回值）：

```json
{
  "chrome_root": "C:/ChromePool",
  "account_ids": ["<opaque id from discovery>"],
  "locale": "zh-CN",
  "timezone": "Asia/Shanghai"
}
```

单账号重绑的请求结构：

```json
{
  "chrome_root": "C:/ChromePool",
  "account_ids": ["<one opaque id from discovery>"],
  "rebind_account_id": "fixture@example.com",
  "confirm_rebind": true
}
```

重绑不接受代理／语言／时区设置；使用原账号出口，核对原 Gaia（已知时），不会借“重绑”新增账号或制造新额度。身份冲突、忙账号、取消、认证交换、运行时准备或原子保存失败不会用不完整结果覆盖原材料。批量新增若已保存部分账号而后续失败，会报告部分成功；不自动删除前批账号，也不谎称撤销了 Google 认证副作用。

旧 `/api/accounts/import/chrome` 显式默认导入接口仍兼容。新 UI 不调用旧路由，也不会在来源路由失败时退回默认发现。

## CLI 与正常续签

CLI 继续使用 `setup --chrome-root`，规范化和拒绝歧义路径的规则与管理端一致。交互式选择保留完整不透明 ID。`--profile` 与 `--email` 仍是原有并集选择，不是“Profile 内的这个 email”交集授权；单账号验证请使用管理页选定的精确身份，不扩大到批量导入。

正常续签继续只使用保存的 refresh token／设备绑定材料，不读取 Chrome 根，也不要求 Chrome 常驻。只有既有明确凭据拒绝条件才允许重新读取来源；新来源仅访问绑定的根、Profile、Gaia，网络或其他协议错误不会触发换根。

旧来源缺根不妨碍已有材料的正常续签。但材料被拒后，必须明确绑定／重绑来源，不能猜默认 Chrome，也不会批量回填历史来源。

## 回退与验收

旧二进制不理解新来源字段。旧结构读取后重写认证扩展可能丢掉根和 Gaia，旧版回源还可能扫描默认 Chrome；“能忽略字段”不是安全回退证明。退回旧版本前应保护或停用本次新增／重绑的专用来源账号，保留最新账本和认证数据，不恢复整个旧号池快照。

真实导入、Google OAuth、账号来源重绑和现役换版分别需要操作员确认；生成 canary 不由认证验证自动授权。新增的 Go fixture 与 `web/scripts/verify-chrome-source.cjs` 只覆盖合成数据、API 合同及脚本态 UI 交互，不是实际 Chrome／账号或浏览器渲染验收。
