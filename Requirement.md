---
AIGC:
  ContentProducer: '001191110102MAD55U9H0F10002'
  ContentPropagator: '001191110102MAD55U9H0F10002'
  Label: '1'
  ProduceID: 'cf7696a9-5514-4b67-84d3-23fcaa77069b'
  PropagateID: 'cf7696a9-5514-4b67-84d3-23fcaa77069b'
  ReservedCode1: '62372f10-fb14-4a28-83fd-721a670fd182'
  ReservedCode2: '62372f10-fb14-4a28-83fd-721a670fd182'
---

# LLMProxy 大模型代理工具 — 需求规格说明书

## 文档信息

| 项目 | 说明 |
|------|------|
| 名称 | LLMProxy 大模型代理工具 |
| 后端技术栈 | Go 语言 |
| 前端技术栈 | 原生 HTML + JavaScript（无前端框架） |
| 持久化 | 配置统一存放于 `config.yml`；运行数据（调用日志）存放于 SQLite 数据库 |
| 构建脚本 | `build.bat`（Windows）/ `build.sh`（Linux/macOS），支持当前平台编译与交叉编译 |
| 文档状态 | v1.8.5（2026-10-06）：第四轮全面检查修复（先实测复现核实、后修复）——日志读接口 store 引用锁内获取与路径切换持锁（G1/G2，v1.8.4 A5 补全）、Responses 兜底合成事件顺序与 reasoning summary 通道解析（G3/G4）、fetch-models ctx 贯穿与 10 分钟总预算（G5）、流式统计口径修正（G6/G7/G8）、文档口径修订（G9/G10），详见 12 版本更新记录。v1.8.4（2026-10-06）：第三轮全量复查修复（先实测复现核实、后修复）——Anthropic 图文混排跨格式丢图（H1）、thinking/reasoning 跨格式通道打通（M6）、断开与重试终止修正（M2/M3）、配置保存 write-skew（M7）、存储与安全收口（S3-S15，含 JWT 摘要改 HMAC：升级后需重新登录一次）、登录 IP 限流（M10）、管理 API 边界修正（A2/A4-A11）、前端 8 项健壮性修复（F1-F8）、注释归属与文档口径修订（D1-D9/S14/P5），详见 12 版本更新记录。v1.8.3（2026-10-03）：第二轮全面复查修复——日志库重建失败自愈（M1）、清空日志引用竞态（M2）、日志路径切换持久化失败回滚（M3）、编辑页自动获取统一表单模式（F1）、三视图迟到响应守卫（F2）、非安全上下文复制降级（F3）、日志时间参数 400（L1）、JWT 有效期兜底（L2）、Key 唯一性竞态（L3）、热加载停止幂等（L4）与失败提示（L6）、别名类型校验（L7）、短 Key 脱敏（L8）、token 用户名校验（L10）、错误消息 UTF-8 截断（L11）、测试上限 50（L12）、前端 8 项健壮性修复（F4/F6~F12）、文档编号与口径修订（D 类），详见 12 版本更新记录。v1.8.2（2026-10-03）：全面代码审查修复——坏 SSE 伪造成功（A1）、流式首字节前断开仍重试（A2）、Responses 文本块累积（A3）、白名单首候选口径（A4）、日志库切换竞态（B1）、详情页客户端响应回退（B2）、构建失败烧 Key 槽位（C1）/死代码（C2）/索引越界（C3）/恒真分支（C4）/截断中文乱码（C5）/多行 data usage（C6）/冷却开关同步（C9）/渲染序号守卫（C10）、文档与注释修订（D1-D5，含章节编号修正），详见 12 版本更新记录。v1.8.1（2026-10-03）：日志记录开关与清空释放空间——设置页新增「记录调用日志」开关（`log.enabled`，**默认关闭**，热加载，旧配置无此字段同样视为关闭）；「删除全部日志」改为**删除日志数据库文件并重新生成**（含 -wal / -shm 伴生文件，磁盘空间即时释放），详见 3.1、4.4 与 12 版本更新记录。v1.8.0（2026-09-30）：调用详情标签页化与内容解析——详情弹窗改为「客户端记录 / 上游记录 / 原始数据」三标签页，请求体与响应体结构化解析（消息卡片、tool_calls 参数格式化、SSE 流重组视图），详见 4.3 与 12 版本更新记录。v1.7.6（2026-09-30）：全面代码审查修复——日志库关闭竞态（H1）、配置热加载数据竞争（H4）、统计接口脱敏（H3）、别名解析模型名错配（M1）、白名单别名误拒（M2）、客户端断开日志归类（M3）、转换器异常路径（M5/M7/M8）、IP 白名单规范化、前端渲染竞态等，详见 12 版本更新记录。v1.2（2026-09-21）：新增多接口格式支持——供应商可选接口类型（Chat Completions / Responses API / Anthropic API），用户暴露端点新增 `/v1/responses` 与 `/v1/messages`，任意"客户端格式 × 供应商格式"组合自动双向转换。v1.1（2026-09-19）：对齐实际实现 |

---

## 1 项目概述

### 1.1 项目目标

开发一个大语言模型代理工具，统一管理各供应商的大模型服务。系统接收终端用户的模型调用请求，经身份认证、路由选择后转发至上游供应商，并将调用结果返回给用户，同时记录完整调用日志供审计和统计使用。

### 1.2 核心能力

- 支持接入多个模型供应商，对外暴露三种大模型 API 接口：OpenAI Chat Completions、OpenAI Responses API、Anthropic Messages API
- 供应商接口类型可选（Chat Completions / Responses API / Anthropic API），用户调用格式与供应商格式可以不同，代理自动双向转换
- 支持给终端用户发放代理 API Key，进行身份认证与访问控制
- 支持灵活的路由策略（随机 / 轮询 / 加权随机）
- 支持失败熔断（冷却池）
- 完整记录所有调用日志，支持搜索与统计
- 提供管理员登录认证与管理界面
- 配置变更热加载，免重启生效

### 1.3 系统架构

```
终端用户
  ├─> GET /v1/models（获取可用模型列表）
  ├─> POST /v1/chat/completions（OpenAI Chat Completions 格式）
  ├─> POST /v1/responses（OpenAI Responses API 格式）
  └─> POST /v1/messages（Anthropic Messages API 格式）
        ├─ [1] 代理 API Key 认证（功能二）
        ├─ [2] 模型别名解析（功能一/功能八）
        ├─ [3] 路由选择（功能二：路由策略 + 冷却池）
        ├─ [4] 上游转发（功能五）
        │     ├─> 供应商类型与客户端格式相同 → 透传至 {base_url}/chat/completions | /responses | /v1/messages
        │     └─> 供应商类型与客户端格式不同 → 请求/响应自动格式转换
        │           └─> 上游供应商（功能一，三种接口类型之一）
        ├─ [5] 调用日志写入（功能三）
        └─> 返回响应给用户（按客户端格式）

管理界面（HTML 前端）
  ├─ /providers          供应商管理（功能一/功能七）
  ├─ /apikeys            代理 Key 管理（功能二）
  ├─ /settings           基础设置 / 路由策略 / 冷却池（功能二）
  ├─ /dashboard          仪表盘（功能四）
  ├─ /statistics         统计页面（功能四）
  └─ /logs               调用日志（功能三）
```

### 1.4 两类 Key 的区分

| 类型 | 用途 | 管理位置 |
|------|------|----------|
| **供应商 API Key** | 本代理调用上游大模型服务时使用 | 功能一（供应商管理） |
| **代理 API Key** | 终端用户调用本代理时进行身份认证 | 功能二（设置功能） |

---

## 2 功能一：供应商管理

### 2.1 功能概述

对模型供应商进行增、删、改、查、启用、停用管理。供应商指一个提供大模型 API 的服务方（如 OpenAI、Anthropic、硅基流动等）。

### 2.2 列表页（`/providers`）

| 区域 | 内容 |
|------|------|
| 顶部操作 | 「新增供应商」按钮 |
| 表格列 | 名称、基础地址、接口类型、模型数量、API Key 数量、是否启用、操作 |
| 操作列 | 修改、删除、启用/停用 |

**接口类型显示**：Chat Completions（默认）/ Responses API / Anthropic API。

**交互规则：**

- 删除：二次确认弹窗，确认后执行真删除。
- 启用/停用：切换供应商 `enabled` 状态。停用后代理层不再路由到该供应商。

### 2.3 新增 / 修改页（`/providers/new`、`/providers/:id/edit`）

新增与修改复用同一页面，修改时回填已有数据。页面底部提供「保存」与「返回」按钮；点击「保存」成功后返回供应商列表页（保存失败时留在当前页并提示错误）。

#### 2.3.1 页面分区与表单字段

新增/修改页自上而下分为五个分区（卡片）：

**① 基本信息**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| 名称 | 文本 | 是 | 供应商显示名称，如「OpenAI」 |
| 基础地址 | 文本 | 是 | API Base URL，如 `https://api.openai.com/v1`（Chat Completions / Responses 类型）或 `https://api.anthropic.com`（Anthropic 类型，末尾不带斜杠） |
| 接口类型 | 下拉框 | 是 | `OpenAI Chat Completions`（默认）/ `OpenAI Responses API` / `Anthropic Messages API`；决定请求上游的接口格式（见 2.3.6） |
| 权重 | 数字 | 否 | 路由权重，默认 1，用于加权随机策略（见功能二 3.3） |
| 是否启用 | 开关 | 是 | 默认启用 |

表单内附接口类型说明文案：Chat Completions 发往 `{基础地址}/chat/completions`；Responses API 发往 `{基础地址}/responses`；Anthropic 发往 `{基础地址}/v1/messages`（携带 `x-api-key` 头）。用户调用格式与供应商格式不同时，代理自动双向转换。

**② 模型测试**（位于 API Key 区域上方）

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| 测试模型 | 下拉框 | 测试时必选 | 从「模型列表」中所有模型名称里选择其一，用于 API Key 测试；模型列表非空时**默认选中第一个模型**（用户手动改选后跟随用户选择；所选模型被删除且列表非空时重新回落到第一个）。保存供应商时不校验此项；未选时禁止发起测试但允许保存 |

卡片内含「测试」按钮，对供应商的**所有** API Key 依次发起批量测试（见 2.5.1）。

**③ API Key（多行可增删）**

每条含一个 Key 输入框 + 测试按钮 + 删除按钮；支持添加多条（见 2.3.2）。

**④ 请求头（键值对多行）**

每条含 key 输入框 + value 输入框 + 删除按钮；支持添加多条（见 2.3.2）。

**⑤ 模型列表（多行可增删）**

每条含「模型名称」+「模型别名」+ 删除按钮；区域上方提供「添加模型」「自动获取」操作按钮（见 2.3.4、2.3.5）。

#### 2.3.2 API Key（多值）

- 每条记录 = Key 输入框 + 「测试」按钮 + 「删除」按钮。
- 顶部「添加 API Key」按钮新增一条空记录。
- 每条 Key 可单独测试（见 2.5），测试结果（成功/失败）显示在该条记录旁。

#### 2.3.3 请求头（多值，键值对）

- 每条记录 = key 输入框 + value 输入框 + 「删除」按钮。
- 顶部「添加请求头」按钮新增一条空记录。
- 调用供应商接口时，这些请求头会被附加到发出的 HTTP 请求中。

#### 2.3.4 模型列表（多值）

- 每条记录 = 「模型名称」输入框 + 「模型别名」输入框 + 「删除」按钮。
- 列表区域**上方**提供「添加模型」按钮新增一条空记录，旁边为「自动获取」按钮（见 2.3.5）。
- **模型名称**：真实模型标识（如 `gpt-4o`），调用时传给供应商的实际值。
- **模型别名**：可选，可为空；多个别名用英文逗号 `,` 分隔（如 `gpt4,four-o,g4`）。
- **别名作用**：调用方传入别名时，代理层自动将其替换为对应的真实「模型名称」再发往供应商。传入真实模型名称时直接透传。
- **转换逻辑**：前端用户输入别名字符串，后端存储时按英文逗号分割并去掉空格，转换为 `aliases` JSON 数组（如 `"gpt4, four-o"` → `["gpt4","four-o"]`）；调用解析时按相同逻辑将用户传入的模型名与数组各元素逐一匹配（大小写不敏感）。
- **模型名称唯一性**：同一供应商内模型名称不允许重复（后端唯一约束；前端实时提示重复）；新增时若名称已存在则拒绝或跳过。

#### 2.3.5 自动获取模型

在模型列表区域上方，「添加模型」按钮旁边提供「自动获取」按钮。

- **触发条件**：基础地址已填写 + 至少有一条 API Key。
- **调用方式（v1.8.3 统一表单模式）**：新增页与编辑页统一走 POST 表单模式，使用**表单当前填写的数据**（base_url、接口类型、所有 Key、表单当前填写的自定义请求头），无需先保存供应商；编辑页表单中的脱敏 Key（含 `***`）由后端按 provider_id + Key id 从已保存配置恢复完整值参与获取（与 2.5.1 测试数据来源同模式），无法恢复的 Key 跳过。后端按顺序依次尝试各 Key 调用上游 `GET` 模型列表接口（按接口类型：Chat Completions / Responses 发往 `{base_url}/models`，Anthropic 发往 `{base_url}/v1/models`，认证头按 2.3.6 规则），以第一个返回 200 的 Key 的响应为准。
- **响应兼容**：兼容不同厂商 `/models` 响应格式，取 `data[].id` 或 `data[].name` 等常见字段。
- **失败处理**：所有 Key 均失败 → 弹出提示「获取模型失败：{原因}」，不影响现有表单数据。
- **选择弹窗**：
  - 展示获取到的所有模型列表。
  - 支持全选 / 取消全选。
  - 已存在于当前供应商的模型名称默认不勾选（过滤掉）。
  - 用户确认后，所选模型追加到列表（不覆盖已有模型）。
- **追加结果**：模型列表中自动填入所选模型的「模型名称」，别名留空供用户按需填写。

#### 2.3.6 接口类型与上游请求规则

供应商接口类型（`api_type`）决定代理请求该供应商时使用的接口格式与端点：

| 接口类型 | 上游端点 | 认证头 | 请求/响应格式 |
|----------|----------|--------|----------------|
| `openai`（Chat Completions，默认） | `POST {base_url}/chat/completions` | `Authorization: Bearer {key}` | OpenAI Chat Completions |
| `responses`（Responses API） | `POST {base_url}/responses` | `Authorization: Bearer {key}` | OpenAI Responses API |
| `anthropic`（Anthropic API） | `POST {base_url}/v1/messages`（base_url 以 `/v1` 结尾时拼 `/messages`） | `x-api-key: {key}` + `anthropic-version: 2023-06-01` + `Authorization: Bearer {key}` | Anthropic Messages API |

- **默认值与兼容**：旧配置（无 `api_type` 字段）自动视为 `openai`；取值不合法时后端保存报 400。
- **格式与端点解耦**：用户调用本代理时使用哪种格式（三个代理端点之一）与供应商类型无关，代理自动在两者之间转换（见 6.7）。

### 2.4 测试模型下拉框

- 位于「模型测试」卡片内（API Key 区域上方）。
- 下拉框数据源 = 当前表单「模型列表」中的所有模型名称（实时联动：新增/修改/删除模型行时同步刷新）。
- 模型列表非空时默认选中第一个模型；列表为空时下拉框为空，测试功能不可用（提示先添加模型）。

### 2.5 模型测试（API Key 连通性验证）

#### 2.5.1 测试触发方式

**批量测试**：测试模型下拉框右侧提供「测试」按钮，用选中的测试模型对供应商的**所有** API Key 依次发起测试请求，每条显示各自结果。

**单条测试**：每条 API Key 记录右侧有独立的「测试」按钮，仅对该条 Key 发起测试请求。

**测试数据来源**：批量/单条测试均直接使用**表单当前填写的内容**（基础地址、API Key、请求头），无需先保存供应商。已保存供应商的脱敏 Key（`sk-***xxx`）按其 id 从已保存配置自动恢复完整值参与测试；保存供应商后 Key id 会变化，旧测试结果自动清空。

#### 2.5.2 测试逻辑（后端）

按供应商接口类型（`api_type`）构造测试请求：

| 接口类型 | 请求地址与请求体 |
|----------|------------------|
| `openai` | `POST {base_url}/chat/completions`，`{"model":...,"messages":[{"role":"user","content":"<测试发送内容>"}],"max_tokens":16}` |
| `responses` | `POST {base_url}/responses`，`{"model":...,"input":"<测试发送内容>","max_output_tokens":16}` |
| `anthropic` | `POST {base_url}/v1/messages`，`{"model":...,"messages":[{"role":"user","content":"<测试发送内容>"}],"max_tokens":16}` |

1. 附加请求头：全局默认 User-Agent（`default_user_agent`，v1.8.6；空则不设置，使用 Go 默认）+ 供应商配置的自定义请求头（含 User-Agent 时覆盖全局默认）+ 按接口类型的认证头（见 2.3.6）。
2. `max_tokens` / `max_output_tokens` 固定设为 16（v1.8.4 A10，原 4096）——测试仅验证连通性（判定响应特征字段即可），无需生成内容，大幅降低单次测试成本。
3. 判定：HTTP 200 且返回合法内容（按类型检查特征字段：`choices` / `output` / `content`，且不含 `error` 字段）→ 成功；超时、401/403、其他错误 → 失败并记录错误原因。
4. 完整捕获本次请求（URL、方法、请求头、请求体）与响应（状态码、响应头、响应体），供详情查看使用。
5. **数量上限（v1.8.3）**：单次测试最多 50 条 API Key，超出返回 400「单次最多测试 50 条 API Key」——测试串行执行且每 Key 超时 120 秒，上限防止误粘贴超长 Key 列表把单次请求掂数分钟；整批测试另受 10 分钟总预算约束（v1.8.4 A2），预算耗尽时未完成的 Key 按超时失败返回（防个别长超时 Key 挂起整批）。

#### 2.5.3 测试结果详情查看

- 测试完成后，该条 API Key 记录旁出现「查看详情」图标，点击弹出详情面板。
- 面板展示内容：
  - **请求**：URL、方法、`Authorization` 头及所有自定义请求头、请求体。
  - **响应**：HTTP 状态码、所有响应头（完整列出）、响应体。
- **认证头脱敏（v1.7.6）**：回显的 `Authorization` / `X-Api-Key` 头脱敏后展示（保留前 10 + 尾 4 字符，与调用日志四组数据的上游请求头同口径），避免完整上游 Key 经 HTTP 响应明文回显扩大泄露面。
- **存储策略**：测试结果为即时动作，**不持久化、不进缓存**。页面刷新或重新打开后，上次结果不再保留。

### 2.6 后端接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/providers` | 供应商列表（含启用状态、数量统计）；**返回的 API Key 内容默认脱敏（`sk-***abc` 格式），完整 Key 不通过列表接口暴露** |
| GET | `/api/providers/:id` | 供应商详情；**返回的 API Key 内容默认脱敏（`sk-***abc` 格式），完整 Key 不通过详情接口暴露** |
| POST | `/api/providers` | 新增供应商 |
| PUT | `/api/providers/:id` | 修改供应商 |
| DELETE | `/api/providers/:id` | 删除供应商（真删除） |
| POST | `/api/providers/:id/enable` | 启用供应商 |
| POST | `/api/providers/:id/disable` | 停用供应商 |
| POST | `/api/providers/test` | 测试 API Key（**表单模式，无需先保存供应商**）；body `{"provider_id":"<可选，用于恢复脱敏Key>","base_url":"<基础地址>","api_type":"<openai/responses/anthropic，可选>","api_keys":[{"id":"...","key":"..."}],"headers":[...],"model":"<测试模型名>","key_ids":["<key_id>"]}`（key_ids 省略表示测试全部 Key；含 `***` 的脱敏 Key 按 provider_id+id 恢复，无法恢复的跳过）；按接口类型构造测试请求（见 2.5.2）；实时返回结果与请求/响应详情，不存储；**需管理员 JWT 认证**（同其他 `/api/*` 管理接口），即使代理转发端点无需登录，该测试接口也需要登录身份以区分管理操作权限 |
| GET | `/api/providers/fetch-models` | 自动获取模型（已保存供应商便捷接口；**v1.8.3 起前端页面统一改走 POST 表单模式，此接口保留兼容**）；参数通过 URL query 传递：`?provider_id=<id>`；后端使用指定 provider 的 base_url、接口类型、第一条 API Key 以及该 provider 的自定义请求头发往对应模型列表端点（见 2.3.5）；成功时返回 JSON `{"models":["gpt-4o","gpt-4o-mini",...]}`；失败时返回 4xx/5xx 并附带错误原因 |
| POST | `/api/providers/fetch-models` | 表单模式自动获取模型（**新增页 / 编辑页统一，v1.8.3**）；body 含 `provider_id`（可选，编辑页传入，用于恢复表单中的脱敏 Key）、`base_url`、`api_type`（可选）、`api_keys`（`[{"id":"...","key":"..."}]`，含 `***` 的脱敏 Key 按 provider_id + id 恢复完整值，无法恢复的跳过）、`headers`；按顺序依次尝试各 Key，以第一个成功的 Key 响应为准；单次尝试 Key 上限 50，超出返回 400（v1.8.4 M9）；整批尝试受 **10 分钟总预算**约束（v1.8.5 G5，与供应商测试 v1.8.4 A2 同口径）——预算耗尽停止尝试剩余 Key、错误消息注明总超时；客户端断开同样终止在途请求（请求全链路贯穿 ctx） |

### 2.7 数据模型

#### 2.7.1 配置数据模型（config.yml / providers 段）

```json
{
  "id": "uuid",
  "name": "OpenAI",
  "base_url": "https://api.openai.com/v1",
  "api_type": "openai",
  "enabled": true,
  "weight": 1,
  "api_keys": [
    { "id": "k1", "key": "sk-xxxx" }
  ],
  "headers": [
    { "key": "X-Custom-Header", "value": "abc" }
  ],
  "models": [
    { "name": "gpt-4o", "aliases": ["gpt4", "four-o"] }
  ],
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
```

字段说明：

- `api_type`：供应商接口类型，`openai`（Chat Completions，默认，旧配置缺省值）| `responses`（OpenAI Responses API）| `anthropic`（Anthropic Messages API）。决定上游端点、认证头与请求格式（见 2.3.6）；管理接口返回时始终为规范化后的三值之一。

- `models[].aliases`：模型别名数组。调用时将传入的模型名与别名匹配（大小写不敏感），命中后替换为真实 `name`。
- `models[].name`：同一供应商内全局唯一；若出现重复名称，后端保存时返回 400（`{"error":{"message":"同供应商内模型名称不能重复: <重复的模型名>","code":"duplicate_model_name"}}`，v1.8.4 D3：错误消息与代码实际返回对齐）。
- `api_keys[].id`：UUID 格式字符串，系统自动生成，用于批量测试等接口中引用特定 Key。
- `weight`：路由权重，默认 1。用于加权随机策略（见功能二 3.3）时按权重比例分配请求。注意：`weight` 是供应商级别的权重，与供应商内各 Key 的权重无关（供应商内 Key 选择统一使用随机策略，见 6.4 请求流程第 5 步）。
- `created_at`：供应商创建时间，ISO 8601 格式，创建后不变。
- `updated_at`：供应商最后修改时间，ISO 8601 格式，每次保存供应商时更新。
- `enabled`：供应商是否启用。停用时路由层不再分配请求至此供应商。
- 模型名称在同一供应商内必须唯一，后端保存时做唯一性约束，重复返回 400。

#### 2.7.2 代理 Key 数据模型（config.yml / apikeys 段）

```json
{
  "id": "uuid",
  "name": "客户端A",
  "key": "sk-xxxx...",
  "expires_at": "2027-01-01T00:00:00Z",
  "ip_whitelist": ["*"],
  "enabled": true,
  "models": [],
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
```

字段说明：

- `key`：代理 API Key，格式为 `sk-` + 32位字母数字随机字符串（a-zA-Z0-9）。全局唯一，不允许重复 Key 值出现（见 3.2.3 Key 唯一性约束）。新增/修改时后端校验该格式，不合规返回 400（编辑时 Key 值未变更则跳过校验，不影响存量手写配置）。
- `expires_at`：过期时间，ISO 8601 格式；为空表示永久有效；过期后该 Key 不可调用。
- `ip_whitelist`：IP 白名单数组，支持 `["*"]`（允许所有）、`["192.168.*"]`（前缀匹配）、`["192.168.1.1"]`（精确匹配），不支持 CIDR，支持多规则。
- `models`：允许调用的模型列表（取所有供应商模型名称与别名，大小写不敏感匹配）；为空表示不限制。
- `created_at`：代理 Key 创建时间，ISO 8601 格式，创建后不变。
- `updated_at`：代理 Key 最后修改时间，ISO 8601 格式，每次保存时更新。
- `quota`：预留字段结构（日/月额度及重置周期），逻辑本次不实现。

---

## 3 功能二：基础设置与代理 Key 管理

### 3.1 基础设置

| 设置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| 主机监听地址 | 文本 | `0.0.0.0` | HTTP 服务监听地址（★需重启生效） |
| 监听端口 | 数字 | `9080` | HTTP 服务监听端口（★需重启生效） |
| IP 层级 | 数字 | `0` | 反向代理层数（0 = 直连，无代理，直接以连接 IP 作为来源 IP；1 = 一层代理，取 `X-Forwarded-For` 首个 IP；2 = 两层，取倒数第二个 IP；以此类推），用于正确获取请求来源 IP；热加载 |
| 测试发送内容 | 多行文本 | `hi` | 功能一中测试供应商 API Key 时发送的消息内容；热加载 |
| 默认 User-Agent | 文本 | 空 | 发往上游请求的默认 User-Agent（v1.8.6）；空 = 不设置（使用 Go 标准库默认 `Go-http-client/1.1`，与旧行为一致）；部分上游网关（如腾讯元宝 bot）会拦截 Go 默认 UA 返回 403，此时配置为 curl / 浏览器等常见 UA；优先级：供应商自定义请求头中的 User-Agent > 此全局默认 > Go 默认；三处出站请求（真实转发 / 测试 Key / 自动获取模型）统一生效；热加载 |
| **记录调用日志** | 开关 | **关闭** | 是否记录新的调用日志（v1.8.1）；**默认关闭**，旧配置文件无 `log.enabled` 字段时同样视为关闭；关闭后新调用不再写入日志库（已有日志仍可查询/删除），仪表盘与统计不再产生新数据；热加载 |
| 日志数据库路径 | 文本 | `data.db` | 调用日志 SQLite 文件路径（相对当前工作目录）；可在设置页修改；热加载——修改后立即切换到新库，后续日志写入新库，旧库文件保留；**切换成功但配置持久化失败时自动回滚到原库并记录日志（v1.8.3，避免界面提示与实际生效不一致）** |
| 上游请求超时 | 数字 | `300` | 单次上游请求超时秒数；功能五 6.5；热加载 |

### 3.2 代理 API Key 管理

代理 API Key 是终端用户调用本代理工具的凭证。调用时需在请求头携带 `Authorization: Bearer <代理APIKey>`。

#### 3.2.1 列表页

- 表格列：名称、Key（**脱敏后显示**，如 `sk-***xyz`）、可用模型、有效期至、IP 白名单、是否启用、操作。
- **Key 脱敏规则**：默认显示前 3 位 + `***` + 后 3 位（如 `sk-***xyz`）；**短 Key ≤6 字符全遮蔽为 `***`、≤10 字符仅保留前 3 位 + `***`（v1.8.3：短 Key 前后段重叠，「前 3 + 后 3」规则反而泄露大部分字符）**；完整 Key 值不在前端页面明文展示；点击「复制」复制完整 Key（优先 Clipboard API，非 HTTPS 等非安全上下文自动降级为传统复制方式，v1.8.3）。
- 每行操作：显示/隐藏切换、复制到剪贴板、启用/停用、删除。
- 删除为真删除，执行前前端弹出二次确认。

#### 3.2.2 新增 / 修改页

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| 名称 | 文本 | 是 | Key 备注名 |
| API Key | 文本 + 随机生成按钮 | 是 | 手动输入；或点击「随机生成」得到 `sk-` + 32位字母数字随机字符串（a-zA-Z0-9） |
| 有效期至 | 日期 | 否 | 过期后该 Key 不可调用；为空表示永久有效 |
| IP 白名单 | 数组 | 否 | 默认 `["*"]`；支持 `["*"]`（允许所有）、`["192.168.*"]`（前缀匹配）、`["192.168.1.1"]`（精确匹配）；不支持 CIDR；支持多规则如 `["192.168.*","10.0.*"]` |
| 是否启用 | 开关 | 是 | 默认启用 |
| 可用模型 | 多选下拉 | 否 | 数据源为所有供应商的模型名称+别名；为空表示不限制 |

**弹窗交互**：新增/修改以弹窗形式承载；**点击遮罩层不关闭弹窗**（防止误触丢失已填内容），通过右下角「保存」右侧的「取消」按钮或右上角「×」关闭。

#### 3.2.3 调用时校验规则

请求到达时依次校验（基础认证项先于请求体解析执行），任一不满足即拒绝：

1. Key 是否存在且已启用 → 401（JSON: `{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`）。
2. 是否在有效期内（有效期至为空视为永久）→ 401（同上）。
3. 来源 IP 是否匹配白名单。IP 按「IP 层级」设置获取：层级为 0 时取直连连接 IP；层级大于 0 时从 `X-Forwarded-For` 头按层级取对应位置的 IP。不匹配 → 403（JSON: `{"error":{"message":"ip not allowed","type":"access_denied","code":"ip_not_allowed"}}`）。**空数组或缺省视为全拒**（fail-safe：手写 yml 配 `ip_whitelist: []` 将拒绝一切请求；管理界面新建/编辑 Key 时默认补 `["*"]`）。
   - **前缀规则规范化（v1.7.6）**：`x.y.*` 点分前缀匹配；缺尾点的非规范写法（如 `10*`、`10.1*`）视为无效规则忽略（fail-safe，不放行）；IPv6 地址大小写/压缩形式自动归一后比较（`FE80::1` 与 `fe80::1` 等价）。
4. 请求模型是否在可用模型范围内（若已配置可用模型列表）→ 403（同上）。
   - **匹配口径（v1.7.6 / v1.8.2 修订）**：分两档——
     (1) 客户端传入的**原始模型名**命中白名单 → 放行全部候选供应商（原始名信任）；
     (2) 否则按**各候选供应商自身命中的真实模型名**逐一过滤，仅保留真实名在白名单内的候选。多供应商共享同一别名（同一别名在不同供应商指向不同真实名）时，白名单配置非首候选的真实名不再导致合法调用被整单 403，路由只在白名单允许的候选中选择；过滤后为空 → 403。
     旧实现（v1.7.6）只按全局首候选的真实名归一匹配，共享别名场景下白名单配非首候选真实名时合法调用被误拒——v1.8.2（V-M4）修复。用户在白名单配置真实名 `gpt-4o` 而客户端用合法别名 `4o` 调用同样放行（反之亦然，单供应商场景不变）。

### 3.3 路由策略（全局）

路由策略为全局设置，与具体代理 API Key 无关。

在设置页以下拉框配置，可选：

- **随机**（默认）：在拥有目标模型的所有启用供应商中随机选一个。
- **轮询**：按顺序轮流选择拥有目标模型的供应商。
- **加权随机**：按供应商 `weight` 字段比例随机分配请求（实现为权重和内随机取点、按区间落位；v1.8.4 D6：「加权轮询」旧命名与实现不符，已正名）。

**适用场景**：当多个供应商都配置了同一个模型名称或别名时，决定调用哪一个供应商。

### 3.4 冷却池（失败熔断）

- **启用开关**：可在设置页启用/停用冷却池；停用后失败不再进入冷却池，已有条目也不再参与路由过滤。默认启用。
- **冷却时长**：全局统一设置，单位**秒**，默认 300 秒，可在设置页修改（旧配置中的 `duration_minutes` 字段自动迁移为秒，保存后不再输出旧字段）。
- **失败重试次数**：同一 `(供应商, 模型)` **连续**「所有 API Key 均失败」的轮数达到该阈值后才加入冷却池；期间任一次调用成功即清零计数。默认 1（一轮失败即冷却）。设为 N 时，单轮内失败只累计、不冷却，给供应商恢复窗口。
- **触发**：该供应商该模型**所有 API Key 均失败**且连续失败轮数达到「失败重试次数」（同供应商内 Key 重试失败不触发，见 6.4）。
- **效果**：冷却期内，路由不再将请求分配给该供应商的该模型；手动移除/清空条目时同步清零失败计数。
- **冷却池列表**：设置页显示供应商名称、模型名称、剩余冷却时长。
- **刷新频率**：前端每 **10 秒** 轮询冷却池状态并更新剩余时长显示；后端每 **30 秒** 定时清理已到期条目。
- **手动移除**：设置页提供操作按钮，支持逐条移除与批量移除全部冷却项。

### 3.5 页面组织

- **设置页**：包含基础设置、路由策略、冷却池配置、冷却池列表四个分区（可做成分区或标签页）；早期版本的限流（预留）分区已隐藏不再展示（配置结构仍预留，见功能六）。
- **代理 Key 管理页**：独立菜单，含增删改查、脱敏显示、显示/隐藏、复制等操作。

### 3.6 代理 Key 后端接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/apikeys` | 代理 Key 列表（返回脱敏 Key、名称、可用模型、有效期、启用状态） |
| GET | `/api/apikeys/:id` | 代理 Key 详情（含完整 Key，供前端「显示/复制」使用） |
| POST | `/api/apikeys` | 新增代理 Key；body `{"name":"<名称>","key":"<sk-...>","expires_at":"<有效期>","ip_whitelist":["*"],"enabled":true,"models":[]}`；`expires_at` 兼容三种格式：RFC3339（`2027-01-01T23:59:59+08:00`）、纯日期 `2027-01-01`（自动补全为当天 23:59:59 本地时区）、无时区日期时间 `2027-01-01T23:59:59` / `2027-01-01T23:59`（按服务器本地时区补全），存储统一为 RFC3339；其他格式返回 400；**Key 全局唯一，与其他 Key 重复返回 400「该 API Key 已存在（全局唯一）」**（v1.8.3：唯一性复查在并发临界区内执行，杜绝并发提交同 Key 双写） |
| PUT | `/api/apikeys/:id` | 修改代理 Key；body 含 `name`、`key`、`expires_at`、`ip_whitelist`、`enabled`、`models` 任意字段；Key 唯一性校验同新增（重复返回 400，v1.8.3） |
| DELETE | `/api/apikeys/:id` | 删除代理 Key（真删除） |
| POST | `/api/apikeys/:id/enable` | 启用代理 Key |
| POST | `/api/apikeys/:id/disable` | 停用代理 Key |
| GET | `/api/models` | 管理端可用模型列表（所有启用供应商的模型名称+别名，含别名指向）；用作代理 Key 表单「可用模型」多选数据源；需管理员登录 |

### 3.6.1 基础设置后端接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/settings` | 获取当前基础设置（监听地址/端口、IP 层级、路由策略、冷却池配置（启用/时长秒/重试次数）、上游超时秒数等）；需管理员登录 |
| PUT | `/api/settings` | 保存基础设置（body 含 listen、ip_trust_level、routing、cooldown、upstream_timeout_seconds、test_message、default_user_agent（v1.8.6）、log（`db_path` + `enabled`）等字段）；需管理员登录 |

响应示例：
```json
{
  "listen": { "host": "0.0.0.0", "port": 9080 },
  "ip_trust_level": 0,
  "routing": { "strategy": "random" },
  "cooldown": { "enabled": true, "duration_seconds": 300, "retry_count": 1 },
  "upstream_timeout_seconds": 300,
  "test_message": "hi",
  "default_user_agent": "",
  "log": { "enabled": true, "db_path": "data.db" }
}
```

### 3.7 配置结构（config.yml）

所有配置统一存放在单一 `config.yml` 文件中：

```yaml
listen:
  host: "0.0.0.0"
  port: 9080
ip_trust_level: 0
routing:
  strategy: "random"              # random | round_robin | weighted
cooldown:
  enabled: true              # 是否启用冷却池；旧配置无此字段时默认启用
  duration_seconds: 300      # 冷却池时长（秒）；旧字段 duration_minutes 自动迁移为秒，保存后不再输出
  retry_count: 1             # 连续失败多少轮后才入池；期间成功即清零计数
upstream_timeout_seconds: 300
test_message: "hi"                # 功能一测试供应商 API Key 时发送的消息内容（默认 hi）
default_user_agent: ""            # 发往上游请求的默认 User-Agent（v1.8.6）；空 = 不设置（Go 默认 Go-http-client/1.1）；供应商自定义头中的 UA 优先于此值
admin:
  username: "admin"
  password: ""                    # bcrypt 哈希；空则启动时自动生成
jwt:
  secret: ""                      # 空则启动时自动生成
  expire_hours: 24                # JWT 有效期（小时）；0/负值自动按 24 兑底（v1.8.3）；类型非法（如字符串）在配置解析阶段报错、不进入兑底（v1.8.4 D5）
log:
  enabled: false                 # 是否记录调用日志（v1.8.1）；默认关闭；旧配置无此字段时同样视为关闭
  db_path: "data.db"
providers:
  - id: "p1"
    name: "OpenAI"
    base_url: "https://api.openai.com/v1"
    enabled: true
    weight: 1
    api_keys: [ { "id": "k1", "key": "sk-xxxx" } ]
    headers: [ { "key": "X-Custom", "value": "abc" } ]
    models: [ { "name": "gpt-4o", "aliases": ["gpt4"] } ]
apikeys:
  - id: "a1"
    name: "客户端A"
    key: "sk-xxxx..."
    expires_at: "2027-01-01T00:00:00Z"   # 空=永久
    ip_whitelist: ["*"]                   # "*" | "192.168.*" | "192.168.1.1"
    enabled: true
    models: []                             # 空=不限制
```

---

## 4 功能三：调用日志

### 4.1 功能概述

记录并查看所有经本代理发生的真实模型调用，供审计与问题排查使用。注意：功能一中对供应商 API Key 的「测试」调用不属于真实业务调用，**不入此日志**。

**记录范围**：无论成功或失败（含全部供应商失败、全部冷却中等）均记录一条日志；模型未匹配的请求（模型从未被任何供应商配置过，返回 404）同样记录失败日志，状态为 `failure`，错误原因为「未匹配到对应模型」；代理 Key 模型白名单拒绝的 403 越权调用（`model_not_allowed`）同样记录失败日志（v1.8.4 P4——与 404 同为需审计的安全拒绝事件，Key 被滥用探测时可在日志页发现）。

**记录豁免**：认证类失败（401 无效/过期 Key、403 IP 白名单拒绝）与请求体解析失败（400 缺少 model/格式不合法）**不入库**——此类请求不产生上游调用，且入库会被恶意扫描刷爆日志库。模型白名单 403 自 v1.8.4 起改为入库（见上），IP 白名单 403 维持豁免（防扫描刷库，且发生在请求体解析之前无审计上下文）。

**后端接口：**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/logs` | 日志查询；query 参数：`page`（页码，默认 1，**上限 100000**，v1.8.4 S3）、`page_size`（每页条数，默认 100，**上限 500**）、`status`（可选，success/failure）、`provider`（可选，供应商名）、`model`（可选，模型名或别名，匹配调用模型或实际模型，大小写不敏感）、`apikey_id`（可选，代理 API Key UUID）、`keyword`（可选，关键字搜索，范围含 API Key、Key 名称、供应商、模型、状态、错误信息、请求体；**`%`/`_` 按字面匹配，不作为通配符**）、`start_time`（可选，起始时间 ISO 8601，含；纯日期补全为当天 00:00:00）、`end_time`（可选，结束时间 ISO 8601，含；**纯日期补全为当天 23:59:59**）；`start_time` / `end_time` 为**非法时间格式时返回 400**（v1.8.3；旧实现原样透传 SQL，查询结果静默为空）；返回分页结果，含 `total`、`page`、`page_size` 字段；列表中 `apikey` 字段返回**脱敏值**（前 3 位 + `***` + 后 3 位，短 Key ≤10 字符仅保留前 3 位（v1.8.3），与 API Key 筛选下拉一致）；需管理员登录 |
| GET | `/api/logs/:id` | 调用详情；返回指定日志完整字段（四组请求/响应数据：req_method、client_req_url、req_headers、req_body、upstream_url、upstream_req_headers、upstream_req_body、resp_status、resp_headers、resp_body、client_resp_status、client_resp_headers、client_resp_body）；需管理员登录 |
| DELETE | `/api/logs/:id` | 删除单条日志（不可恢复）；返回 `{"ok":true,"deleted":1}`；id 不存在时返回 404；需管理员登录 |
| DELETE | `/api/logs` | 清空全部日志（不可恢复）；返回 `{"ok":true,"deleted":<删除行数>}`；需管理员登录 |

响应示例：
```json
{
  "total": 1234,
  "page": 1,
  "page_size": 100,
  "logs": [
    {
      "id": "uuid",
      "timestamp": "2026-01-01T10:00:00Z",
      "apikey": "sk-***abc",
      "apikey_name": "客户端A",
      "provider": "OpenAI",
      "requested_model": "gpt-4o",
      "actual_model": "gpt-4o",
      "status": "success",
      "error_msg": null,
      "duration_ms": 1500,
      "generation_ms": 1200,
      "prompt_tokens": 100,
      "completion_tokens": 200,
      "total_tokens": 300,
      "tokens_per_second": 200.0
    }
  ]
}
```

`tokens_per_second` 为本次调用的**输出 token 速率**（completion_tokens ÷ generation_ms 秒；生成耗时或输出 token 为 0 时记 0），写入时随日志一并落库，旧库自动迁移补列。

`generation_ms` 为生成耗时，速率计算口径（v1.7.5）：**流式调用从模型供应商返回给代理的首个响应字节开始计算**（`last_chunk_ms - first_byte_ms`，即模型实际吐字窗口，不含排队与首字节延迟）；**非流式调用按整个请求的耗时**（`duration_ms`）计算；流式异常（未收到首字节）回退总耗时；模型未匹配等无上游响应场景为 0。`first_byte_ms`（上游首字节耗时）已入库，详情页可查看。旧库自动迁移补列，默认 0。

> 列表接口的 `apikey` 为脱敏显示（节省空间并降低泄露面），详情接口 `/api/logs/:id` 仍返回完整 Key。

### 4.2 列表页

表格列：

| 列 | 说明 |
|----|------|
| 调用时间 | 请求到达时间 |
| API Key | 发起调用的代理 API Key（脱敏显示：前 3 位 + *** + 后 3 位，短 Key ≤10 字符仅保留前 3 位，与筛选下拉一致） |
| Key 名称 | 代理 Key 备注名 |
| 供应商名称 | 实际调用的供应商 |
| 调用模型 | 用户请求的模型（可为别名） |
| 实际模型 | 别名解析后的真实模型名 |
| 状态 | 成功 / 失败 |
| 耗时（ms） | 本次调用总耗时（请求发往上游到收到完整响应，含排队与网络） |
| 输出速率（tok/s） | tokens_per_second = 输出 Token ÷ 生成耗时秒；为 0 时显示 - |
| 请求 Token | prompt_tokens |
| 响应 Token | completion_tokens |
| 操作 | 「查看」调用详情；「删除」删除该条日志（确认后执行，不可恢复） |

**搜索**：支持按关键词全文搜索（API Key、供应商、模型、状态等字段）。

**分页**：默认每页 100 条，支持翻页；当前页删除后无剩余数据时自动回到第一页。

**筛选**：支持按 API Key / 供应商 / 模型 / 时间范围 / 状态 过滤。其中：

- **供应商**：下拉框，选项来自当前配置中的供应商名称；
- **API Key**：下拉框，选项来自当前配置中的代理 Key（显示「名称（脱敏 Key）」）；
- 已删除的供应商 / Key 不在下拉选项中，可通过关键词搜索其历史日志。

**删除**：

- 每行操作列提供「删除」按钮，二次确认后删除单条日志；
- 筛选卡片右上角提供「删除全部日志」按钮，二次确认后清空全部日志（不受当前筛选条件影响）；v1.8.1 起清空采用**删除数据库文件并重建**的方式（见 4.1 / 4.4），确认文案会提示将删除日志数据库文件并重建以释放磁盘空间。

### 4.3 调用详情

点击列表「查看」，弹出详情弹窗（宽约 1100px）。弹窗自上而下分为**常驻区**与**标签页区**（v1.8.0）：

**常驻区**（所有标签页均可见）：

- **基本信息**：调用时间、状态、供应商、调用模型→实际模型、代理 API Key、重试次数、总耗时、上游首字节、生成耗时、Token 三元组、输出速率、错误信息（若有）。
- **尝试记录**：结构化列表逐条展示（`#序号 · 供应商 · 成功/失败 · 错误摘要`），替代原始 JSON。

**标签页区**——三个标签页，各标签页独立滚动、滚动位置独立记忆，懒渲染（首次切换才构建 DOM）：

| 标签页 | 内容 | 数据来源 |
|--------|------|----------|
| **客户端记录**（默认） | 客户端请求解析视图 + 客户端响应解析视图 | `req_method` / `client_req_url` / `req_body` / `client_resp_status` / `client_resp_body` |
| **上游记录** | 上游请求解析视图 + 上游响应解析视图 | `upstream_url` / `upstream_req_body` / `resp_status` / `resp_body` |
| **原始数据** | 四组方向数据完整原文（含请求头/响应头）+ 尝试记录原始 JSON | 全部四组字段 |

「客户端记录」与「上游记录」结构对称，每个区块含一行元信息（方法+URL 或状态码徽章、自动识别的协议格式标注、内容类型标注）与解析视图。请求头/响应头仅保留在「原始数据」标签页。

#### 4.3.1 格式识别规则

按 URL 路径优先、请求体结构兜底，四组数据各自独立识别（跨格式转发时两侧格式不同）：

| 判定依据 | 识别格式 |
|----------|----------|
| 路径含 `/chat/completions`，或 body 含 `messages` + `model` | OpenAI Chat Completions（CC） |
| 路径含 `/responses`，或 body 含 `input` | OpenAI Responses |
| 路径含 `/messages`，或 body 含 `max_tokens` + 块结构 content | Anthropic Messages |
| 无法识别 | 通用 JSON 视图（美化 + 语法高亮），标注「未识别格式」 |

#### 4.3.2 请求体解析

- **顶层参数**：kv 网格展示有值字段（`model`、`stream`、`temperature`、`top_p`、`max_tokens`/`max_completion_tokens`/`max_output_tokens`、`stop`/`stop_sequences`、`tool_choice`、`response_format`、`reasoning_effort`、`seed`、`user`、`n`、`parallel_tool_calls`、`stream_options`、`system`/`instructions` 等）。
- **消息列表**（messages / input / system）：每条消息一张卡片，role 徽章配色（system 灰紫、user 蓝、assistant 绿、tool 橙）。content 按类型解析：
  - 字符串 → 原文文本（`pre-wrap`），超 2000 字符默认折叠可展开，可复制；
  - content parts / Anthropic blocks / Responses input 条目逐段解析：`text` → 文本段；`image_url` / `source` → 图标 + URL 文本（不主动加载外链图片），base64 内嵌图片标注「内嵌图片 · N KB」；`input_audio` / file → 图标 + 格式说明 + 数据折叠；
  - `tool_calls` / `tool_use` / `function_call` → 函数名徽章 + call id 小字 + **arguments 二次解析为格式化 JSON**（arguments 为 JSON 字符串；解析失败原样显示并标注）；
  - role=tool / `tool_result` / `function_call_output` → call id 小字 + 内容文本；
  - `thinking` / `reasoning` 块 → 推理内容样式（深色斜体）。
- **tools 工具定义**：每个工具一张可折叠卡片（type、函数名、description、`parameters`/`input_schema` 格式化 JSON Schema）。
- **其他字段**：未显式处理的顶层字段原样收入折叠区（JSON 美化）。**完整性原则：解析视图 + 其他字段区合计覆盖原始 JSON 全部 key，不丢弃任何字段。**

#### 4.3.3 响应体解析

- **非流式 JSON**：CC 的 `choices` 逐项卡片（content 文本、tool_calls 格式化、reasoning_content、finish_reason 徽章）+ usage kv（含 cached_tokens / reasoning_tokens 细分，有值才显示）+ 其他字段折叠区；Anthropic 的 content blocks + stop_reason + usage；Responses 的 output 条目 + usage。
- **错误响应**（4xx/5xx）：`error.message` / `error.type` 红色提示卡 + 其余字段折叠；body 非 JSON（HTML 错误页等）→ 标注「非 JSON 内容」原样显示。
- **流式 SSE → 重组视图**：将 SSE 帧流重组为与非流式同构的最终结果展示——CC 按 `delta.content`/`delta.reasoning_content`/`delta.tool_calls`（按 index 聚合、arguments 分片拼接后二次解析）累积，`n>1` 按 choice.index 分组，取末帧 usage 与 finish_reason；Responses 按 `output_text.delta` 拼接、function_call 聚合、`response.completed` 取 usage；Anthropic 按 message_start / text_delta / input_json_delta / thinking_delta / message_delta 聚合。底部统计行（总帧数、总字节、坏帧计数；finish_reason 以徽章展示、usage 以卡片展示）；部分帧无法解析时顶部警示「N 帧无法解析，结果可能不完整」。

#### 4.3.4 原始数据标签页

四组方向数据区块 + 尝试记录原始 JSON 平移展示，三点增强：JSON 语法高亮（前端自实现，无外部依赖）；1MB 截断检测（尾部 `...(truncated)` 标记）显示醒目中文警示条「该数据超过 1MB 已截断，内容不完整」；所有代码块均提供「复制」按钮。

#### 4.3.5 降级与边界情况

| 场景 | 处理 |
|------|------|
| 1MB 截断（尾部 truncated 标记） | 区块顶部中文警示条；JSON 解析失败则该部分回退原文展示并标注「数据不完整，无法解析」 |
| body 非 JSON | 标注「非 JSON 内容」原样文本展示 |
| SSE 单帧解析失败 | 重组视图警示计数，不影响其余帧重组 |
| 某组数据为空 | 显示「(无数据)」及场景说明（如重试期间未送达客户端、模型未匹配无上游数据） |
| 旧数据（v1.7.5 前） | 沿用兼容回退（`upstream_url`←`req_url`、`client_resp_body`←`resp_body`） |
| 伪流式组合（resp_body 为 JSON、client_resp_body 为合成 SSE） | 各自按内容独立识别解析 |
| UTF-8 截断乱码 | 容忍展示 |

**超长内容交互（v1.7.5，保留）**：各请求/响应体超长时默认折叠预览（前 6 万字符），提供「显示全部」与「复制全文」按钮；后端入库上限 1MB（超出部分以 `...(truncated)` 标记）。

**性能要求**：标签页懒渲染（打开详情仅渲染默认标签）；长文本沿用「预览截断 + 显示全部」机制；1MB 级数据打开详情折叠态渲染无明显卡顿，浏览器 console 无报错。

旧版本兼容：v1.7.5 之前的日志中 `req_url` 记录上游 URL、流式 `resp_body` 为发给客户端的字节，详情页对旧数据做兼容回退展示。

### 4.4 存储与保留

- **存储引擎**：SQLite 数据库。选用纯 Go 的 SQLite 库（如 `modernc.org/sqlite`，无 CGO 依赖），以保证跨平台编译。
- **写入方式**：异步写入，不阻塞代理主响应流程。
- **记录开关（v1.8.1）**：设置页「记录调用日志」开关（配置 `log.enabled`，**默认关闭**）；
  - 关闭后：新的模型调用（成功、失败、模型未匹配）**均不再写入日志库**，仪表盘「今日调用/总调用」与统计页不再产生新数据（基于已有日志的统计不变）；
  - **已有日志不受影响**：仍可正常列表查询、查看详情、删除单条与清空全部；
  - 开关仅控制**写入**，日志数据库始终打开（供历史日志查询），关闭状态不释放库文件；
  - 热加载生效：修改 `config.yml` 或设置页保存后立即生效，无需重启；
  - 兼容：旧配置文件无 `log.enabled` 字段时同样视为关闭（含从旧版本升级的场景，升级后若需记录需手动打开）。
- **文件路径**：在设置页配置（`log.db_path`），默认当前工作目录下 `data.db`。
- **保留策略**：日志默认永久保留，不自动覆盖，不自动清理；管理员可在日志页手动删除单条或清空全部（见 4.2 删除）。
- **清空即重建（v1.8.1）**：清空全部日志时**直接删除日志数据库文件并重新生成**（关闭当前库 → 删除库文件与 `-wal` / `-shm` 伴生文件 → 原路径重建空库）。原因：SQLite 的 `DELETE` 只把页标记为空闲复用，**不会把空间归还文件系统**，长期运行后库文件只增不减；删除文件重建可即时腾出全部空间。重建完成后新到达的调用日志写入重建后的新库；重建窗口内（旧库已关闭、新库未就绪，毫秒级）到达的日志会被丢弃，不阻塞重建。文件被外部工具占用导致删除失败时，自动降级为 SQL 清空（保证数据清空，空间待文件解锁后下次清空释放）。**重建失败自愈（v1.8.3）**：重建失败（目标路径不可写、磁盘满等）时自动回开原库文件，日志子系统持续可用（旧实现后续写入静默丢弃），并在服务日志中醒目提示。**重建/切换与查询互斥（v1.8.5 G1/G2）**：清空重建与日志路径切换（设置页保存、配置热加载）均持写锁、查询/单删/仪表盘/统计在读锁内获取库引用——重建或切换期间到达的查询阻塞等待、完成后用新库正常返回（此前引用在锁外获取，高并发清空时实测可触发一波 500；热加载路径最初仍绕过锁，修复后统一经 `SwitchLogPathLocked` 持锁入口接入）；读接口另设「database is closed 时重取引用重试一次」的防御性兜底。
- **配置与数据分离**：配置项（listen、providers、apikeys、routing 等）存放于 `config.yml`；调用日志存放于独立 SQLite 文件。

**安全提示**：日志中代理 API Key 与请求/响应 Header（含 Authorization）均为明文存储，SQLite 文件应按敏感数据管理（文件系统权限控制、备份时加密等）。

### 4.5 数据模型（SQLite）

```sql
CREATE TABLE call_logs (
  id TEXT PRIMARY KEY,
  timestamp TEXT NOT NULL,
  apikey TEXT NOT NULL,
  apikey_id TEXT NOT NULL,
  apikey_name TEXT,
  retry_count INTEGER DEFAULT 0,
  attempt_logs TEXT,
  provider TEXT NOT NULL,
  requested_model TEXT NOT NULL,
  actual_model TEXT NOT NULL,
  status TEXT NOT NULL,
  error_msg TEXT,
  duration_ms INTEGER NOT NULL,
  generation_ms INTEGER DEFAULT 0,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  total_tokens INTEGER,
  tokens_per_second REAL DEFAULT 0,
  req_url TEXT,                 -- 兼容旧版：旧版记录上游 URL，新数据为空
  req_method TEXT,
  req_headers TEXT,             -- 客户端 → 代理：请求头
  req_body TEXT,                -- 客户端 → 代理：请求体（客户端原始格式）
  client_req_url TEXT,          -- 客户端 → 代理：请求路径（如 /v1/responses）
  upstream_url TEXT,            -- 代理 → 上游：请求 URL
  upstream_req_headers TEXT,    -- 代理 → 上游：请求头（认证头脱敏）
  upstream_req_body TEXT,       -- 代理 → 上游：请求体（跨格式为转换后内容）
  resp_status INTEGER,          -- 上游 → 代理：HTTP 状态码
  resp_headers TEXT,            -- 上游 → 代理：响应头
  resp_body TEXT,               -- 上游 → 代理：响应体（上游响应副本）
  client_resp_status INTEGER,   -- 代理 → 客户端：HTTP 状态码
  client_resp_headers TEXT,     -- 代理 → 客户端：响应头
  client_resp_body TEXT,        -- 代理 → 客户端：响应体（跨格式为转换后内容；流式为已发送部分）
  first_byte_ms INTEGER DEFAULT 0,    -- 上游首字节耗时（流式速率起算点）
  first_content_ms INTEGER DEFAULT 0, -- 首个 content 增量耗时
  last_chunk_ms INTEGER DEFAULT 0,    -- 末字节耗时
  sent_bytes INTEGER DEFAULT 0,       -- 实际写入客户端字节数
  last_sent_ms INTEGER DEFAULT 0      -- 最后写入客户端时刻
);

CREATE INDEX idx_logs_timestamp ON call_logs(timestamp);
CREATE INDEX idx_logs_apikey ON call_logs(apikey);
CREATE INDEX idx_logs_provider ON call_logs(provider);
CREATE INDEX idx_logs_requested_model ON call_logs(requested_model);
CREATE INDEX idx_logs_actual_model ON call_logs(actual_model);
```

字段说明：

- `status`：取值为 `success` 或 `failure`。
- `apikey`：代理 API Key 明文存储，**建议**通过后端加密（如 AES-GCM 或 ChaCha20-Poly1305）后存储，数据库只保存密文，或使用透明加密层（如 SQLite SEE）；若泄漏风险高，可对调用日志表的该字段单独加密存储（建议F）。
- 请求/响应按传输方向分四组（v1.7.5）：客户端 → 代理（`client_req_url` / `req_method` / `req_headers` / `req_body`，原始格式）；代理 → 上游（`upstream_url` / `upstream_req_headers` / `upstream_req_body`，跨格式为转换后内容，可与客户端原始请求对照定位转换问题）；上游 → 代理（`resp_status` / `resp_headers` / `resp_body`，上游响应副本）；代理 → 客户端（`client_resp_status` / `client_resp_headers` / `client_resp_body`，跨格式为转换后内容，流式为已发送部分）。`req_headers` 包含调用方传入的完整 HTTP 请求头（含 Authorization）；`upstream_req_headers` 中认证字段（Authorization / X-Api-Key）脱敏后存储（保留前 10 + 尾 4 字符）。旧库启动时自动 `ALTER TABLE` 补列。
- `attempt_logs`：JSON 数组，每条尝试含 `provider`、`api_key_id`、`error_msg`；字符串形式存储在数据库中。
- `prompt_tokens` / `completion_tokens` / `total_tokens`：以供应商返回的 `usage` 字段为准；供应商不返回 usage 时均计为 0。上游返回错误 HTTP 状态（4xx/5xx）时，无论响应体中是否包含 `usage` 字段，三个 token 字段均计为 0，不计入统计。
- `tokens_per_second`：输出 token 速率 = completion_tokens ÷ (generation_ms ÷ 1000)；生成耗时或输出 token 为 0 时记 0；仅在成功日志中计算（失败日志 token 恒为 0，速率也为 0）。旧库启动时自动 `ALTER TABLE` 补列。
- `generation_ms`：生成耗时，速率计算口径（v1.7.5）：流式 = 末字节耗时 - 上游首字节耗时（`last_chunk_ms - first_byte_ms`，从模型供应商返回给代理的首个响应字节起算，即模型实际吐字窗口）；非流式 = 总耗时（整个请求的时间）；流式异常（未收到首字节）回退总耗时；模型未匹配等无上游响应场景为 0。旧库自动补列，默认 0。
- `first_content_ms`：首个文本增量到达耗时（请求发出 → 首个文本输出类 SSE 事件到达），用于拆分 reasoning 阶段与 content 阶段的等待（reasoning 流模型如 qwen3 / o1 整个响应都在生成时仍可区分首 content 前的真实等待）。同格式透传与跨格式转换两条路径同口径识别——跨格式在转换器解析事件时识别、同格式在透传扫描器识别（v1.8.4 P5 补注）。透传扫描器按标记子串识别：OpenAI `delta.content` / Anthropic `text_delta` 事件与 `content_block_start(text)` / Responses `content_part.added`（`part.type=output_text`，真实流中先于 `output_text.delta` 到达）；裸 `"text":` 标记自 v1.8.5（G8）起弃用——Responses 的 `reasoning_summary_text.done` 事件同样含 "text" 字段，会把 reasoning 阶段误判为 content 起点；累积副本达 64MB 封顶后检测同时覆盖当前块（v1.8.5 G7，此前仅扫已封顶副本导致此后漏检）。未收到文本增量（纯工具调用 / 纯 reasoning / 上游不含该字段）或非流式调用记 0。旧库自动补列，默认 0。

---

## 5 功能四：仪表盘、管理登录与统计

### 5.1 仪表盘首页

登录后默认落地页，包含以下内容：

**概览卡片：**

| 指标 | 说明 |
|------|------|
| 供应商 | 启用数 / 总数 |
| 代理 API Key | 启用数 / 总数 |
| 今日调用次数 | 当天 00:00 起算 |
| 总调用次数 | 调用日志数据库累计值（跨服务重启持续累计） |
| 成功率 | 成功次数 / 总调用次数 |
| Token 总量 | 所有调用的 prompt_tokens、completion_tokens、total_tokens 各自累计之和 |

**后端接口：**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/dashboard` | 仪表盘数据（供应商数、代理 Key 数、今日调用、总调用、成功率、Token 总量、运行状态）；需管理员登录 |

响应示例：
```json
{
  "providers": { "enabled": 2, "total": 3 },
  "apikeys": { "enabled": 5, "total": 6 },
  "today_calls": 123,
  "total_calls": 4567,
  "success_rate": 0.98,
  "tokens": { "prompt": 1000000, "completion": 2000000, "total": 3000000 },
  "status": "running",
  "listen": "0.0.0.0:9080",
  "routing_strategy": "random",
  "cooldown_pool_size": 0,
  "upstream_timeout_seconds": 300
}
```

**运行状态区：**

- 监听地址 + 端口
- 当前路由策略
- 冷却池当前条目数

**最近调用列表：**最近 10 条调用记录（数据来源为调用日志）。

### 5.2 管理员登录

#### 5.2.1 认证范围

| 接口类型 | 鉴权方式 |
|----------|----------|
| 管理接口（供应商管理、设置、代理 Key、日志、仪表盘、统计等） | 必须登录（JWT） |
| 代理转发接口（`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`、`GET /v1/models`） | 代理 API Key（功能二），不受登录保护 |

#### 5.2.2 登录页

- 路径：`/login`，无需登录即可访问（登录页 + 所有静态资源）。
- 未登录访问其他管理接口 → 返回 401 状态码 + JSON 错误体 `{"error":{"message":"unauthorized","type":"authentication_error","code":"unauthorized"}}` → 前端跳转登录页。
- 登录后右上角提供「修改密码」与「退出登录」。

#### 5.2.3 密码存储

- `config.yml` 中 `admin.password` 存储 bcrypt 哈希，**不存储明文**。
- 首次运行、或手动将配置文件中 `admin.password` 字段清空后，系统自动生成初始密码，并将密码**打印到控制台**。
- 修改密码后，新的 bcrypt 哈希写入 `config.yml`。

#### 5.2.4 认证方式

- **JWT token**，有效期由 `jwt.expire_hours` 配置，默认 24 小时；配置为 0 / 负值时**自动按 24 兑底**（v1.8.3，含配置热加载路径）；类型非法（如字符串）在配置解析阶段报错、不进入兑底（v1.8.4 D5，旧文栏「非法值自动兑底」为过度宣称）。
- 前端每次管理接口请求携带 `Authorization: Bearer <token>`。
- 已颁发 token 校验签发时的管理员用户名与当前配置一致（v1.8.3）：运行中修改 `admin.username` 后旧 token 全部失效，需重新登录。
- token 绑定签发时的 JWT secret 摘要（v1.8.4 S15：摘要算法由 sha256 截断改为 HMAC-SHA256(secret, …)，杜绝无密钥参与的可伪造性；**升级到 v1.8.4 后升级前签发的旧 token 全部失效，需重新登录一次**）。
- 登录失败按 IP 滑窗限流（v1.8.4 M10）：同一 IP 15 分钟内累计失败 5 次后锁定，锁定期内该 IP 的登录请求直接拒绝（不做账户锁定，其他 IP 不受影响）。

#### 5.2.5 登录接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/login` | 管理员登录；body `{"username":"...","password":"..."}`；成功返回 `{"token":"<jwt>","expires_at":"<ISO8601>"}`（HTTP 200）；认证失败返回 401 `{"error":{"message":"invalid credentials","type":"authentication_error","code":"invalid_credentials"}}` |
| POST | `/api/logout` | 退出登录（可选，后端使 token 失效或前端清除 token） |
| PUT | `/api/password` | 修改当前管理员密码；body `{"old_password":"<旧密码>","new_password":"<新密码>"}`；修改成功返回 200；旧密码错误返回 400；需管理员登录 |

#### 5.2.6 配置项

```yaml
admin:
  username: "admin"
  password: ""        # bcrypt 哈希；为空时自动生成并打印到控制台
jwt:
  secret: ""          # 签名密钥；为空时自动生成并持久化
  expire_hours: 24  # JWT 有效期（小时）；0/负值自动按 24 兑底（v1.8.3）；类型非法在配置解析阶段报错（v1.8.4 D5）
```

### 5.3 统计功能

统计页与仪表盘首页为独立页面。

**数据来源：**调用日志 SQLite 数据库（`call_logs` 表）。

**统计维度与指标：**

| 维度 | 指标 |
|------|------|
| 时间序列（按天/按小时） | 调用量、成功率、平均耗时、Token 用量 |
| 按供应商 | 调用次数、Token 总量、成功率 |
| 按模型（actual_model，别名解析后的真实模型名） | 调用次数、Token 总量、成功率 |
| 按代理 API Key | 调用次数、Token 消耗量、成功率 |
| 全局汇总 | 总调用、成功/失败数、成功率、总 Token、平均耗时 |

**时间范围：**今日 / 近 7 天 / 近 30 天 / 自定义日期范围。

**图表：**本地引入图表库（如 ECharts 或 Chart.js 的本地文件），无需 CDN，支持离线使用。

**导出：**本次不做数据导出功能。

**后端接口：**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/statistics` | 统计数据；query 参数：`range`（`today`/`7d`/`30d`/`custom`），`custom_start`（YYYY-MM-DD，自定义时必填），`custom_end`（YYYY-MM-DD，自定义时必填；**起始日期晚于结束日期返回 400**，v1.8.3，前端选择反向区间时即时提示不发请求），`dimension`（可选 `provider`/`model`/`apikey`/`apikey_detail`/`timeline`/`summary`，默认返回所有维度）；`apikey_detail` 维度含各 Key 的独立成功/失败计数及平均耗时；需管理员登录。**by_apikey 维度的 `apikey` 字段自 v1.7.6 起脱敏返回**（脱敏口径与日志列表一致：前 3 位 + `***` + 后 3 位，短 Key ≤10 字符仅保留前 3 位（v1.8.3）；分组依据仍为完整 Key，仅输出脱敏） |

响应示例：
```json
{
  "range": "7d",
  "timeline": [
    { "date": "2026-01-01", "calls": 120, "success_rate": 0.98, "avg_duration_ms": 2000, "prompt_tokens": 50000, "completion_tokens": 100000, "total_tokens": 150000 }
  ],
  "by_provider": [
    { "provider": "OpenAI", "calls": 80, "tokens": 120000, "success_rate": 0.99 }
  ],
  "by_model": [
    { "model": "gpt-4o", "calls": 60, "tokens": 90000, "success_rate": 0.95 }
  ],
  "by_apikey": [
    { "apikey": "sk-xxxx...abc", "apikey_name": "客户端A", "calls": 40, "tokens": 60000, "success_rate": 0.975 }
  ],
  "by_apikey_detail": [
    { "apikey_id": "uuid-a1", "apikey_name": "客户端A", "calls": 40, "success": 39, "failure": 1, "tokens": 60000, "avg_duration_ms": 1500 }
  ],
  "summary": {
    "total_calls": 120,
    "success": 117,
    "failure": 3,
    "success_rate": 0.975,
    "total_tokens": 150000,
    "avg_duration_ms": 2000
  }
}
```

---

## 6 功能五：核心代理转发

### 6.1 功能概述

本工具的核心数据面：接收用户请求，完成身份认证、模型解析、路由选择、上游转发、响应归一化，并将调用写入日志。

### 6.2 转发端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 健康检查（建议A），无需认证，返回 `{"status":"ok","version":"<build_version>"}`，用于容器编排（K8s/Docker Compose）、负载均衡探活、外部监控等场景 |
| `POST` | `/v1/chat/completions` | 主转发端点，OpenAI Chat Completions 格式 |
| `POST` | `/v1/responses` | 转发端点，OpenAI Responses API 格式 |
| `POST` | `/v1/messages` | 转发端点，Anthropic Messages API 格式 |
| `GET` | `/v1/models` | 返回本代理可用模型列表（所有启用供应商的模型名称 + 别名，去重合并）。别名指向的模型名与别名均独立列出，有别名的模型条目附加 `alias_for` 标注其归属真实名（此字段为**非标准扩展字段**，与 OpenAI 官方 `/v1/models` 格式不完全兼容，若客户端严格校验字段则可能出现兼容性问题）。响应为 OpenAI 格式的超集：额外携带 `type`（恒为 `"model"`）与 `display_name` 字段，以兼容 Anthropic 客户端（多余字段 OpenAI 客户端会忽略）。同一模型名无论来自哪个供应商只出现一次（大小写不敏感去重）。响应格式：<br>`{"object":"list","data":[{"id":"gpt-4o","object":"model","type":"model","display_name":"gpt-4o"},{"id":"gpt4","object":"model","type":"model","display_name":"gpt4","alias_for":"gpt-4o"},...]}` |

鉴权：请求头 `Authorization: Bearer <代理APIKey>`；三个对话端点额外兼容 Anthropic SDK 风格的 `x-api-key: <代理APIKey>` 头（`/v1/models` 同样支持）。按功能二 3.2.3 规则校验。

三个对话端点的请求体均需含 `model` 字段，且请求体须符合对应端点的格式（解析失败返回 400）；请求体中的模型名经别名解析后路由，与供应商接口类型无关（自动转换，见 6.7）。

### 6.3 请求处理

1. **认证**：基础认证项（Key 解析、存在/启用、有效期、IP 白名单）先于请求体解析执行；解析 `Authorization: Bearer` 或 `x-api-key` 头（模型白名单校验在解析与别名解析之后执行，按 3.2.3 两档口径：客户端传入的原始模型名命中白名单即放行全部候选，否则按各候选供应商自身命中的真实模型名过滤候选，过滤后为空则 403）。
   - **跨域**：代理转发端点（`/v1/*`）响应包含 `Access-Control-Allow-Origin: *`，并对 OPTIONS 预检请求返回 200 + `Access-Control-Allow-Methods: GET, POST, OPTIONS`、`Access-Control-Allow-Headers: *`；管理接口（`/api/*`）不允许跨域，不返回 CORS 头，对预检返回 405。
2. **请求体解析**：按客户端调用端点的格式解析请求体（宽松解析，未知字段忽略）；解析失败或缺少 `model` 字段 → 400（错误响应格式随客户端格式，见 6.6）。
3. **模型解析**：从请求体 `model` 字段取出模型名，在所有启用供应商的 `(模型名称, 别名列表)` 中匹配：
   - 命中别名（大小写不敏感）→ 得到候选供应商集合，**每个候选携带其自身命中的真实模型名**（v1.7.6：同一别名在不同供应商可指向不同真实模型，发往上游的模型名取该供应商自己配置的名字；同一供应商内多个模型共享同一别名时只入候选一次）。
   - 命中真实名称（大小写不敏感，如 `GPT-4O` 匹配 `gpt-4o`）→ 直接得到候选供应商集合。
   - 未匹配 → 返回 404（模型从未被任何供应商配置过），并异步记录失败日志（status = `failure`，错误原因「未匹配到对应模型」，`client_resp_status` = 404、`resp_status` = 0（无上游响应），未尝试任何供应商、`provider` 为空）。
4. **冷却池过滤**：从候选供应商集合中排除处于冷却中的 `(供应商, 模型)` 组合；冷却池停用时跳过过滤（候选原样保留）。
5. **路由选择**：按全局路由策略（功能二 3.3）在剩余候选供应商中选出一个（随机 / 轮询 / 加权随机）。候选供应商可含不同接口类型，类型不影响路由。
6. **上游 Key 选择**：该供应商的多条 API Key 在选中的供应商内部按**随机策略**选取（无论全局路由策略为随机/轮询/加权，供应商内 Key 选择均使用随机）。
 7. **发往上游**：按供应商接口类型拼接端点、设置认证头（见 2.3.6）与全局默认 User-Agent（`default_user_agent`，v1.8.6；空则不设置，供应商自定义头中的 UA 优先），并按「同格式透传 / 跨格式转换」两种模式构造请求体（见 6.7）：
   - **同格式**（供应商类型 = 客户端格式）：
     - 调用方传入的模型名与真实模型名**完全一致**（含大小写）→ 请求体**字节级原样转发**。
     - 通过**别名**命中，或仅**大小写不同**（如 `GPT-4O` → `gpt-4o`）→ 仅将 `model` 字段替换为规范的真实模型名，其余字段原样转发。
     - `stream` 按客户端请求原样传递。
   - **跨格式**（供应商类型 ≠ 客户端格式）：请求体转换为供应商格式（见 6.7），`model` 使用真实模型名，`stream` 随客户端请求（客户端 `stream: true` 时上游也流式，边收边转换转发，见第 8 步）；流式时向上游附带 `stream_options.include_usage`（OpenAI 类型，请求末 chunk 携带 usage，保证 token 统计）。
8. **响应处理**：
   - **非流式**（`stream: false` 或未设置）：
     - 同格式：上游响应原样返回（状态码、响应头、响应体）。
     - 跨格式：上游响应转换为客户端格式后一次性下发，`Content-Type` 为 `application/json; charset=utf-8`。
   - **流式**（`stream: true`）：**边收边转发**——上游数据一到即写入客户端并 Flush，同时在内存累积副本（上限 64MB，超出仅转发不再累积），客户端几乎零首字延迟、与直连一致的逐字体验：
     - **同格式**：上游 SSE **原样实时透传**（字节级）。
     - **跨格式**：上游 SSE 事件流**实时转换**为目标客户端格式的 SSE 事件流（解析器 + 发射器三段式流水线，任意 3×3 格式组合可用）：Chat Completions 客户端输出 `chat.completion.chunk` 序列 + `[DONE]`；Responses 客户端输出对齐官方协议的完整事件序列（`response.created` → `response.in_progress` → `output_item.added` → `content_part.added` → `output_text.delta` → `output_text.done` → `content_part.done` → `output_item.done`（工具调用条目为 `output_item.added` → `function_call_arguments.delta` → `function_call_arguments.done` → `output_item.done`）→ `response.completed`，max_output_tokens 截断时为 `response.incomplete` 并携带 `incomplete_details.reason=max_output_tokens`）；Anthropic 客户端输出 `message_start` / `content_block_start` / `content_block_delta`（含 `tool_use` / `input_json_delta` 工具块）/ `content_block_stop` / `message_delta` / `message_stop` 事件序列。
     - **延迟提交与重试**：首个有效数据到达前不向客户端写任何字节（含状态行）；此阶段失败（网络错误 / 非 2xx / 流首事件为错误 JSON / 空流）与普通失败同样触发换 Key / 换供应商重试。一旦开始转发则不再重试，后续失败（客户端断开、上游中途断流、流中错误事件）只能断流并记 failure 日志。
      - **上游忽略 stream 参数兼容**：部分兼容网关对 `stream: true` 仍返回完整 JSON，此时按非流式响应解析并一次性合成客户端格式 SSE 返回（兜底）；Responses 客户端的合成事件序列与实时转换路径完全一致（`response.created` / `response.in_progress` 前置于全部条目事件，v1.8.5 G3——此前 reasoning 条目事件先于 created 写入违反官方序列）。
     - **usage 提取**：流式结束后从累积的上游副本中提取 usage（三格式自动归一化，含 OpenAI 末 chunk / Responses `response.completed` / Anthropic `message_start` + `message_delta`）。

   > **说明**：v1.7.0 起流式改为边收边转发（v1.6.x 及以前为有限缓冲：收完 SSE 提取 usage 后一次性转发）。跨格式流式的 usage 依赖上游流式响应携带（OpenAI 类型上游已附带 `stream_options.include_usage` 请求；个别不支持的网关可能记 0）；OpenAI/Responses 上游 → Anthropic 客户端时，因上游 usage 在流末到达而 Anthropic `message_start` 已发出，客户端侧 `input_tokens` 可能记 0（`output_tokens` 与日志统计均正确）。若供应商在流式响应中途断开（网络异常、报错等），视为失败并记 failure 日志。

   > **跨格式流式的重试语义**：上游返回 2xx 且流中携带错误事件（如 OpenAI 兼容网关的「200 + `data: {"error":...}`」延迟报错）时，若尚未向客户端写出数据则按普通失败重试；已写出则断流记日志。同格式为字节级透传语义，上游错误流原样透给客户端（与直连行为一致），不做拦截。
9. **日志写入**：异步写入调用日志（功能三）。四组请求/响应数据完整记录（v1.7.5）：客户端请求（client_req_url / req_headers / req_body，原始格式）、代理→上游请求（upstream_url / upstream_req_headers / upstream_req_body，跨格式为转换后内容，认证头脱敏）、上游响应（resp_status / resp_headers / resp_body，上游副本）、代理→客户端响应（client_resp_status / client_resp_headers / client_resp_body，跨格式为转换后内容，流式为已发送部分）。`generation_ms`：流式 = 上游首字节 → 末字节（模型实际吐字窗口），非流式 = 总耗时；`tokens_per_second` = 输出 Token ÷ 生成耗时秒。

### 6.4 失败重试与跨供应商降级

**重试策略：**

1. 上游请求失败（超时、401、403、429、5xx）→ 在同供应商内切换到下一条可用 Key 重试（最多重试 2 次，即 1 次原始请求 + 最多 2 次重试，共最多 3 次上游请求；供应商 Key 数 > 3 时随机不重复抽取 3 条尝试）。
2. 同供应商重试槽位内 Key 均失败（Key 数 > 3 时为槽位内 3 条，v1.8.4 M3 口径）→ 按路由策略选择其他拥有该模型的供应商（排除冷却中）尝试。
3. 所有供应商均失败 → 返回 503 归一化错误。

**冷却池更新：**

- 冷却粒度为 `(供应商, 模型)` 级别，同供应商内 Key 重试**不触发冷却池更新**。
- 只有当该供应商该模型的**重试槽位内 Key 均失败**（Key 数 ≤ 3 时即全部 Key；> 3 时为随机不重复尝试的 3 条，v1.8.4 M3——旧实现按「全部 Key 均失败」判定，Key > 3 时该条件恒不成立、供应商永不入池）且**连续失败轮数达到设置页「失败重试次数」**后，才将该 `(供应商, 模型)` 组合加入冷却池；期间任一次调用成功即清零连续失败计数。重试次数为 1 时与历版行为一致（一轮失败即冷却）。
- 跨供应商降级时，若新供应商该模型同样所有 Key 失败，同样计入其连续失败计数（达到阈值后入池）。
- 冷却池停用时：失败不再计入/入池，已有冷却条目不再参与路由过滤。
- 失败日志写入调用日志（status = `failure`，含 error_msg）。

**调用日志写入语义**：无论重试多少次，最终只写 **1 条**调用日志记录，标记最终状态（`success`/`failure`）。日志包含以下额外字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `retry_count` | integer | 重试次数（0 表示一次成功，1 表示重试 1 次，依此类推） |
| `attempt_logs` | JSON 数组 | 每次请求（原始 + 重试）的详细尝试记录，每条含 `provider`、`api_key_id`、`error_msg` |

> **说明**：若某次重试中某供应商/Key 失败但后续重试成功，最终日志 status 为 `success`，`retry_count` 记录总重试次数，`attempt_logs` 记录所有尝试（含失败记录）。

### 6.5 超时与并发

- **上游请求超时**：默认 300 秒，可配置。
- **最大并发**：不限制。

### 6.6 错误归一化

错误响应格式**随客户端调用的端点格式**：

- `/v1/chat/completions`、`/v1/responses` 客户端 → OpenAI 风格 JSON：

```json
{ "error": { "message": "...", "type": "...", "code": "..." } }
```

- `/v1/messages` 客户端 → Anthropic 风格 JSON：

```json
{ "type": "error", "error": { "type": "...", "message": "..." } }
```

Anthropic 错误 `type` 按 HTTP 状态码映射：400 → `invalid_request_error`，401 → `authentication_error`，403 → `permission_error`，404 → `not_found_error`，413 → `request_too_large`，429 → `rate_limit_error`，529 → `overloaded_error`，其他 → `api_error`。

状态码映射规则（对所有端点一致）：

| 场景 | 返回状态码 |
|------|-----------|
| 代理 Key 无效 / 已停用 / 过期 | 401 |
| IP 不在白名单 / 模型不在可用列表 | 403 |
| 请求体不符合端点格式 / 缺少 model 字段 | 400 |
| 模型在任何启用供应商中均未配置（从未被添加过） | 404 |
| 模型已配置但当前全部不可用（全部处于冷却中或全 Key 失败） | 503 |
| 上游返回 400 Bad Request | 502 |
| 上游返回的错误（401/403/429/5xx 等） | 触发 6.4 重试；穷尽所有供应商与 Key 后仍失败，返回 503（见上） |
| 跨格式时上游响应解析失败 | 视为该次尝试失败，参与 6.4 重试与降级；穷尽后返回 503 |

### 6.7 多格式转换矩阵（新增，v1.2）

代理在「客户端调用格式 × 供应商接口类型」之间自动转换，共 3 × 3 = 9 种组合：

| 客户端格式 ↓ \ 供应商类型 → | openai（Chat Completions） | responses（Responses API） | anthropic（Anthropic API） |
|---|---|---|---|
| `/v1/chat/completions` | **透传**（无转换） | 转换：CC → Responses → CC | 转换：CC → Anthropic → CC |
| `/v1/responses` | 转换：Responses → CC → Responses | **透传**（无转换） | 转换：Responses → Anthropic → Responses |
| `/v1/messages` | 转换：Anthropic → CC → Anthropic | 转换：Anthropic → Responses → Anthropic | **透传**（无转换） |

**实现方式**：以内部中间表示（IR）为中心，每种格式实现「请求解析 / 请求构建 / 响应解析 / 响应构建」各一对，任意组合经 IR 中转完成（`internal/convert` 包）。

**请求转换映射规则**：

| 语义 | Chat Completions | Responses API | Anthropic API |
|------|------------------|---------------|---------------|
| 系统提示词 | `messages[]` 中 `system` / `developer` 消息 | `instructions` 字段（或 input 中 system 条目） | 顶层 `system` 字段 |
| 用户/助手消息 | `messages[]` | `input[]` 条目 | `messages[]` |
| 图片输入 | `image_url`（http 链接或 `data:` 内联） | `input_image.image_url` | `image.source`（`base64` / `url`） |
| 最大输出 Token | `max_tokens` / `max_completion_tokens` | `max_output_tokens` | `max_tokens`（**必填**，缺失时默认 4096） |
| 停止序列 | `stop` | 无对应参数（**丢弃**） | `stop_sequences` |
| 工具定义 | `tools[].function.{name,description,parameters}` | `tools[].{name,description,parameters}` | `tools[].{name,description,input_schema}` |
| 工具选择 | `auto` / `none` / `required` / `{type:function}` | 同左（指定工具为 `{type:function,name}`） | `{type:auto}` / `{type:none}` / `{type:any}` / `{type:tool,name}` |
| JSON 模式 | `response_format: {type:json_object}` | `text.format: {type:json_object}` | 无对应参数（**丢弃**） |
| 推理力度 | `reasoning_effort` | `reasoning.effort` | 无对应参数（**丢弃**） |
| 工具调用（助手） | `tool_calls[]` | `function_call` 条目 | `tool_use` 内容块 |
| 工具结果 | `role:tool` 消息 | `function_call_output` 条目 | `tool_result` 内容块（并入 user 消息） |

**响应转换映射规则**：

| 语义 | Chat Completions | Responses API | Anthropic API |
|------|------------------|---------------|---------------|
| 正文文本 | `choices[0].message.content` | `output[].content[].text`（output_text） | `content[].text`（text 块） |
| 思考内容 | `choices[].message.reasoning_content`（流式 `delta.reasoning_content`；v1.8.4） | `output[]` 中 `reasoning` 条目（reasoning_text；条目仅有 `summary` 数组时兜底读取 summary_text，v1.8.5 G4；流式 `response.reasoning_text.delta`；v1.8.4） | `content[]` 中 `thinking` 块（流式 `thinking_delta`；signature 不跨格式重建、构建时空置；v1.8.4） |
| 工具调用 | `choices[0].message.tool_calls[]` | `output[]` 中 `function_call` 条目 | `content[]` 中 `tool_use` 块 |
| 结束原因 | `finish_reason`（stop/length/tool_calls） | `status`+`incomplete_details`（→ tool_calls/length/stop） | `stop_reason`（end_turn/max_tokens/tool_use） |
| Token 用量 | `usage.{prompt_tokens,completion_tokens,total_tokens}` | `usage.{input_tokens,output_tokens,total_tokens}` | `usage.{input_tokens,output_tokens}`（total = 两者之和） |

**转换限制与约定**：

- 转换为 Anthropic 时相邻同角色消息自动合并（工具结果转为带 `tool_result` 块的 user 消息），保证角色交替结构。
- Responses 的内置工具条目（web_search 等）不参与跨格式转换。思考内容（CC `reasoning_content` / Anthropic `thinking` 块 / Responses `reasoning` 条目）自 v1.8.4（M6）起在**响应侧**跨格式互转（见上表）；**请求侧**客户端回传的历史思考内容（thinking 块 / reasoning 条目）被解析器忽略——signature 等校验字段跨格式不可重建，回传无效签名会被上游拒绝。
- 无对应参数的字段（如 Responses 的 stop 序列、Anthropic 的 JSON 模式）静默丢弃，不报错。
- 同格式透传路径**不经转换器**，保持字节级语义（见 6.3 第 7 步），零转换损耗。
- 日志中的 `requested_model` / `actual_model` / token 统计对全部 9 种组合一致：请求模型按客户端传入值记录，实际模型按别名解析后的真实名记录，token 用量从上游响应提取并按格式归一化。

---

## 7 功能六：访问控制与配额

当前版本暂不实现额度限制与限流功能。

- `apikeys` 数据模型中预留 `quota` 字段结构（token 额度 / 请求次数额度，支持日/月/总量限制及重置周期配置），位置预留但逻辑不实现。
- 设置页早期版本的限流（Rate Limit）预留分区已**隐藏不再展示**（v1.6 起）；配置结构仍预留，后续版本启用时再恢复展示。

---

## 8 功能七：供应商管理增强（自动获取模型 + 模型名称唯一性）

### 8.1 自动获取模型

见 2.3.5。

### 8.2 模型名称唯一性

同供应商内模型名称不允许重复。

- **后端约束**：保存供应商时对 `models[].name` 做唯一性校验，重复时返回 400 错误 JSON（`{"error":{"message":"同供应商内模型名称不能重复: <重复的模型名>","code":"duplicate_model_name"}}`，v1.8.4 D3：错误消息与代码实际返回对齐）。
- **前端交互**：新增/修改模型时，实时检测当前供应商内是否有同名模型（大小写不敏感）；发现重复时即时提示「该模型名称已存在」，不允许提交。
- **生效时机**：仅在当前供应商范围内校验，跨供应商不受此约束（不同供应商可配置相同模型名）。

---

## 9 功能八：运行、部署与配置管理（含前端资源嵌入）

本功能涵盖配置热加载、优雅关闭、启动参数、配置文件自动生成及前端资源嵌入。

### 9.1 配置热加载

- 修改 `config.yml` 后，系统自动检测文件变更（约 2 秒轮询）并重新加载配置（免重启）。
- 以下配置项支持热加载：供应商信息、代理 Key、路由策略、冷却池配置（启用开关/时长秒数/失败重试次数）、上游超时秒数、IP 层级、测试发送内容、默认 User-Agent（v1.8.6）、日志记录开关（`log.enabled`，v1.8.1 起）、日志数据库路径（切换后新日志写入新库）。
- **加载失败处理（v1.8.3）**：文件变更但解析失败（如 YAML 语法错误）或自动生成凭证失败时，跳过本次变更并**在服务日志中记录原因**，系统继续按原配置运行（此前为静默跳过、无提示）。
- 特殊行为：运行中将 `admin.password` 清空 → 自动生成新密码并打印到控制台（同 5.2.3）。
- **例外（需重启）**：监听地址与端口变更、`jwt.secret` 变更——这两项修改后需重启服务才能生效；其中 `jwt.secret` 变更会使所有已颁发的 JWT token 立即失效，用户需重新登录。

### 9.2 优雅关闭

- 收到停止信号（SIGTERM / SIGINT）后，立即停止接收新请求。
- 等待所有在途请求处理完成（含流式响应完成），或达到超时时间后退出。
- 退出前确保所有在途日志写入 SQLite 数据库。

### 9.3 启动参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-config <path>` | 指定配置文件路径 | 当前目录 `config.yml` |
| `-port <port>` | 覆盖监听端口（**仅运行时生效，不写回 config.yml**，v1.7.6：避免临时参数经后续任意保存被意外持久化） | `config.yml` 中配置值 |
| `-host <addr>` | 覆盖监听地址（同上，不落盘） | `config.yml` 中配置值 |

> 说明：覆盖期间管理界面显示实际生效值；若用户在界面显式修改并保存监听配置，新值正常落盘，下次不带参数启动时生效。

### 9.4 配置文件自动生成

程序首次启动时，若指定路径（默认当前目录）下不存在 `config.yml`，自动在启动目录下生成一份包含所有默认值的配置文件。

生成内容包括：监听地址 `0.0.0.0`、端口 `9080`、默认路由策略 `random`、默认冷却时长 5 分钟、测试发送内容默认 `hi`、日志路径默认 `data.db`、默认管理员用户名 `admin`（密码自动生成后打印至控制台）、JWT 签名密钥（自动生成并写入 `jwt.secret` 字段）、空供应商列表、空代理 Key 列表。

### 9.5 前端资源嵌入

管理界面（HTML、CSS、JavaScript 等静态资源）以嵌入方式编译进可执行文件，无需以独立文件形式分发。

实现方式：使用 Go 的 `embed` 指令或第三方打包工具（如 `statik`、`esc`）将前端构建产物（HTML 文件、图片、图表库本地文件等）嵌入二进制，程序启动时从内存直接提供静态文件服务。

分发方式：用户仅需获取一个可执行文件即可运行整个服务，无需额外部署前端资源目录。

### 9.6 时区约束（v1.8.4 S14）

统计「今日」边界、日志时间参数解析、`expires_at` 无时区输入的补全均按**服务器本地时区**计算，因此要求运行环境时区恒定：容器化部署需显式固定时区（如 Docker 设置 `TZ=Asia/Shanghai` 环境变量并确认镜像含 tzdata）；跨机器迁移部署需保持时区一致。若时区变更，「今日」统计边界与日志筛选区间将随环境漂移（历史数据本身存储为绝对时间戳，不受影响）。

---

## 10 系统配置完整结构

以下为 `config.yml` 的完整结构示例，涵盖所有功能模块的配置项。

```yaml
# ===== 基础监听 =====
listen:
  host: "0.0.0.0"
  port: 9080
ip_trust_level: 0               # 反向代理层数（0=直连）；>0 时从 X-Forwarded-For 取对应位置 IP

# ===== 管理员认证 =====
admin:
  username: "admin"
  password: ""                    # bcrypt 哈希；启动时为空则自动生成并打印

jwt:
  secret: ""                      # 签名密钥；启动时为空则自动生成；修改后需重启并使所有已颁发 token 失效
  expire_hours: 24                # JWT 有效期（小时）；0/负值自动按 24 兑底（v1.8.3）；类型非法（如字符串）在配置解析阶段报错、不进入兑底（v1.8.4 D5）

# ===== 日志 =====
log:
  enabled: false               # 是否记录调用日志（v1.8.1）；默认关闭；旧配置无此字段时同样视为关闭
  db_path: "data.db"              # SQLite 调用日志文件路径

# ===== 路由与熔断 =====
routing:
  strategy: "random"              # random | round_robin | weighted（全局）

cooldown:
  enabled: true              # 是否启用冷却池（旧配置无此字段时默认启用）
  duration_seconds: 300      # 冷却池时长（秒）；旧字段 duration_minutes 自动迁移，保存后不再输出
  retry_count: 1             # 连续失败多少轮后才入池；期间任一次成功即清零计数

upstream_timeout_seconds: 300   # 上游请求超时（秒），默认 300
test_message: "hi"                # 功能一测试供应商 API Key 时发送的消息内容（默认 hi）
default_user_agent: ""            # 发往上游请求的默认 User-Agent（v1.8.6）；空 = 不设置（Go 默认）；供应商自定义头中的 UA 优先于此值

# ===== 供应商列表（功能一）=====
providers:
  - id: "uuid-1"
    name: "OpenAI"
    base_url: "https://api.openai.com/v1"
    api_type: "openai"              # openai(默认) | responses | anthropic（见 2.3.6）
    enabled: true
    weight: 1
    api_keys:
      - id: "k1"
        key: "sk-xxxx"
    headers:
      - key: "X-Custom-Header"
        value: "abc"
    models:
      - name: "gpt-4o"
        aliases: ["gpt4", "four-o"]
    created_at: "2026-01-01T00:00:00Z"
    updated_at: "2026-01-01T00:00:00Z"
  - id: "uuid-2"
    name: "Anthropic"
    base_url: "https://api.anthropic.com"   # anthropic 类型：末尾不带 /v1，端点自动拼 /v1/messages
    api_type: "anthropic"
    enabled: true
    weight: 1
    api_keys:
      - id: "k2"
        key: "sk-ant-xxxx"
    headers: []
    models:
      - name: "claude-sonnet-4-5"

# ===== 代理 Key 列表（功能二）=====
apikeys:
  - id: "a1"
    name: "客户端A"
    key: "sk-xxxx..."             # sk- 前缀 + 随机字符串
    expires_at: "2027-01-01T00:00:00Z"   # 空=永久
    ip_whitelist:                 # ["*"] | ["192.168.*"] | ["192.168.1.1"]
      - "*"
    enabled: true
    models: []                    # 空=不限制；非空=仅允许以下模型
    # quota: {}                   # 预留：额度结构（本次不实现）
```

---

## 11 部署说明

### 11.1 部署形态

程序为单一可执行文件，前端资源已嵌入二进制，无需额外部署静态文件目录。

首次运行自动生成 `config.yml`，之后程序与配置文件同目录运行。

### 11.1.1 构建脚本

项目根目录提供跨平台构建脚本：

- `build.bat`（Windows）/ `build.sh`（Linux / macOS）
- 不带参数：编译当前系统，产物输出到项目根目录（`llmproxy.exe` / `llmproxy`）。
- 带参数交叉编译：`build.bat <os> [arch]`，产物输出到 `dist/` 并带平台后缀。
- 支持目标：windows（386/amd64/arm64）、linux（386/amd64/arm/arm64）、darwin（amd64/arm64）。
- 交叉编译固定 `CGO_ENABLED=0`，无外部依赖；`GOPROXY` 未设置时默认国内镜像。
- 可选环境变量 `VERSION=<x.y.z>` 注入版本号（默认使用源码中的 buildVersion）。

### 11.2 供应商 API Key vs 代理 API Key 对比

| 维度 | 供应商 API Key（功能一） | 代理 API Key（功能二） |
|------|------------------------|----------------------|
| 用途 | 本代理调用上游大模型供应商 | 终端用户调用本代理 |
| 存放在 | `config.yml` → `providers[].api_keys` | `config.yml` → `apikeys` |
| 调用方式 | 在发往供应商的请求中附加（按接口类型：`Authorization: Bearer` 或 `x-api-key`） | 在发往本代理的请求头中携带 |
| 鉴权方式 | 无需鉴权（内部使用） | `Authorization: Bearer <key>`（对话端点兼容 `x-api-key`） |
| 管理界面 | 供应商管理页 | 代理 Key 管理页 |

### 11.3 前端路由（建议）

| 路径 | 页面 | 说明 |
|------|------|------|
| `/login` | 登录页 | 无需认证即可访问 |
| `/` | 仪表盘首页 | 需管理员登录 |
| `/dashboard` | 仪表盘首页（同上） | 需管理员登录 |
| `/providers` | 供应商列表 | 需管理员登录 |
| `/providers/new` | 新增供应商 | 需管理员登录 |
| `/providers/:id/edit` | 修改供应商 | 需管理员登录 |
| `/settings` | 基础设置 / 路由策略 / 冷却池 | 需管理员登录 |
| `/apikeys` | 代理 Key 管理 | 需管理员登录 |
| `/logs` | 调用日志列表 | 需管理员登录 |
| `/statistics` | 统计页面 | 需管理员登录 |
| `/v1/chat/completions` | 代理转发端点（OpenAI Chat Completions 格式） | 需代理 API Key |
| `/v1/responses` | 代理转发端点（OpenAI Responses API 格式） | 需代理 API Key |
| `/v1/messages` | 代理转发端点（Anthropic Messages API 格式） | 需代理 API Key |
| `/v1/models` | 可用模型列表端点 | 需代理 API Key |

> AI生成

---

## 12 版本更新记录

### v1.8.6（2026-10-08）：全局默认 User-Agent（default_user_agent）

- **背景与实测复现**：用户为腾讯元宝 bot 接口（`bot.yuanbao.tencent.com`）配置供应商（含必需的 `X-Forwarded-For` 自定义头），「测试 Key」返回 403，curl 同参数成功。逐项对照复现（Go 程序模拟 testOneKey 真实行为，同一请求体七组对照）：带 XFF + Go 默认 UA（`Go-http-client/1.1`）→ **403「错误请求」**；不带 XFF → 401「来源IP无效」（证明 **XFF 已被正确传递**，403 卡在更靠前的 UA 层）；带 XFF + curl UA / 自标识 UA（`llmproxy/1.8.5`）→ **200**。结论：该网关按 User-Agent 黑名单拦截 Go 标准库默认 UA，属黑名单机制（非白名单）；且不止影响测试 Key——真实转发（`newUpstreamRequest`）同样不设 UA、同样会 403。
- **新增全局设置项 `default_user_agent`**（3.1 基础设置 / config.yml / 设置页输入框）：发往上游请求的默认 User-Agent；**空（默认）= 不设置任何 UA 头，保持 Go 标准库默认，与旧行为完全一致**（升级无行为变化）；热加载。
- **优先级三层**：供应商自定义请求头中的 User-Agent > 全局默认 > Go 默认（不设置）。实现为公共函数 `convert.SetDefaultUserAgent`（空值不写头），在供应商自定义头之前应用、后者经 `Header.Set` 覆盖。
- **三处出站请求统一接入**（同一口径）：真实转发（proxy `newUpstreamRequest`，流式 / 非流式两路径共用）、测试 Key（api `testOneKey`）、自动获取模型（api `fetchUpstreamModels`，GET 按 ID / POST 表单两入口共用）。测试详情弹窗的请求头回显同步可见生效的 UA。
- **管理接口**：`GET /api/settings` 返回 `default_user_agent`；`PUT /api/settings` 接受该字段（TrimSpace 后纯空白视为空，可随时清空回退 Go 默认）。
- **使用建议**：遇上游 403 且排除 Key / IP 因素时，可将全局默认设为 curl / 浏览器等常见 UA；仅个别供应商有此要求时，也可只在供应商自定义请求头里加 `User-Agent`，不影响其他供应商。
- **回归测试**：配套新增 `audit5_regression_test.go`（convert 1 用例：SetDefaultUserAgent 三语义；proxy 1 用例：newUpstreamRequest 五场景优先级矩阵；api 3 用例：settings 写入-回读-yml 持久化-清空回退全链路、测试 Key 经 mock 上游三层优先级（含未配置时服务端实收 `Go-http-client/1.1` 的完整链路验证）、fetch-models 携带全局默认 UA）；既有全部回归用例无回归；全量测试通过后重新编译。

### v1.8.5（2026-10-06）：第四轮全面检查修复（实测复现 4 项 + 代码取证 4 项 + 文档 2 项；逐项先实测/取证核实、后修复，0 误报全实修）

- **日志子系统竞态收口（G1/G2，关联 v1.8.4 A5 补全）**：G1 五个日志读接口（列表 / 详情 / 单删 / 仪表盘 / 统计）的 store 引用实际在 logOpMu 读锁**外**获取——v1.8.4 A5 注释宣称「阻塞等待重建完成后用新 store 正常返回」从未成立，阻塞在锁上的查询醒来后拿到的仍是已关旧库，实测并发清空 + 查询 15 秒捕获 **3943 次** 500；现引用全部移入读锁内获取，并增加「database is closed 错误时重取引用重试一次」兜底（判定引用未变则不重试；DELETE 幂等可安全重试，statistics 各维度查询纯读幂等、整体重跑无副作用）。G2 日志路径切换与并发查询零互斥，实测 15 秒捕获 **2171 次** 500——分两处收口：设置页路径切换（OnLogPathChange 回调）与 v1.8.3 M3 回滚路径均持 logOpMu 写锁；配置热加载路径（Watch 后台 goroutine）旧实现直接调回调绕过锁，audit4 复跑时背靠背切换仍捕获 176 次 500（G1 的重试一次在高频切换窗口内不足），新增 `SwitchLogPathLocked` 持锁入口统一接入（锁序 logOpMu → main 层 logSw.mu，与清空路径一致，无死锁）——三条切换路径全部与查询互斥后，G1 重试退化为防御性兜底。
- **跨格式转换正确性（G3/G4）**：G3 兜底合成路径（上游忽略 stream 参数返回完整 JSON、经 fallbackFullBodySSE 一次性合成）Responses SSE 事件顺序违反官方协议——reasoning 条目事件先于 response.created 写入（实测首事件为 output_item.added）；现 created / in_progress 前置于全部条目事件（与流式路径 responsesEmitter 对齐）。G4 Responses 非流式响应 reasoning 条目只解析 content 通道——官方常见形态仅有 summary 数组（content 为显式开启输出模式后的新增通道），此时思考内容跨格式静默丢失；现 content 优先、**按条目独立**兜底读取 summary（多 reasoning 条目混用两通道时各自独立判定）。
- **fetch-models 可用性（G5）**：表单模式自动获取模型串行尝试 50 Key × 60s 无总预算（最坏挂起约 50 分钟，v1.8.4 M9 只加了数量上限），且 fetchUpstreamModels 构建请求不带 ctx（客户端断开无法终止在途请求）；现 fetchUpstreamModels 贯穿 ctx（NewRequestWithContext）、表单模式以 r.Context() 派生 **10 分钟总预算**（与供应商测试 v1.8.4 A2 同口径），预算耗尽停止尝试剩余 Key 并在错误消息注明；按 ID 获取接口随请求 ctx 取消。
- **流式统计口径（G6/G7/G8）**：G6 透传 / 转换两条流式路径写客户端失败时，部分写入字节（`w.Write` 返回 wn）计入 sentBytes 但 clientBytes 截断到上一块 / 整块丢弃——`client_resp_body < sent_bytes` 两口径矛盾，排查「客户端断开时实际收到多少」时误导；现部分写入字节同样计入 clientBytes（拼接先复制，不污染上游副本 RespBody）。G7 透传流累积副本达 64MB 封顶后不再增长，content 检测仅扫已封顶的副本导致此后永远漏检（`first_content_ms` 恒 -1）；现检测同时覆盖当前块（`streamChunkHasContent(data) || streamChunkHasContent(chunk[:n])`）。G8 contentMarkers 含裸 `"text":`——Responses 上游 `reasoning_summary_text.done` 事件（思考摘要完成帧）JSON 同样含 "text" 字段，reasoning 阶段被误判为 content 起点、`first_content_ms` 偏早；现改用 `"text_delta"` + `"type":"text"`（Anthropic 文本增量事件名 / 块类型声明），已验证 thinking / reasoning_summary_text.done / reasoning_text.delta 均不误命中，text_delta / content_block_start(text) 正常命中。
- **文档修订（G9/G10）**：G9 README 构建说明版本号写死「1.8.3」（源码已 1.8.4）改为「源码 buildVersion」口径（与 v1.8.4 D2 同精神，不再随版本升级漏更新）；G10 2.5.2 测试请求体表格 Responses 行 `max_output_tokens:4096` 与同节第 2 条（v1.8.4 A10：16）及代码实现矛盾，改为 16。
- **复核与误报排除**：10 项候选逐项实测复现（A 类 4 项经 `.temp/audit4` 复核脚本 T1-T4：3943 / 2171 次 500、SSE 首事件错序、summary 丢失）或代码/文本逐行取证（B/C/D 类），全部属实、0 误报。
- **回归测试**：本轮配套新增 `audit4_regression_test.go`（api 5 用例：并发清空+查询 0×500、并发路径切换+查询 0×500、热加载路径（SwitchLogPathLocked）并发切换+查询 0×500、fetchUpstreamModels ctx 取消即返、表单模式 ctx 取消即停且注明总超时；convert 3 用例：兜底合成首事件 response.created 与完整顺序、无 reasoning 对照组、summary 通道三场景（仅 summary / content 优先 / 多条目独立兜底）；proxy 4 用例：contentMarkers 判定矩阵（10 场景）、64MB 封顶后 content 检测、透传/转换两路径部分写入口径）；G5 的 10 分钟预算为时间类行为，单测无法压缩等待，其 ctx 传递与停止判定链路由两个取消用例覆盖；既有全部回归用例无回归；全量测试通过后重新编译。

### v1.8.4（2026-10-06）：第三轮全量复查修复（高危 1 项 + 中危 10 项 + 低危多项 + 前端 8 项 + 文档修订；逐项先实测复现核实、后修复）

- **跨格式转换正确性（H1/M4/M5/M6/C5/C6/C7/C8）**：H1（高危）Anthropic 客户端图文混排 user 消息（content 数组含 text + image_url 块）跨格式转发时图片块被整体丢弃、仅保留图片前文本——现三格式 parts 归一化正确展开图片块，跨格式图文请求不再丢图；M4 SSE 事件结束判定按「位置先后」而非「分隔符类型优先级」切割（两种分隔符混用时不再跨帧粘连）；M5 跨格式流式无参工具调用 arguments 补 `"{}"`（严格上游不再 400）；M6 thinking / reasoning 跨格式通道完整打通——CC `reasoning_content`、Anthropic `thinking` 块、Responses `reasoning` 条目三格式双向转换（请求 / 响应 / 流式三路径全覆盖，此前跨格式思考内容丢失）；C5 流重组时间字段取首帧固化；C6 转换器 Finish 残余帧错误上抛（不再静默吞错记成功）；C7 空文本片段过滤、全空时回退纯文本通道（不再产出空 content 数组）；C8 同 index 重复 tool_start 幂等跳过（兼容网关重发防御）。
- **代理转发健壮性（M1/M2/M3/P4/P6/C1）**：M1 客户端断开标记改原子操作（并发读写竞态）；M2 非流式客户端断开后终止重试（不再换 Key/换供应商白烧配额），记「客户端断开」；M3 重试终止条件修正为 `attempted == maxAttempts`——供应商 Key 数 > 3 时旧实现烧光全部 Key 才停、冷却不生效，现 3 次失败即达冷却条件（对齐「最多 3 次上游请求」口径）；P4 403 越权 / 模型未匹配等无上游场景写调用日志（调用审计完整）；P6 透传流写客户端失败时已发送字节截断到实际送达部分；C1 行为修正——v1.8.2 的「continue 改 break」实际跳出供应商循环直接 503（其余可用供应商不再尝试），与注释宣称「进入下一家供应商」不符，修正回 continue（不烧同供应商 Key 重试槽位的原意图保持）。
- **存储与配置（M7/S3-S15）**：M7 管理界面保存与配置热加载 write-skew——Watch 期间界面保存被旧文件内容覆盖，现保存前吸收盘上外部变更（`AbsorbExternalChange`），界面保存永远基于最新内容；S3 分页 Page 参数 clamp 至 100000；S4 删除重建先清 `-shm`/`-wal` 伴生文件（残留伴生文件致重建库读到旧数据）；S5 迁移前 PRAGMA 预查已存在列；S6 Close 超时前排空写入队列（优雅关闭最后日志不靠竞速）；S7 写失败计数线程安全、库关闭后丢弃计数、清空失败上抛；S8 randomString 混入时间熵；S9 Watch 读取失败回滚 mtime（不漏下一次变更）；S10 claims 恒定时间比较；S11 非 IP 串不命中前缀白名单（`10.1.bypass` 类串不再误放行）；S12 配置落盘 fsync；S13 弱 JWT secret 启动告警；S15 shortHash 改 HMAC-SHA256(secret,…)——**升级注意：摘要算法变更使升级前已签发的管理 token 全部失效，需重新登录一次**。
- **管理 API（M9/M10/A2/A4-A11）**：M9 自动获取模型单次尝试 Key 上限 50；M10 登录接口 IP 滑窗限流（15 分钟内失败 5 次锁定该 IP，防爆破）；A2 供应商测试总预算 10 分钟（防 50 Key × 120s 无限挂起）；A4 apikeys 更新锁内捕获结果（并发编辑竞态）；A5 日志读操作改读锁并发；A6 「今日」统计边界按服务器本地时区计算；A7 statistics dimension 白名单校验（非法 400）；A8 custom 区间全未来/全过去 400（不再静默全零）；A9 脱敏 Key 恢复改精确比对（Key 内容含 `***` 时不再误恢复）；A10 连通性测试 max_tokens 4096→16（降测试成本）；A11 test / fetch-models 的 base_url 协议校验。
- **前端健壮性（F1-F8）**：F1 代理 Key 编辑弹窗路由切换守卫；F2 调用详情弹窗并发守卫（慢的旧详情不再覆盖后打开的详情）；F3 Anthropic 流重组 usage 全量拷贝（缓存统计字段不再被窄化丢弃）；F4 格式兜底识别支持 Responses input 纯字符串；F5 格式识别 URL 末段精确匹配优先（base_url 含 `/responses` 等子串不再误判）；F6 SSE 未识别格式降级文案与行为对齐（按原文展示、不做 JSON 美化）；F7 监听端口 1-65535 校验（不再静默按 9080 保存）；F8 删除无样式定义的 jcode 死类、补 expand-block 样式。
- **文档与注释修订（D1-D9/S14/P5）**：D1 v1.8.2 批次 7 处注释错标 v1.8.3 改回（B1×2、V-N1×3、V-P2、V-M4、C1 归属）；D2 build.sh / build.bat 帮助文本版本引用改为「源码 buildVersion」（不再随版本升级漏更新）；D3 模型唯一性错误消息与代码对齐（2 处）；D4 4.3.1 格式识别规则按 log-parser 实现改写（四级判定顺序）；D5 `jwt.expire_hours`「非法值自动兜底」过度宣称修正（4 处，类型非法在解析阶段报错）+ v1.8.2 乱码修复漏网的 5 处「兑底」一并修正；D6 「加权轮询」正名「加权随机」（实现为权重区间随机抽样，README / 需求文档 / 前端共 8 处）；D7 历史批次撞号注释补版本前缀（v1.7.6 L2/L7/L11/M2、前端 M9 共 11 处、proxy ReqBody 截断去裸编号），跨批次编号可追溯；D8 api.go L5 注释归属修正（v1.7.6）；D9 v1.8.2 小节补记 C7/C8/E1（C7 无代码变更、具体描述未留存；C8 fail-safe 说明已落 3.2.1；E1 与 D2 同一事项）；S14 新增 9.6 时区约束（统计「今日」边界 / 日志时间解析 / expires_at 补全均按服务器本地时区，容器化与迁移部署需保持时区恒定）；P5 `first_content_ms` 口径补注（同格式透传与跨格式转换同口径识别）。
- **回归测试**：本轮修复配套新增 `audit3_regression_test.go`（convert 16 用例 / proxy 4 用例 / core 2 用例 / logsdb 2 用例 / api 9 用例，覆盖 M9/M10/A4/A6/A7/A8/A9/A10/A11；A2 为时间类预算行为由 A10 用例覆盖 ctx 传递链路），前端 parser-test 73 用例全过；v1.7.6 / v1.8.2 / v1.8.3 全部既有回归用例无回归；全量 8 包测试通过后重新编译。

### v1.8.3（2026-10-03）：第二轮全面复查修复（中危 6 项 + 低危 14 项 + 文档修订 5 项，共 25 项候选；复核后 1 项判定为误报未修改，实修 24 项）

- **日志子系统健壮性（M1/M2/M3）**：M1 清空日志「删除重建」失败（目标路径不可写、磁盘满等）时旧实现日志子系统整体瘫痪（store 置空、后续写入静默丢弃），现重建失败自动回开原库文件持续可用，并在服务日志中醒目提示；M2 清空日志的 store 引用捕获在互斥锁外，与日志路径切换 / 热加载并发时存在引用交错窗口，现移入 logStoreSwitcher 锁内（与 v1.8.2 B1 同一把锁，重建与降级路径同口径）；M3 设置页修改日志数据库路径时「已切换新库但持久化 config.yml 失败」旧实现界面提示成功、重启后却回退旧库（界面 / 运行态 / 持久化三方不一致），现持久化失败自动回滚到原库并记录日志。
- **自动获取模型统一表单模式（F1）**：编辑页「自动获取」旧实现走 GET `?provider_id=` 使用**已保存配置**，与 2.3.5 / 2.5.1「使用表单当前数据、无需先保存」语义矛盾（修改基础地址未保存时，获取结果与页面所见不一致）；现新增页 / 编辑页统一 POST 表单模式（使用表单当前填写内容），编辑页脱敏 Key 由后端按 provider_id + Key id 恢复完整值（与测试接口同模式），GET 接口保留兼容；2.3.5 / 2.6 同步修订。
- **输入校验与边界（L1/L2/L12/F4/F12）**：L1 日志查询 `start_time` / `end_time` 非法时间格式旧实现原样透传 SQL、查询结果静默为空，现返回 400；L2 `jwt.expire_hours` 0 / 负值 / 非法自动按 24 兜底（含热加载路径，5.2.4 同步修订）；L12 供应商测试单次上限 50 条 API Key（超出 400，2.5.2 同步修订）；F4 设置页数值项（冷却时长 / 重试次数 / 上游超时）非正数即时提示，不再「后端静默忽略、前端提示成功」；F12 统计自定义区间起止反向时前端即时提示、后端返回 400。
- **并发与竞态（L3/L4/F8）**：L3 代理 Key 新增 / 修改的唯一性复查移入并发临界区，消除「锁外预检 + 锁内写入」TOCTOU 双写窗口；L4 配置热加载 Manager.Stop 幂等（二次调用不再 panic）；F8 统计页同页快速切换时间范围时旧响应不再覆盖新结果（请求序号守卫）。
- **安全面收口（L8/L10）**：L8 Key 脱敏新增短 Key 规则——≤6 字符全遮蔽、≤10 字符仅保留前 3 位（旧规则「前 3 + `***` + 后 3」在短 Key 上前后段重叠，7 字符 Key 泄露 6/7 字符；3.2.1 / 4.1 / 4.2 / 5.3 同步修订）；L10 管理 JWT 校验签发用户名与当前 `admin.username` 一致——运行中修改管理员用户名后旧 token 全部失效需重新登录（5.2.4 同步修订）。
- **上游与配置健壮性（L6/L7/L11）**：L6 热加载文件解析失败 / 凭证自动生成失败时在服务日志记录原因并保持原配置运行（此前静默跳过，9.1 同步修订）；L7 供应商 `aliases` 类型非法（要求字符串或字符串数组）返回 400，不再吞错后按空别名处理；L11 API 错误消息截断回退 UTF-8 字符边界（中文错误不再截出乱码，与 v1.8.2 C5 同口径）。
- **前端健壮性（F2/F3/F6/F7/F9/F10/F11）**：F2 供应商 / 代理 Key / 仪表盘三视图渲染补迟到响应守卫（v1.8.2 C10 此前只覆盖日志页）；F3 「复制」在非安全上下文（如非 HTTPS 访问）自动降级传统复制方式，不再必然失败；F6 供应商与代理 Key 保存按钮防双击（重复提交产生重复数据）；F7 供应商编辑页自动获取 / 测试等异步动作完成后校验页面序号，迟到结果不再写入已切换页面；F9 视图卸载钩子——统计页 Chart 实例与设置页轮询定时器随路由切换销毁（防内存泄漏与僵尸轮询）；F10 代理 Key 表单「可用模型」数据源加载失败给出提示；F11 代理 Key 有效期回填按本地时区显示（旧实现按 UTC 切日期，东八区上午编辑回填会早一天）。
- **文档修订（D 类）**：D-A README 白名单匹配口径对齐两档制（v1.8.2 A4 的同步说明）；D-B 章节编号修正（3.8→3.7、5.4→5.3，消除跳号）；D-C 6.3 第 1 步白名单口径描述对齐 3.2.3 两档制；D-D 5.2.4 JWT 有效期由「固定 24 小时」改为可配置表述；D-E 6.3 有序列表第 7 / 8 / 9 步缩进修正；README 同步测试上限、短 Key 脱敏、expire_hours 兜底、fetch-models 表单模式、时间参数 400、区间反向校验等口径。
- **复核与误报排除**：25 项候选逐项实测 / 关键行取证；F5（冷却池轮询 stopPoll「误杀新定时器」）深挖后判定不成立（stopPoll 仅在轮询函数开头执行、仅匹配旧定时器自身或空值），未修改；既有已知误报维持结论（build.sh 乱码为 PowerShell GBK 显示误读、PickProvider 循环变量取址在 go 1.22+ 语义安全）。
- **回归测试**：后端修复配套回归测试（`internal/api/audit_regression_test.go` 10 项 + `main_test.go` / `config_test.go` / `logs_test.go` 用例，覆盖 M1 自愈、L1 参数校验、L2 兜底、L3 并发唯一、L4 幂等、L7~L12、F1 脱敏 Key 恢复、F12 反向区间），全量测试 8 包通过后重新编译。

### v1.8.2（2026-10-03）：全面代码审查修复（行为类 4 项 + 竞态 2 项 + 逻辑小修 8 项 + 文档修订）

- **坏 SSE 伪造成功（A1，V-P2）**：跨格式流式时上游返回 200 + SSE 帧但载荷全部无法解析（如被网关替换为 `data: <html>` 错误页），旧实现落入收尾路径向客户端伪造「成功」终止流并记 success；现转换器记录「是否出现过任何可识别事件」，无任何事件时按失败处理（可重试），穷尽后归一化 503、日志记 failure「上游返回了无法解析的 200 响应」。
- **流式首字节前客户端断开仍重试（A2，V-N1）**：流式转发在首字节到达前客户端断开时，旧实现断开与上游 TTFB 赛跑失败即对每条 Key 真实发起上游请求（实测 3 Key 全烧）、且日志误记「all providers failed: 客户端断开」；现断开尝试显式标记 clientCanceled，重试分支检测到后与非流式路径（v1.7.6 M3）同口径：立即按「客户端断开」记 failure 收尾，不再重试（上游仅 1 次请求）、retry_count=0。
- **Responses 文本块累积（A3，V-P3）**：文本→工具→文本交替流中，Responses 发射器的块级累积 textAccum 未在关闭文本块时重置，导致后续块的 `output_text.done` 携带前序块内容（实测 `[A, AB]`）；现关闭时按块重置（`[A, B]`），全响应口径的 `response.completed.output_text` 改由独立的 totalText 跨块累计。
- **模型白名单首候选口径（A4，V-M4）**：多供应商共享别名时，白名单校验旧实现只按全局首候选解析的真实名归一匹配——白名单配置非首候选的真实名时合法调用被整单 403；现改为候选过滤式校验（详见 3.2.3 匹配口径修订）：原始名命中放行全部候选，否则仅保留自身真实名在白名单内的候选，路由只在白名单允许的候选中选择。
- **日志库切换竞态（B1）**：运行态 logStore 被三条路径并发读写（设置页改路径 / 清空重建 / 配置热加载），api 层互斥仅覆盖清空路径；现提取 main 层 logStoreSwitcher 统一互斥（changePath / recreate / close 共用一把锁），消除 store 引用交错与数据竞态，配套互斥回归测试。
- **详情页客户端响应回退误伤新数据（B2）**：v1.7.5 起日志含独立 client_resp_body 列，但前端回退条件未区分数据版本——「客户端断开」等失败日志（client_resp_body 为空、resp_body 是上游未送达内容）被错误展示为客户端响应；现按 upstream_url / client_resp_status 字段识别新数据、不再回退（旧数据兼容不变），判定逻辑提取为 log-parser.js 纯函数并纳入 Node 测试。
- **逻辑小修（C 类）**：C1 跨格式上游请求构建失败由 continue 改 break（不白烧同供应商 Key 重试槽位；注：v1.8.4 发现该 break 实际跳出供应商循环直接 503，已再修正为 continue，见 v1.8.4 小节）；C2 删除 parseModelsResponse 死代码；C3 冷却池列表解析先判长再取下标（不可触发，防御性）；C4 删除非流式 400 分支恒真条件；C5 truncate 截断点回退到 UTF-8 字符边界（中文不再截半乱码）；C6 usage 提取按 SSE 事件帧解析、帧内多行 data: 按规范拼接（多行帧不再提取失败）；C7 无代码变更（上轮审查清单项，无独立注释与复现脚本，具体描述未留存，v1.8.4 D9 补记时如实标注）；C8 IP 白名单为空数组/缺省视为全拒属 fail-safe 合理行为，无代码变更、补文档说明（见 3.2.1「空数组或缺省视为全拒」，v1.8.4 D9 补记）；C9 冷却池开关保存后界面状态同步；C10 日志表加载加请求序号守卫（防慢响应乱序覆盖）。E1（工程卫生）：build.sh 全文件重写修复中文乱码（双重转码损坏），与 D2 为同一事项（v1.8.4 D9 补记）。
- **文档与注释修订（D 类）**：D1 README 构建说明版本号更新；D2 build.sh 全文件重写修复中文乱码（双重转码损坏）；D3 sse.go 头注释更新（「强制上游非流式」已过时）；D4 章节编号修正（功能六/七/八标题重复与错位，功能八子章节 9.1~9.5 随之对齐）；D5 4.3.3 统计行措辞对齐实现、9.1 热加载清单补 `log.enabled`、4.4 重建窗口措辞修正；另修复代码注释与文档中 8 处「皤/兑底」类编码乱码（含本节 v1.7.6 小节的「兑底」）。
- **回归测试**：A1~A4 / B1 / C5 / C6 各配套回归测试（proxy / convert / main 三包 + 前端 Node 测试），全量测试通过后重新编译。

### v1.8.1（2026-10-03）：日志记录开关 + 清空释放磁盘空间

- **记录调用日志开关**：设置页基础设置新增「记录调用日志」开关（配置 `log.enabled`），**默认关闭**；旧配置文件无该字段时同样视为关闭（从旧版本升级后日志自动停记，需记录时在设置页打开）。关闭后新调用（成功/失败/模型未匹配）不再写入日志库，已有日志仍可查询/删除；热加载立即生效（设置页保存或手改 `config.yml` 约 2 秒生效）；仪表盘与统计在关闭期间不再产生新数据。
- **清空即删除重建**：日志页「删除全部日志」由 SQL `DELETE FROM call_logs` 改为**关闭当前库 → 删除库文件及 `-wal` / `-shm` 伴生文件 → 原路径重建空库**——SQLite 删除行不缩小文件，删除重建才能把磁盘空间即时归还文件系统；重建期间在途调用的日志写入新库；文件被外部工具占用删除失败时降级为 SQL 清空（数据清空、空间暂不释放）；接口返回不变（`{"ok":true,"deleted":<行数>}`，行数为重建前统计）。
- 前端确认文案与成功提示同步更新（提示将删除日志数据库文件并重建以释放空间）。

### v1.8.0（2026-09-30）：调用详情标签页化与内容解析

- **详情弹窗三标签页**：调用详情由单列长页面改为「客户端记录（默认）/ 上游记录 / 原始数据」三标签页；基本信息与尝试记录（结构化列表）提为常驻区；标签页懒渲染、滚动位置独立记忆；弹窗加宽至约 1100px。
- **请求体结构化解析**：消息按卡片展示（role 徽章配色），content parts / Anthropic blocks / Responses input 条目逐段解析（文本、图片 URL、base64 内嵌图片、音频、tool_calls、tool_result、thinking 全覆盖）；tool_calls 的 arguments 二次解析为格式化 JSON；tools 定义可折叠卡片（含 JSON Schema 格式化）；顶层采样参数 kv 化；未识别字段收入「其他字段」折叠区，与已知字段合计对账原始 JSON 全部 key。
- **响应体结构化解析**：非流式响应按 choices / content blocks / output 条目卡片化 + usage kv（含 cached / reasoning 细分）；错误响应红色提示卡；**流式 SSE 重组视图**——CC / Responses / Anthropic 三格式按帧累积重组为最终结果（delta 拼接、tool_calls 分片聚合、usage 与 finish_reason 提取），底部统计总帧数/总字节，坏帧警示计数。
- **格式自动识别**：按 URL 路径优先、body 结构兜底识别 OpenAI Chat Completions / Responses / Anthropic 三格式，四组数据各自独立识别（跨格式转发两侧格式不同时各自正确解析）。
- **原始数据标签页增强**：JSON 语法高亮（前端自实现）；1MB 截断检测显示醒目中文警示条；所有代码块提供「复制」按钮。
- **实现方式**：纯前端变更（后端接口与存储零改动）；新增 `web/log-parser.js` 解析引擎（纯函数、解析与渲染分离）；请求头/响应头仅保留在原始数据标签页。

### v1.7.6（2026-09-30）：全面代码审查修复（高危 3 项 + 中危 10 项 + 低危多项）

- **日志库关闭竞态（H1）**：`WriteAsync` 与 `Close` 存在 send-on-closed-channel panic 竞态（实测并发写+关闭几乎每轮可复现），重构为 quit/done 信号对 + 锁内 send，优雅关闭与日志库热切换不再 panic/丢日志。
- **配置热加载数据竞争（H4）**：`Manager.Cfg()` 返回共享指针且 `Update` 原地修改，与管理接口并发读构成 data race；改为 copy-on-write 快照发布（`Update` 作用在浅拷贝上，旧快照不可变）。Watch 的「检测→读文件→替换」全程移入锁内，消除「外部编辑触发 Watch 期间界面并发保存被旧文件内容覆盖」的 write-skew。
- **统计接口脱敏（H3）**：`by_apikey` 维度此前明文返回完整代理 Key，现脱敏输出（分组依据不变）。
- **别名解析模型名错配（M1）**：同一别名在不同供应商指向不同真实模型时，发往上游的模型名全局取自第一个命中供应商导致选错模型；现每个候选携带自身命中的真实名，冷却键/日志实际模型同口径；同供应商内多模型共享别名不再重复入候选（权重翻倍）。
- **白名单别名误拒（M2）**：代理 Key 可用模型按客户端原始串字面匹配，合法别名被 403；现按「原始名或解析后真实名」归一匹配。认证基础项提前至请求体解析之前（对齐 6.3 顺序）。
- **客户端断开日志归类（M3）**：非流式请求客户端断开时，旧实现连带上游请求取消、烧光全部 Key 重试槽位并误记「all providers failed: context canceled」；现上游 ctx 脱离客户端 ctx，断开不烧重试，日志与流式同口径记「客户端断开」。
- **跨格式流式异常路径（M5）**：上游 200+无法解析响应体（错误 JSON）时不再伪造成功空流，按失败重试/降级；伪流式兜底成功的日志 usage 不再全记 0（SSE 提取失败自动回退 JSON 提取）。
- **供应商测试接口回显脱敏（M6）**：测试详情的认证头脱敏（前 10+尾 4，与日志同口径），需求 2.5.3 同步修订。
- **Anthropic 流发射器索引（M7）**：文本块不再硬编码 index 0，与工具块统一顺序分配——修复「工具先于文本/交替」场景产生非法事件序列（严格 SDK 断连）。
- **Responses 流 error 事件（M8）**：`type=error` 事件此前无处理分支被静默丢弃并在 EOF 伪造成功终止流；现归一为错误事件（未写客户端时可重试），`response.failed` 正确提取 `response.error.message`。
- **IP 白名单规范化**：非规范缺尾点前缀规则（`10*`）视为无效忽略（fail-safe）；IPv6 大小写/压缩形式归一比较。
- **前端**：跨视图渲染竞态守卫（旧响应不再覆盖新页面、冷却池轮询不再复活）；编辑保存后完整 Key 缓存失效；重复查询销毁旧 Chart 实例；随机 Key 拒绝采样消除取模偏差。
- **其他**：`req_body` 入库按 1MB 截断（对齐 4.3）；管理 API 请求体 8MB 上限；`page_size` 上限 500；keyword LIKE 通配符转义（`%`/`_` 按字面匹配）；`end_time` 纯日期补 23:59:59；代理 Key 格式校验（sk-+32位，存量未变更可编辑）；JWT 校验 alg 头、登录用户名常量时间比较；`-port`/`-host` 覆盖不落盘；日志路径热更先切换后持久化；SSE 帧扫描器 16MB 上限；分页同秒记录按 rowid 稳定排序；修复 flaky 测试 `TestStreamLogFourWayData`（mock 上游加帧间隔）。
- 全部修复配套回归测试（`fix_regression_test.go` / `resolve_test.go` / `fix_stream_test.go` 等），全量测试通过。

### v1.7.5（2026-09-23）：调用日志四组请求/响应数据 + 速率口径调整

- **四组请求/响应数据（日志结构重构）**：调用详情按传输方向完整记录——客户端 → 代理（请求路径 client_req_url、请求头、请求体）、代理 → 上游（upstream_url、请求头**认证头脱敏**（保留前 10 + 尾 4 字符）、请求体，跨格式为转换后内容，可与客户端原始请求对照定位转换问题）、上游 → 代理（状态码、响应头、响应体副本）、代理 → 客户端（状态码、响应头、响应体，跨格式为转换后内容，流式为已发送部分）。
- **字段语义修正**：`req_url` 旧版记录上游 URL（新数据改由 upstream_url 承担，旧库字段保留）；流式 `resp_body` 旧版被覆盖为发给客户端的字节（现统一为上游响应副本，客户端侧内容由 client_resp_body 承担）；模型未匹配 404 状态由 client_resp_status 记录。旧库启动自动 ALTER TABLE 补列，前端详情页对旧数据兼容回退。
- **速率口径调整**：流式调用 token 速率从**模型供应商返回给代理的首个响应字节**开始计算（last_chunk_ms - first_byte_ms，模型实际吐字窗口，不含排队与网络往返）；非流式调用按**整个请求时间**计算；first_byte_ms 入库并在详情页展示。
- **详情页四段式展示**：客户端请求 / 上游请求 / 上游响应 / 客户端响应分四段展示，基本信息补上游首字节。

### v1.7.4（2026-09-23）：终止帧延迟发出 + 客户端断开误判修复

- **终止帧延迟**：跨格式流式转换器收到上游 finish_reason 不再立即发终止帧，统一延迟到流真正结束（[DONE] / message_stop / response.completed / EOF）——终止帧 usage 不再全 0，且避免客户端收完终止帧正常断开后被误记「客户端断开」。
- **完成误判修复**：终止帧已成功写出后，客户端关闭连接属正常完成，记 success 不再记 failure。

### v1.7.3（2026-09-23）：工具调用历史回放 400 修复

- **问题**：Codex 断流重试回带工具调用历史，Responses → Chat Completions 转换时同一 assistant 回合的文本被拆成独立消息，导致 tool 结果不紧跟发起调用的 assistant，校验严格的上游（MiniMax）400 invalid params。
- **修复**：同一 assistant 回合的文本与工具调用合并为一条消息（CC 格式允许 content 与 tool_calls 共存）。

### v1.7.2（2026-09-23）：跨格式流式工具收尾事件补发

- **问题**：OpenAI CC 流式协议无工具调用结束信号（由 finish_reason 隐式标记），转换器永不产生 evToolEnd，导致 Responses 客户端（Codex）收到的流缺 function_call_arguments.done / output_item.done，严格校验后解析失败断开（表现「客户端断开」）。
- **修复**：StreamConverter.finalize() 为「已开始未结束」的工具条目统一补发 evToolEnd（幂等）。

### v1.7.1（2026-09-23）：需求对照代码审查修复

- 修复 7 个 bug：流式上游 400 误重试、IP 层级 ≥ 2 提取 off-by-one、新增供应商页 Key 测试失效、日志时间筛选时区错乱、上游 400 日志与客户端消息不一致、丢弃计数数据竞争、过时测试断言；新增 first_content_ms / last_chunk_ms / sent_bytes / last_sent_ms 观测字段。

### v1.7.0（2026-09-21）：流式边收边转发 + 失败日志消息一致性修复

- **流式实时转发（架构变更）**：`stream: true` 请求不再全缓冲，改为**边收边转发**——上游数据一到即写给客户端并 Flush，客户端获得与直连一致的逐字体验，彻底消除长生成任务下客户端等不到数据而超时断开的问题（表现为 `读取上游响应失败: context canceled`）。
  - **同格式**：上游 SSE 字节级实时透传。
  - **跨格式**：上游 SSE 事件流**实时转换**为目标客户端格式的 SSE 事件流（`convert.StreamConverter`：SSE 帧扫描器 + 三格式解析器 + 三格式发射器流水线，3×3 组合全覆盖，含文本增量与工具调用分片映射）；跨格式流式时上游也以流式请求并附带 `stream_options.include_usage`（OpenAI 类型）。
  - **延迟提交与重试**：首个有效数据到达前不写客户端，失败可重试换 Key / 供应商；部分兼容网关对 `stream: true` 返回完整 JSON 时自动兜底为「解析 + 一次性合成 SSE」。
  - **日志口径不变**：仍在内存累积上游副本（64MB 上限，超出仅转发不累积），流结束后提取 usage、计算生成耗时与输出速率；`resp_body` 记录实际返回给客户端的内容。
- **失败日志消息一致性修复**：`writeFailureLog` 不再自行构造错误消息——全部冷却中（cooling down）、无可用 Key（no provider attempted）等场景下，日志 `error_msg` 与返回给客户端的最终消息保持一致（此前日志误记为 `all providers failed (no provider attempted)`）。
- **已知边界**：OpenAI/Responses 上游 → Anthropic 客户端的跨格式流式，客户端侧 `input_tokens` 可能记 0（上游 usage 在流末到达而 `message_start` 已发出；`output_tokens` 与日志统计均正确）。

### v1.6.1（2026-09-21）：伪流式上游速率显示修复

- **问题**：部分流式调用日志的输出速率显示为 `-`——上游为伪流式/缓冲型（整个 SSE 缓冲到最后一次性发出）时，首字节耗时≈总耗时，生成耗时被算成 0，速率记 0；个别首字节仅早于结束 1ms 的记录速率虚高至数十万 tok/s。
- **修复**：上游响应记录分块到达次数（ReadChunks）；流式且响应体分多块到达（真流式）时生成耗时 = 总耗时 - 首字节耗时；响应体一次性到达或差值≤0 时回退为总耗时（与非流式同口径，速率略低估、不虚高）。
- **兼容**：仅影响新生成日志的速率口径，转发行为与历史日志不变。

### v1.6（2026-09-21）：冷却池增强 + 日志筛选区两行布局

- **冷却池启用开关**：设置页新增「是否启用冷却池」开关（配置 `cooldown.enabled`）；停用后失败不再入池，已有条目不再参与路由过滤；旧配置无此字段时默认启用。
- **时长单位改秒**：冷却池时长由分钟改为**秒**（`duration_minutes` → `duration_seconds`，默认 300）；旧配置自动迁移为秒，保存后不再输出旧字段。
- **失败重试次数**：新增 `cooldown.retry_count`（默认 1）——同一 `(供应商, 模型)` 连续「全部 Key 失败」达到阈值才入冷却池，期间任一次成功即清零计数；手动移除/清空条目同步清零计数（3.4、6.4）。
- **限流预留分区隐藏**：设置页不再展示限流（预留）卡片；配置结构仍预留（3.5、功能六）。
- **日志筛选区两行布局**：第一行状态/供应商/API Key/模型/关键词，第二行时间范围 + 查询/重置按钮组（定宽 96px、分隔线分组）；控件同高对齐、窄屏自动换行。

### v1.5（2026-09-21）：速率计算修复 + 筛选区样式优化

- **输出速率**：`tokens_per_second` 修正为输出 Token ÷ 生成耗时秒（此前误用总 Token ÷ 总耗时）。
- **生成耗时**：新增 `generation_ms` 字段（首 token → 响应结束）：流式 = 首 chunk 到流结束，不含首 token 延迟；非流式 / 跨格式 ≈ 总耗时。旧库自动补列。详情页展示总耗时与生成耗时。
- **总耗时口径修正**：`duration_ms` 此前取响应头到达时刻（不含响应体传输），现改为响应体读完毕时刻，流式场景耗时更真实。
- **筛选区样式**：下拉/输入框统一高度、时间控件加宽、关键词/供应商/API Key 控件分级定宽、间距优化，窄屏自动换行；列表速率列头改为「输出速率(tok/s)」。

### v1.4（2026-09-21）：未匹配模型日志记录 + 列表 Key 脱敏

- **未匹配模型记录失败日志**：模型未匹配（返回 404）的请求同样写入调用日志，状态为 `failure`，错误原因「未匹配到对应模型」，`resp_status` 记录 404；`provider` 为空、`retry_count` 为 0（6.3 第 3 步）。
- **列表 API Key 脱敏**：`GET /api/logs` 列表返回的 `apikey` 字段改为脱敏值（前 3 位 + `***` + 后 3 位，与 API Key 筛选下拉一致），节省列宽并降低泄露面；详情接口保留完整 Key（4.1、4.2）。
- 列表页「实际模型」列为空时显示 `-`（未匹配场景无实际模型）。

### v1.3（2026-09-21）：调用日志速率与删除、供应商编辑页优化

- **Token 速率**：调用日志新增 `tokens_per_second` 吞吐速率（total_tokens ÷ 耗时秒，0 时记 0），旧库启动自动补列；列表与详情均展示。
- **日志删除**：新增 `DELETE /api/logs/:id`（单条）与 `DELETE /api/logs`（清空全部，均需管理员 + 二次确认）；当前页删空自动回第一页。
- **筛选下拉化**：供应商 / API Key 筛选由输入框改为下拉框（Key 显示「名称（脱敏 Key）」按 apikey_id 精确筛选）；已删除对象可经关键词搜索历史日志。
- **供应商编辑页**：模型测试下拉默认选中第一个模型（增删联动）；保存成功后自动返回供应商列表页。

### v1.2（2026-09-21）：多接口格式支持

- **供应商接口类型**：新增供应商时可选择接口类型——`OpenAI Chat Completions`（默认）/ `OpenAI Responses API` / `Anthropic Messages API`；配置字段 `providers[].api_type`，旧配置缺省视为 openai。
- **用户暴露端点**：新增 `POST /v1/responses`（Responses API 格式）与 `POST /v1/messages`（Anthropic Messages API 格式），与既有 `POST /v1/chat/completions` 并列。
- **跨格式自动转换**：任意"客户端调用格式 × 供应商接口类型"组合（9 种）自动双向转换（请求 + 响应），见 6.7 转换矩阵；同格式保持字节级透传。
- **流式跨格式**：客户端请求 `stream:true` 时返回按客户端格式合成的完整 SSE 事件流（含 usage）；上游强制非流式。
- **认证兼容**：对话端点兼容 Anthropic SDK 风格 `x-api-key` 头；`/v1/messages` 错误返回 Anthropic 格式。
- **/v1/models**：响应增加 `type` / `display_name` 字段（OpenAI + Anthropic 超集）。
- **供应商测试 / 自动获取模型**：按接口类型构造请求与端点（2.5.2、2.3.5）。

### v1.1（2026-09-19）：对齐实际实现

供应商表单五分区布局、设置页限流预留分区、`/api/models` 接口、模型名替换语义（字节级透传/仅替换 model）、上游响应 64MB 上限、热加载清单补全为 8 项、构建脚本（build.bat / build.sh）与交叉编译说明。

> AI生成