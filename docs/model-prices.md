# 模型参考价格单

只用于本体用量页的美元参考估价，不是AI Studio实际扣费、额度余额或公益站账单。历史记录每次查询按当前价格重算；不改请求记录，不锁定请求当时价格。

程序工作目录下的 `config/model-prices.json` 是本实例运行价格单。首次分发模板为 `internal/pricing/prices.json`，已有运行文件升级时不覆盖。Windows/DMIT的权威路径与维护入口见运行Owner，不新增远程价格服务：维护同一份表后同步两端并核对摘要。每端独立原子生效，不声称两端具有分布式切换事务。

## 格式

UTF-8（接受BOM），最多512 KiB。整表替换、不与模板合并；模型缺项就是未定价，不能写0来代替未知。明确0价允许，仍属于已定价。

```json
{
  "schema_version": 1,
  "version": "2026-11-11.base",
  "currency": "USD",
  "unit_tokens": 1000000,
  "models": {
    "gemini-4-argon": {
      "input": 2,
      "cached_input": 0.10,
      "output": 10
    },
    "gemini-2.5-pro": {
      "input": 1.25,
      "cached_input": 0.125,
      "output": 10,
      "long_context": {
        "input_tokens_above": 200000,
        "input": 2.50,
        "cached_input": 0.25,
        "output": 15
      }
    }
  }
}
```

此例仅解释结构，不要用它覆盖完整价格单。`input`是非缓存输入，`cached_input`是输入中缓存命中部分，`output`包含思考和回复。全部价格为每百万Token美元单价，允许0至1,000,000,000的有限数；三个价格均必填。长上下文仅在原始输入Token**严格大于**该模型的阈值时整次请求使用长分档，阈值为正整数，长分档三个价格也须齐全。键名严格小写，模型键为基础模型ID。

初始模板原样迁移15个既有模型的参考价，Carbon未定价；确认参考单价后在完整运行表里补 `gemini-4-carbon` 即可，不套用Argon价格或把两者额度独立当成同价证据。

## 热更新与核验

1. 在同目录临时文件编辑完整价格单，建议修改`version`，再以原子重命名替换正式文件。不要覆盖正在读取的半份内容，不写凭据。
2. 程序每2秒只读检测。仅整表通过JSON、唯一键、已知字段、币种/单位、必填与数值校验后，原子替换不可变快照。文件重排或不改version但改内容也会以新SHA生效。
3. 管理只读 `GET /api/pricing` 沿既有管理员会话/回环规则，返回有效表、version、sha256、source、loaded_at、model_count及last_error。sha256对应原始文件字节（含可能存在的BOM）；返回的price_list是可编码的规范JSON正文，不含BOM。无写API或编辑界面。
4. `GET /api/usage` 增加`pricing_version`与`pricing_sha256`；同份报告的当前、前期和所有桶固定使用该版本。HTTP刷新不会写入请求库。

运行中坏表、缺文件或读取失败保持上一份有效内存价格，日志/管理状态标错；修正后自动恢复。冷启动没有可用运行表则使用内嵌JSON模板，并保留错误与`embedded:prices.json`来源，不冒称恢复了上一进程的自定义内存表。这些错误不阻断生成、调度或配额。
