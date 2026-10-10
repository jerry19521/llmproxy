---
AIGC:
  ContentProducer: '001191110102MAD55U9H0F10002'
  ContentPropagator: '001191110102MAD55U9H0F10002'
  Label: '1'
  ProduceID: '6f926dd0-fdbe-4710-8554-5e3e406cba13'
  PropagateID: '6f926dd0-fdbe-4710-8554-5e3e406cba13'
  ReservedCode1: '5e97ec11-e5ff-4e3a-a5a7-a69eb4d75691'
  ReservedCode2: '5e97ec11-e5ff-4e3a-a5a7-a69eb4d75691'
---

# LLMProxy 大模型代理工具

统一管理多个大模型供应商的代理服务：对外暴露 OpenAI Chat Completions、OpenAI Responses API、Anthropic Messages API 三种接口，对内完成身份认证、模型别名解析、智能路由、失败重试与熔断降级、跨格式自动转换，并记录完整调用日志供审计与统计。

单文件部署、零外部依赖（SQLite 为纯 Go 驱动，无需 CGO），前端管理界面已编译进可执行文件。

---

## 目录

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [首次登录](#首次登录)
- [使用指南](#使用指南)
  - [管理界面导览](#管理界面导览)
  - [接入供应商](#接入供应商)
  - [发放代理 Key](#发放代理-key)
  - [客户端调用](#客户端调用)
- [配置说明](#配置说明configyml)
- [HTTP 接口参考](#http-接口参考)
- [核心机制说明](#核心机制说明)
- [项目结构](#项目结构)
- [常见问题](#常见问题-faq)
- [安全提示](#安全提示)

---

## 功能特性

### 供应商管理

- 多供应商增删改查、启用/停用
- **接口类型**：每个供应商可选 `OpenAI Chat Completions`（默认）/ `OpenAI Responses API` / `Anthropic Messages API`，决定请求上游的端点、认证头与请求格式；用户调用格式与供应商格式无需一致
- 每个供应商可配置多条 API Key（`sk-` 格式），支持**逐条测试**与**批量测试**（按接口类型构造测试请求；单次最多测试 50 条 Key，超出 400 拒绝；整批测试设 10 分钟总预算，v1.8.4；测试请求 max_tokens=16，仅验证连通性，v1.8.4），测试结果展示请求/响应详情（认证头脱敏，保留前 10 + 尾 4 字符）
- 自定义请求头（如 `X-Custom: abc`），调用上游时附加；可含 `User-Agent`（覆盖全局默认，优先级：供应商自定义头 > 全局默认 `default_user_agent` > Go 默认，v1.8.6）
- 模型列表支持**别名**（如 `gpt-4o` 配置别名 `gpt4`，客户端用别名调用即可路由），大小写不敏感
- **自动获取模型**：通过供应商的模型列表接口拉取可用模型（端点按接口类型自动适配），弹窗勾选后批量导入
- 同供应商内模型名称唯一性校验（大小写不敏感），前端实时提示、后端 400 拒绝

### 代理 Key 管理

- 发放 `sk-` 前缀 32 位随机 Key，客户端调用本代理的凭证；自定义 Key 必须符合 `sk-` + 32 位字母数字格式，否则 400 拒绝
- **有效期**（到期自动失效）、**IP 白名单**（`*` 全放行 / `192.168.*` 前缀 / 精确 IP；前缀规则须以 `.*` 结尾，非规范写法如 `10.1*` 按无效规则忽略；IPv6 大小写归一；**空数组或缺省视为全拒**——fail-safe 设计，手写 yml 留空将拒绝一切请求，管理界面新建/编辑 Key 时会默认补 `["*"]`）、**可用模型限制**（不勾选则不限制；客户端用别名调用时，解析后的真实模型名命中白名单同样放行）
- 列表脱敏显示（`sk-***xyz`，短 Key 仅保留前 3 位），支持「显示完整 Key」与「一键复制」（非安全上下文自动降级复制方案，局域网 http 访问可用）
- 启用/停用、真删除

### 核心代理转发

- **三种调用端点**：`POST /v1/chat/completions`（OpenAI Chat Completions）、`POST /v1/responses`（OpenAI Responses API）、`POST /v1/messages`（Anthropic Messages API）、`GET /v1/models`
- **跨格式自动转换**：客户端调用格式与供应商接口类型可以任意组合（3 × 3 = 9 种），代理自动双向转换请求与响应；同格式保持字节级透传；转换覆盖思考内容（CC `reasoning_content` / Anthropic `thinking` 块 / Responses `reasoning` 条目双向映射，v1.8.4；Responses 条目仅有 summary 数组时兜底读取，v1.8.5）与图文混排消息（图片块完整展开不再丢失，v1.8.4）；跨格式流式的无参工具调用 arguments 自动补 `{}`（v1.8.4）
- **认证兼容**：除 `Authorization: Bearer` 外，对话端点兼容 Anthropic SDK 风格的 `x-api-key` 头；`/v1/messages` 的错误响应返回 Anthropic 格式
- **模型别名解析**：客户端传别名 → 自动替换为真实模型名发往上游，其余请求字段原样透传
- **全局路由策略**：随机（默认）/ 轮询 / 加权随机（按供应商 `weight` 比例随机分配；v1.8.4 D6：旧称「加权轮询」与实现不符，已正名）
- **供应商内 Key 随机**：无论全局策略为何，供应商内部的多条 Key 均按随机选取
- **失败重试**：同一供应商内切换 Key 重试（1 次原始请求 + 最多 2 次重试，共 3 次上游请求）；客户端主动断开时不做无谓重试，直接按失败收尾并记日志
- **跨供应商降级**：某供应商所有 Key 失败后，自动切换到其他拥有该模型的供应商（不同接口类型的供应商可互为灾备）
- **冷却熔断**：`(供应商, 模型)` 粒度冷却池（可启用/停用，时长按秒配置），连续失败达到重试次数后才入池，冷却期内路由自动绕开；后端每 30 秒定时清理到期条目
- **流式边收边转发**：`stream: true` 请求上游数据一到即转发客户端并 Flush（与直连一致的逐字体验），同时在内存累积副本供日志统计；同格式字节级实时透传，跨格式实时转换为目标客户端格式 SSE（v1.7.0）
- 上游响应大小限制 64MB，防御异常响应导致内存耗尽

### 调用日志

- **记录开关（v1.8.1，默认关闭）**：设置页「记录调用日志」开关（配置 `log.enabled`）；关闭后新调用不再写入日志库（已有日志仍可查询/删除），需审计排查时再开启；热加载即时生效；旧配置无该字段同样视为关闭（升级后自动停记）
- SQLite 异步写入（队列满时丢弃不阻塞主流程，关闭时报告丢弃计数）
- 无论重试多少次，每次调用最终只写 **1 条**日志，标记最终状态
- `retry_count` 重试次数 + `attempt_logs` 每次尝试明细（供应商 / Key / 错误信息，详情页结构化列表展示）
- **四组请求/响应数据**（v1.7.5，排查时完整还原链路；各组入库上限 1MB，超长部分截断并以 `...(truncated)` 标记，前端检测到截断时显示醒目中文警示条）：
  - 客户端 → 代理：`client_req_url`（请求路径）、`req_method`、`req_headers`、`req_body`
  - 代理 → 上游：`upstream_url`、`upstream_req_headers`（认证头脱敏）、`upstream_req_body`（跨格式为转换后请求，可与客户端原始请求对照）
  - 上游 → 代理：`resp_status`、`resp_headers`、`resp_body`（上游响应体副本）
  - 代理 → 客户端：`client_resp_status`、`client_resp_headers`、`client_resp_body`（跨格式为转换后响应；流式为已发送部分）
  - 旧库自动补列；v1.7.5 之前的旧日志中 `req_url` 记录的是上游 URL、流式 `resp_body` 为发给客户端的字节，前端详情页已做兼容回退
- **调用详情标签页化与内容解析**（v1.8.0）：详情弹窗改为「客户端记录（默认）/ 上游记录 / 原始数据」三标签页（懒渲染、滚动位置独立记忆，弹窗加宽至约 1100px）；基本信息与尝试记录为常驻区，弹窗打开期间锁定背景页面滚动
  - **结构化解析**：OpenAI Chat Completions / Responses / Anthropic Messages 三格式按 URL 路径优先、body 结构兜底自动识别（四组数据各自独立识别，跨格式转发两侧各自正确解析）；消息按卡片展示（system/user/assistant/tool 角色徽章配色），content parts 全覆盖（文本、图片 URL、base64 内嵌图片可预览、音频、tool_calls、tool_result、thinking 推理内容）
  - **tool_calls 参数二次解析**：arguments JSON 字符串格式化为缩进代码块；tools 定义可折叠卡片（含 JSON Schema 格式化）；顶层采样参数 kv 化；未知字段收入「其他字段」折叠区，与已知字段合计对账原始 JSON 全部 key，不丢弃任何字段
  - **流式 SSE 重组视图**：SSE 帧流重组为与非流式同构的最终结果（delta 拼接、tool_calls 分片聚合、usage 与 finish_reason 提取），底部统计总帧数/总字符，坏帧警示计数；原始 SSE 帧文本可在「原始数据」标签页完整查看
  - **降级兼容**：1MB 截断、非 JSON（HTML 错误页）、未识别格式均回退原文展示并标注；解析引擎为纯函数模块（`web/log-parser.js`，解析与渲染分离）
- Token 用量与输出速率：`tokens_per_second` = 输出 Token ÷ `generation_ms`
- 速率口径（v1.7.5）：流式调用从**上游首个响应字节**开始计算（`last_chunk_ms - first_byte_ms`，即模型实际吐字窗口，不含排队与网络往返）；非流式调用按**整个请求时间**（`duration_ms`）计算；`first_byte_ms` 已入库可在详情页查看
- 模型未匹配的调用同样记录失败日志（status = failure，错误原因「未匹配到对应模型」，client_resp_status = 404）；代理 Key **模型白名单**拒绝的 403 越权调用同样写调用日志（v1.8.4，Key 被滥用探测时可在日志页发现）；401 无效 Key 与 IP 白名单 403 维持不入库（防扫描刷库）
- 分页查询（页码上限 100000，v1.8.4），支持状态 / 供应商（下拉）/ API Key（下拉）/ 模型 / 关键词 / 时间范围多维筛选；列表页 API Key 脱敏显示（前 3 位 + *** + 后 3 位，短 Key 仅保留前 3 位，与筛选下拉一致），详情页保留完整 Key
- 支持单条删除与一键清空全部日志（均二次确认）；**清空即删除重建（v1.8.1）**：清空时删除日志数据库文件及 `-wal` / `-shm` 伴生文件后在原路径重建空库，磁盘空间即时释放（SQL DELETE 不会缩小 SQLite 文件）；文件被外部占用时降级为 SQL 清空

### 管理后台

- JWT 登录（HS256，令牌算法严格校验），密码 bcrypt 哈希存储；修改密码后所有旧 token 立即失效；登录接口按 IP 滑窗限流（15 分钟内失败 5 次锁定该 IP，v1.8.4）；token 绑定签发时的 secret 摘要（v1.8.4 改 HMAC-SHA256：升级后旧 token 全部失效，需重新登录一次）
- **仪表盘**：供应商/Key 概览、今日调用、总调用、成功率、Token 用量、最近调用
- **统计分析**：时间序列（按天/按小时）、按供应商/按模型/按 Key 维度聚合（Key 显示脱敏形式），本地 Chart.js 离线图表（无外网依赖）
- **设置页**：基础设置（监听地址、IP 层级、超时、日志记录开关、日志路径、测试消息）、路由策略、冷却池配置（启用开关 / 时长秒数 / 失败重试次数，热加载）与冷却池实时列表（10 秒自动刷新、支持逐条移除与清空）

### 运维能力

- **配置热加载**：修改 `config.yml` 后约 2 秒自动生效（监听地址/端口、`jwt.secret`、管理员密码除外）
- **凭证自动生成**：首次启动或配置中密码/secret 为空时，自动生成并打印到控制台
- **优雅关闭**：收到 Ctrl+C / SIGTERM 后停止接收新请求，等待在途请求完成（30 秒上限），日志队列排空落盘
- 启动参数覆盖：`-config` / `-port` / `-host`（`-port` / `-host` 仅本次运行生效，不写回配置文件）
- 前端资源 `embed` 编译进单一可执行文件（约 12MB），部署只需一个文件
- **时区约束（v1.8.4）**：统计「今日」边界、日志时间参数解析、`expires_at` 无时区补全均按服务器本地时区计算；容器化/迁移部署需保持时区恒定（如 Docker 设置 `TZ` 环境变量），否则统计边界随环境漂移

---

## 快速开始

### 环境要求

- 运行：Windows / Linux / macOS（Go 交叉编译产物均可），无其他依赖
- 编译：Go 1.22+

### 编译

**方式一：编译脚本（推荐）**

```bash
# Windows
build.bat                    # 编译当前系统 → llmproxy.exe
build.bat linux              # 交叉编译 linux/amd64 → dist\llmproxy-linux-amd64
build.bat darwin arm64       # 交叉编译 macOS → dist\llmproxy-darwin-arm64

# Linux / macOS
./build.sh                   # 编译当前系统 → ./llmproxy
./build.sh windows amd64     # 交叉编译 → dist/llmproxy-windows-amd64.exe
```

| 目标系统 | 支持架构 |
|---------|---------|
| windows | 386, amd64, arm64 |
| linux | 386, amd64, arm, arm64 |
| darwin | amd64, arm64 |

不带参数默认编译当前系统，产物放项目根目录；指定平台则交叉编译到 `dist/` 并带平台后缀。可选环境变量：`VERSION=1.2.3` 注入版本号（默认使用源码中的 buildVersion）；`GOPROXY` 未设置时默认使用国内镜像。

**方式二：手动编译**

```bash
# 网络受限时使用国内镜像
export GOPROXY=https://goproxy.cn,direct

CGO_ENABLED=0 go build -ldflags "-s -w" -o llmproxy.exe .
```

### 运行

```bash
# 默认读取当前目录 config.yml，监听 0.0.0.0:9080
./llmproxy.exe

# 指定配置文件与端口
./llmproxy.exe -config /etc/llmproxy/config.yml -port 8080

# 仅监听本机
./llmproxy.exe -host 127.0.0.1
```

| 启动参数 | 默认值 | 说明 |
|---------|--------|------|
| `-config` | `config.yml` | 配置文件路径 |
| `-port` | 配置文件端口 | 覆盖监听端口（仅本次运行生效，不写回配置文件） |
| `-host` | 配置文件地址 | 覆盖监听地址（仅本次运行生效，不写回配置文件） |

首次启动会自动在配置文件同目录生成 `config.yml` 与日志数据库 `data.db`。

---

## 首次登录

1. 启动后控制台打印初始管理员密码：

```
========================================
  首次启动：已自动生成管理员初始密码
  用户名: admin
  密  码: Xk9mP2qR7wLz
  （请尽快登录后修改密码）
========================================
```

2. 浏览器访问 `http://127.0.0.1:9080/`，用 `admin` + 初始密码登录
3. 点击右上角「修改密码」立即更换

**忘记密码**：停止服务 → 将 `config.yml` 中 `admin.password` 的值改为 `""` → 重启（或等待热加载），系统会自动生成新密码并打印到控制台。

---

## 使用指南

### 管理界面导览

| 菜单 | 功能 |
|------|------|
| 📊 仪表盘 | 运行概览、今日/累计调用、成功率、Token、最近 10 条调用 |
| 🔌 供应商管理 | 供应商增删改查、Key 测试、自动获取模型 |
| 🔑 代理 Key | 发放/管理客户端调用凭证 |
| 📜 调用日志 | 全量调用记录（含未匹配模型的失败记录，记录开关默认关闭 v1.8.1）、多维筛选（供应商/Key 下拉）、Token 速率、单条/全部删除（清空即删除库文件重建释放空间）、调用详情三标签页（客户端记录/上游记录/原始数据）与请求/响应体结构化解析（v1.8.0） |
| 📈 统计分析 | 时间序列与维度聚合图表 |
| ⚙️ 设置 | 基础设置、路由策略、冷却池配置与监控 |

### 接入供应商

1. **供应商管理 → 新增供应商**
2. 填写**基本信息**：名称（如 `OpenAI`）、基础地址、**接口类型**、权重（加权随机策略下生效）
   - **OpenAI Chat Completions**（默认）：基础地址如 `https://api.openai.com/v1`，请求发往 `{基础地址}/chat/completions`
   - **OpenAI Responses API**：基础地址如 `https://api.openai.com/v1`，请求发往 `{基础地址}/responses`
   - **Anthropic Messages API**：基础地址如 `https://api.anthropic.com`（末尾不带 `/v1`），请求发往 `{基础地址}/v1/messages`，携带 `x-api-key` + `anthropic-version` 头
3. **API Key 区域**添加供应商的调用凭证，可逐条测试连通性（测试请求按接口类型自动构造）
4. （可选）添加**自定义请求头**（含 `User-Agent`，可覆盖全局默认；遇上游拦截 Go 默认 UA 返回 403 时用，见下方常见问题）
5. **模型列表**：手动添加模型（名称 + 可选别名），或点击「⤓ 自动获取」从上游模型列表接口拉取勾选导入
6. 在**模型测试**区域选择一个模型，对所有 Key 批量发起真实调用测试（测试消息内容在「设置」页配置）；测试直接使用当前填写的内容，**无需先保存**（已保存供应商的脱敏 Key 自动恢复完整值参与测试）；测试详情回显的请求/响应中认证头已脱敏（保留前 10 + 尾 4 字符）

### 发放代理 Key

1. **代理 Key → 新增代理 Key**
2. 填写名称（如 `客户端A`），点击「随机生成」产生 `sk-` Key，或自定义（须为 `sk-` + 32 位字母数字，否则保存被拒绝）
3. 按需配置有效期、IP 白名单、可用模型范围
4. 保存后通过「显示 / 复制」把完整 Key 交给客户端

### 客户端调用

客户端将 SDK / HTTP 请求的 base_url 指向本代理即可，无需修改其他代码。三种接口格式任选，与供应商实际类型无关（代理自动转换）：

```python
# 方式一：OpenAI Chat Completions
from openai import OpenAI

client = OpenAI(
    api_key="sk-你的代理Key",
    base_url="http://127.0.0.1:9080/v1"
)

resp = client.chat.completions.create(
    model="gpt4",              # 模型名或别名均可，大小写不敏感
    messages=[{"role": "user", "content": "你好"}],
    # stream=True              # 支持流式
)
print(resp.choices[0].message.content)
```

```python
# 方式二：OpenAI Responses API（客户端用 Responses 格式，供应商可以是任意类型）
resp = client.responses.create(
    model="gpt4",
    input="你好"
)
print(resp.output_text)
```

```python
# 方式三：Anthropic Messages API（base_url 指向代理根路径，不带 /v1）
import anthropic

ant = anthropic.Anthropic(
    api_key="sk-你的代理Key",
    base_url="http://127.0.0.1:9080"
)

msg = ant.messages.create(
    model="gpt4",              # 供应商是 OpenAI 类型也没关系，自动转换
    max_tokens=1024,
    messages=[{"role": "user", "content": "你好"}]
)
print(msg.content[0].text)
```

```bash
# curl：Chat Completions
curl http://127.0.0.1:9080/v1/chat/completions \
  -H "Authorization: Bearer sk-你的代理Key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt4","messages":[{"role":"user","content":"hi"}]}'

# curl：Responses API
curl http://127.0.0.1:9080/v1/responses \
  -H "Authorization: Bearer sk-你的代理Key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt4","input":"hi"}'

# curl：Anthropic Messages API（支持 x-api-key 头）
curl http://127.0.0.1:9080/v1/messages \
  -H "x-api-key: sk-你的代理Key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}'

# 查询可用模型（含别名；OpenAI/Anthropic 客户端均可解析）
curl http://127.0.0.1:9080/v1/models \
  -H "Authorization: Bearer sk-你的代理Key"
```

---

## 配置说明（config.yml）

```yaml
listen:
  host: "0.0.0.0"        # 监听地址（★需重启生效）
  port: 9080             # 监听端口（★需重启生效）
ip_trust_level: 0        # IP 层级：0=直连 IP；1=取 XFF 首个；2=取 XFF 倒数第二个，依此类推（热加载）
admin:
  username: "admin"
  password: "<bcrypt>"   # 留空则启动时自动生成并打印（热加载时同样自动重新生成）
jwt:
  secret: ""             # 留空则自动生成；变更后所有登录失效（★需重启生效）
  expire_hours: 24       # 登录 token 有效期；配置为 0/负值时自动回退 24
log:
  enabled: false          # 是否记录调用日志（v1.8.1，默认关闭）；旧配置无此字段时同样视为关闭（热加载）
  db_path: "data.db"     # 调用日志 SQLite 路径（热加载，切换后新日志写入新库）
routing:
  strategy: "random"     # random | round_robin | weighted（热加载）
cooldown:
  enabled: true            # 是否启用冷却池（热加载）
  duration_seconds: 300    # 冷却池时长（秒，热加载）；旧配置 duration_minutes 自动迁移为秒
  retry_count: 1           # 连续失败多少轮后才入池（热加载）；期间成功即清零计数
upstream_timeout_seconds: 300   # 上游请求超时（热加载）
test_message: "hi"       # 供应商 Key 测试发送内容（热加载）
default_user_agent: ""   # 发往上游的默认 User-Agent（v1.8.6，热加载）；空 = 不设置（Go 默认 Go-http-client/1.1）；供应商自定义头中的 UA 优先

providers:               # 供应商列表
  - id: "p1"
    name: "OpenAI"
    base_url: "https://api.openai.com/v1"
    api_type: "openai"    # openai(默认) | responses | anthropic
    enabled: true
    weight: 1
    api_keys:
      - id: "k1"
        key: "sk-xxxx"
    headers:
      - key: "X-Custom"
        value: "abc"
    models:
      - name: "gpt-4o"
        aliases: ["gpt4", "4o"]
    created_at: "2026-09-19T10:00:00+08:00"
    updated_at: "2026-09-19T10:00:00+08:00"
  - id: "p2"
    name: "Anthropic"
    base_url: "https://api.anthropic.com"   # anthropic 类型末尾不带 /v1
    api_type: "anthropic"
    enabled: true
    weight: 1
    api_keys:
      - id: "k2"
        key: "sk-ant-xxxx"
    headers: []
    models:
      - name: "claude-sonnet-4-5"
    created_at: "2026-09-21T10:00:00+08:00"
    updated_at: "2026-09-21T10:00:00+08:00"

apikeys:                 # 代理 Key 列表
  - id: "a1"
    name: "客户端A"
    key: "sk-代理Key..."
    expires_at: ""       # ISO 8601，留空=永久
    ip_whitelist: ["*"]  # * 全放行 | 192.168.* 前缀（须以 .* 结尾，非规范写法按无效忽略）| 精确 IP
    enabled: true
    models: []           # 留空=不限制可用模型
    created_at: "2026-09-19T10:00:00+08:00"
    updated_at: "2026-09-19T10:00:00+08:00"
```

> **热加载规则**：除带 ★ 的三项外，其余字段修改保存后约 2 秒自动生效，无需重启。管理界面的修改实时写回此文件（保存基于最新配置快照原子写入，界面上的并发保存不会互相覆盖丢失）。

---

## HTTP 接口参考

### 代理端点（代理 Key 认证）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | 对话补全，OpenAI Chat Completions 格式，支持 stream |
| POST | `/v1/responses` | OpenAI Responses API 格式，支持 stream |
| POST | `/v1/messages` | Anthropic Messages API 格式，支持 stream |
| GET | `/v1/models` | 可用模型列表（含别名；响应为 OpenAI + Anthropic 超集格式） |

认证：`Authorization: Bearer <代理Key>`；对话端点亦兼容 `x-api-key: <代理Key>`（Anthropic SDK 风格）。

**错误响应格式随端点**：`/v1/chat/completions` 与 `/v1/responses` 返回 OpenAI 风格；`/v1/messages` 返回 Anthropic 风格：

```json
{ "error": { "message": "invalid api key", "type": "invalid_request_error", "code": "invalid_api_key" } }
```

```json
{ "type": "error", "error": { "type": "authentication_error", "message": "invalid api key" } }
```

| 场景 | 状态码 | code（OpenAI 端点） |
|------|--------|------|
| 请求体不符合端点格式 / 缺少 model | 400 | `invalid_model` |
| 代理 Key 无效 / 已停用 / 已过期 | 401 | `invalid_api_key` |
| IP 不在白名单 | 403 | `ip_not_allowed` |
| 模型不在该 Key 的可用列表 | 403 | `model_not_allowed` |
| 模型从未被任何供应商配置 | 404 | `model_not_found` |
| 上游返回 400（请求本身有误，不重试） | 502 | `bad_gateway` |
| 全部供应商失败 / 全部冷却中 | 503 | `service_unavailable` |

### 管理接口（JWT 认证，`Authorization: Bearer <token>`）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/login` | 登录获取 token |
| POST | `/api/logout` | 登出 |
| PUT | `/api/password` | 修改密码（旧 token 全部失效） |
| GET/POST | `/api/providers` | 供应商列表 / 新增 |
| GET/PUT/DELETE | `/api/providers/{id}` | 供应商详情（Key 脱敏）/ 修改 / 删除 |
| POST | `/api/providers/{id}/enable` `/disable` | 启用 / 停用 |
| POST | `/api/providers/test` | 用表单当前填写的内容测试 Key，无需先保存（body: `{"provider_id":"可选","base_url":"...","api_type":"openai/responses/anthropic","api_keys":[{"id":"...","key":"..."}],"headers":[],"model":"...","key_ids":[...]}`，`key_ids` 留空测全部；脱敏 Key 按 provider_id 恢复；测试请求按接口类型构造；回显的请求/响应中认证头脱敏） |
| GET | `/api/providers/fetch-models?provider_id=` | 已保存供应商自动获取模型（端点与认证头按接口类型适配） |
| POST | `/api/providers/fetch-models` | 表单模式自动获取模型（新增页与编辑页统一）：按表单当前数据获取（body 含 base_url、api_type、api_keys、headers、provider_id 可选）；编辑页的脱敏 Key 按 provider_id + key id 自动恢复完整值（与测试接口同模式）；单次尝试 Key 上限 50，超出 400（v1.8.4）；整批尝试受 10 分钟总预算约束（v1.8.5），超时停止尝试剩余 Key、客户端断开即终止 |
| GET/POST | `/api/apikeys` | 代理 Key 列表（脱敏）/ 新增（`expires_at` 兼容 RFC3339 / `YYYY-MM-DD` / 无时区 `YYYY-MM-DDTHH:MM[:SS]`，统一按 RFC3339 存储；Key 须为 `sk-` + 32 位字母数字，非法格式 400 拒绝） |
| GET/PUT/DELETE | `/api/apikeys/{id}` | Key 详情（含完整 Key）/ 修改 / 删除 |
| POST | `/api/apikeys/{id}/enable` `/disable` | 启用 / 停用 |
| GET/PUT | `/api/settings` | 读取 / 保存设置（含 `log.enabled` 日志记录开关与 `log.db_path`） |
| GET | `/api/cooldowns` | 冷却池列表 |
| POST | `/api/cooldowns/remove` `/clear` | 移除单条 / 清空冷却池 |
| GET | `/api/logs` | 日志分页查询（参数：page、page_size 上限 500、status、provider、model、apikey_id、keyword、start_time、end_time；end_time 传纯日期时自动补全为当天 23:59:59；无法解析的时间参数返回 400 而非静默过滤） |
| GET | `/api/logs/{id}` | 日志详情（含完整请求/响应与 attempt_logs） |
| DELETE | `/api/logs/{id}` | 删除单条日志 |
| DELETE | `/api/logs` | 清空全部日志（v1.8.1：删除库文件及 -wal/-shm 并原路径重建空库，磁盘空间即时释放；返回删除前条数） |
| GET | `/api/models` | 管理端模型列表（含别名映射） |
| GET | `/api/dashboard` | 仪表盘数据 |
| GET | `/api/statistics` | 统计数据（参数：range=today/7d/30d/custom，custom_start、custom_end 为 YYYY-MM-DD 且 start ≤ end（反向区间 400）；custom 区间完全落在未来时 400（v1.8.4）；dimension 仅接受白名单值，非法 400（v1.8.4）；by_key 维度的 Key 已脱敏） |
| GET | `/health` | 健康检查（无需认证） |

> 管理接口请求体上限 8MB，超出将被拒绝。

---

## 核心机制说明

### 请求处理流程

```
客户端请求（三种端点格式之一）
  │
  ├─ [1] 认证：Key 存在? 启用? 未过期? IP 白名单?
  │        失败 → 401 / 403（错误格式随客户端端点）
  ├─ [1.5] 按端点格式解析请求体（失败或缺少 model → 400）
  ├─ [2] 模型解析：名称或别名（大小写不敏感）→ 候选供应商集合，各供应商按自身
  │        配置解析真实模型名（同名别名可指向不同模型）
  │        ├─ 未命中 → 404，并记录失败日志（错误原因「未匹配到对应模型」）
  │        └─ Key 可用模型校验（两档制，v1.8.2）：原始名命中白名单 → 放行全部候选；
  │             否则仅保留自身解析后真实名在白名单内的候选，空则 403 model_not_allowed
  ├─ [3] 冷却池过滤：排除冷却中的 (供应商, 模型)（冷却池停用时跳过过滤）
  ├─ [4] 循环：路由策略选出供应商（不同接口类型的供应商可同池竞争）
  │        ├─ 构建上游请求：
  │        │    ├─ 同格式 → 别名/大小写命中仅替换 model 字段，其余字节级透传
  │        │    └─ 跨格式 → 请求体转换为供应商格式；stream 随客户端（流式时附
  │        │         stream_options.include_usage，OpenAI 类型）
  │        ├─ 供应商内 Key 随机排序，最多尝试 3 次上游请求
  │        │    ├─ 成功 → 跳到 [8]
  │        │    ├─ 400  → 不重试，归一化 502 返回
  │        │    ├─ 客户端断开 → 不重试，记 failure「客户端断开」
  │        │    └─ 其他失败（超时/401/403/429/5xx）→ 换下一条 Key 重试
  │        └─ 该供应商所有 Key 失败 → 连续失败达到重试次数后加入冷却池 → 换下一家供应商
  ├─ [7] 全部失败 → 503
  └─ [8] 成功：
          ├─ 非流式：同格式原样转发；跨格式转换为客户端格式 JSON
          └─ 流式：边收边转发——同格式字节级实时透传；跨格式实时转换为目标
               客户端格式 SSE；首事件前失败可重试，开始转发后中断只断流记日志
          最终只写 1 条调用日志（含 retry_count 与 attempt_logs，token 用量按格式归一化；
          四组请求/响应数据记录：客户端请求 / 代理→上游请求 / 上游响应 / 代理→客户端响应
          （各组入库上限 1MB，超长截断标记）；
          generation_ms：流式 = 上游首字节→末字节，非流式 = 总耗时，
          tokens_per_second = 输出 Token ÷ 生成耗时秒）
```

### 路由策略对比

| 策略 | 行为 | 适用场景 |
|------|------|---------|
| random（默认） | 候选中随机选一家 | 通用，简单分流 |
| round_robin | 按创建顺序轮流（每个模型独立计数） | 均衡分配请求量 |
| weighted | 按 `weight` 比例加权随机 | 不同供应商容量不同 |

### 多格式自动转换

供应商接口类型与客户端调用格式独立选择，代理自动在两者之间转换（同格式透传，不经转换器）：

| 客户端格式 ↓ \ 供应商类型 → | Chat Completions | Responses API | Anthropic API |
|---|---|---|---|
| `/v1/chat/completions` | 透传 | CC ↔ Responses 双向转换 | CC ↔ Anthropic 双向转换 |
| `/v1/responses` | Responses ↔ CC 双向转换 | 透传 | Responses ↔ Anthropic 双向转换 |
| `/v1/messages` | Anthropic ↔ CC 双向转换 | Anthropic ↔ Responses 双向转换 | 透传 |

转换覆盖的主要语义：系统提示词（system 消息 / instructions / system 字段）、思考内容（CC reasoning_content / Anthropic thinking 块 / Responses reasoning 条目双向映射，v1.8.4；Responses 条目仅有 summary 数组时兜底读取，v1.8.5）、多模态图片（image_url / input_image / base64 source）、工具定义与调用（function / function_call / tool_use）、工具结果、tool_choice（`none` 转 Anthropic 时省略该字段，回退其默认 auto）、最大输出 Token（Anthropic 侧必填，缺省 4096）、停止序列、温度 / top_p、JSON 模式、usage 三项 Token 统计。个别无对应参数的字段（如 Responses 的 stop、Anthropic 的 JSON 模式）静默丢弃。详细映射见需求文档 6.7。

### 冷却熔断

- **开关与参数**：可在设置页启用/停用冷却池，配置冷却时长（秒）与失败重试次数；均热加载
- **触发条件**：某供应商某模型的**所有 Key** 均失败（供应商内 Key 重试不触发冷却）；且**连续失败轮数达到「失败重试次数」**（默认 1，即一轮失败即冷却；期间任一次调用成功即清零计数）
- **效果**：冷却期内路由自动绕开该 `(供应商, 模型)` 组合；停用后失败不再入池，已有条目也不再生效
- **恢复**：到期自动恢复；也可在设置页手动移除或清空（同时清零失败计数）
- **监控**：设置页实时展示冷却条目与剩余时间，10 秒自动刷新

### 流式边收边转发（v1.7.0）

`stream: true` 请求边收边转发：上游数据一到即写入客户端并 Flush，客户端获得与直连一致的逐字体验（v1.6.x 及以前为有限缓冲：收完 SSE 提取 usage 后一次性转发）。

- **同格式**：上游 SSE 字节级实时透传
- **跨格式**：上游 SSE 事件流实时转换为目标客户端格式的 SSE（三段式流水线：SSE 帧扫描器 → 三格式解析器 → 三格式发射器，3×3 组合全覆盖，含文本增量与工具调用分片映射）；Responses 格式事件序列对齐官方协议，工具调用含 `function_call_arguments.delta/done`，max_output_tokens 截断时为 `response.incomplete`
- **延迟提交与重试**：首个有效数据到达前不向客户端写任何字节，此阶段失败（网络错误 / 非 2xx / 流首事件错误 / 空流 / 200 但响应无法解析为有效流）照常换 Key / 换供应商重试；部分网关对 stream=true 返回完整 JSON 时自动兜底为「解析 + 一次性合成 SSE」
- **日志与统计不变**：内存累积上游副本（64MB 上限，超出仅转发不累积），流结束后提取 usage（三格式自动归一化；SSE 模式提取全 0 时自动回退按整体 JSON 提取，覆盖伪流式兜底场景）、计算生成耗时（真流式 = 总耗时 - 首字节耗时，伪流式/一次性到达回退总耗时）与输出速率
- **边界**：OpenAI/Responses 上游 → Anthropic 客户端的跨格式流式，客户端侧 `input_tokens` 可能记 0（上游 usage 在流末到达而 `message_start` 已发出；`output_tokens` 与日志统计均正确）；上游不返回 usage 时 Token 计 0；中途断开视为失败记 failure 日志

---

## 项目结构

```
llmproxy/
├── main.go                      # 启动、路由组装、安全响应头、优雅关闭、前端 embed
├── config.yml                   # 运行时生成/读取的配置（首次启动自动创建）
├── data.db                      # 调用日志 SQLite（运行时创建）
├── internal/
│   ├── config/
│   │   ├── config.go            # 配置定义、加载/原子保存、凭证自动生成、热加载轮询
│   │   └── util.go              # UUID 工具
│   ├── auth/
│   │   └── auth.go              # JWT(HS256) 签发校验、bcrypt、IP 提取与白名单匹配
│   ├── convert/                 # ★ 三格式互转（v1.2 新增）
│   │   ├── convert.go           # IR 中间表示、分发、usage 提取、错误输出、URL/认证头适配
│   │   ├── openai.go            # Chat Completions 解析/构建
│   │   ├── responses.go         # Responses API 解析/构建
│   │   ├── anthropic.go         # Anthropic Messages 解析/构建
│   │   ├── sse.go               # 三格式 SSE 事件流合成（非流式响应合成/流式兜底）
│   │   ├── stream.go            # 跨格式流式实时转换（SSE 扫描器+解析器+发射器，v1.7.0）
│   │   ├── convert_test.go      # 单元测试（往返转换/usage/SSE）
│   │   └── stream_test.go       # 流式转换单元测试
│   ├── core/
│   │   └── core.go              # copy-on-write 运行时快照、模型解析、路由策略、
│   │                            #   冷却池（30 秒定时清理）
│   ├── proxy/
│   │   ├── proxy.go             # 核心转发：认证/别名替换/重试/降级/同格式透传/
│   │   │                        #   跨格式转换/流式边收边转发/日志写入
│   │   ├── proxy_test.go        # 单元测试（速率/生成耗时）
│   │   └── proxy_integration_test.go  # 集成测试（mock 三格式上游，9 组合验证）
│   ├── logsdb/
│   │   └── logsdb.go            # SQLite 异步写入、分页筛选查询、统计聚合
│   └── api/
│       ├── api.go               # 登录/密码/设置/冷却池/仪表盘/统计 + 路由注册
│       ├── providers.go         # 供应商 CRUD、Key 测试（按类型）、自动获取模型
│       ├── apikeys.go           # 代理 Key CRUD
│       └── logs.go              # 日志查询接口
├── web/                         # 原生 HTML+CSS+JS 管理界面（embed 编译进二进制）
│   ├── index.html               # 单页外壳
│   ├── style.css                # 全局样式
│   ├── api.js / ui.js / app.js  # 请求封装 / 组件工具 / 路由与登录
│   ├── views-dashboard.js       # 仪表盘
│   ├── views-providers.js       # 供应商管理（含接口类型选择、测试/自动获取弹窗）
│   ├── views-apikeys.js         # 代理 Key 管理
│   ├── views-logs.js            # 调用日志（详情三标签页 + 结构化解析，v1.8.0）
│   ├── log-parser.js            # 日志内容解析引擎（纯函数：三格式请求/响应解析 + SSE 重组 + JSON 高亮，v1.8.0）
│   ├── views-statistics.js      # 统计分析（Chart.js）
│   ├── views-settings.js        # 设置与冷却池
│   └── chart.umd.js             # Chart.js 本地文件（离线图表）
└── .temp/                       # 开发测试用（mock 上游、e2e 脚本），非交付物
```

---

## 常见问题 FAQ

**Q: 修改监听端口后为什么没生效？**
监听地址/端口需重启服务生效（其余配置均为热加载）。管理界面修改时会提示「需重启服务后生效」。命令行 `-port` / `-host` 覆盖仅本次运行生效、不写回配置文件，重启后回到配置文件值。

**Q: 热加载会覆盖我在管理界面的修改吗？**
管理界面所有修改实时写回 `config.yml`，与手工编辑的最终落盘结果一致。界面保存基于最新配置快照原子写入，界面上的并发保存不会互相覆盖丢失；与手工编辑文件几乎同时发生时（2 秒窗口内），后保存的一方覆盖先保存的一方。

**Q: 客户端传的模型名和上游收到的一样吗？**
- 客户端传**真实模型名**（完全一致，含大小写）且与供应商同格式 → 请求体字节级原样转发
- 客户端传**别名**或仅大小写不同 → 仅 `model` 字段替换为真实名，其余字段原样保留
- **跨格式**调用（如用 Anthropic 格式调 Chat Completions 供应商）→ 请求体经转换器重建，模型名始终为解析后的真实名
- **多家供应商配置同一别名**时，各供应商按自己的配置解析真实模型名发送（可为不同模型）
- 代理 Key 的**可用模型限制**（两档制，v1.8.2）：白名单配真实名、客户端用别名调用同样放行（反之亦然）；原始名不命中时，多供应商共享别名场景只在真实名命中的候选中路由，不再整单拒绝

**Q: 客户端调用格式和供应商接口类型不一致时会怎样？**
代理自动双向转换，无需任何额外配置。例如供应商是 Anthropic 类型，客户端照样可以用 OpenAI Chat Completions / Responses API 格式调用；反之亦然。同格式请求则保持字节级透传，零转换损耗。转换不支持的参数（如 Responses 的 stop 序列）会静默丢弃，不会报错。

**Q: 日志里出现「客户端断开」是什么原因？**
这是客户端主动断开了连接（取消/超时/关闭页面）。v1.7.6 起此类情况被准确归类为「客户端断开」并记 failure：非流式调用不消耗重试槽位（不再误记为「all providers failed」烧光全部 Key），日志可直接用于排查客户端侧超时设置。v1.7.0 之前代理对流式请求做全缓冲转发，长生成任务期间客户端收不到任何字节，容易触发客户端超时断开；v1.7.0 起改为边收边转发后该问题基本消除。若仍偶发，通常是客户端自身设置了请求超时（如 60s AbortSignal），可调大其超时或调小任务规模。

**Q: 跨格式流式的 usage 统计可靠吗？**
跨格式流式时上游也以流式请求，OpenAI 类型供应商会附带 `stream_options.include_usage`（官方标准字段，主流兼容网关均支持）；Responses / Anthropic 上游天然在流末携带 usage。个别不支持该字段的网关可能 Token 计 0；上游对 stream=true 返回完整 JSON 时自动兜底按非流式解析，usage 提取在 SSE 模式全 0 时自动回退整体 JSON 提取（v1.7.6），不受兜底影响。日志统计从上游原始副本提取，与客户端侧展示相互独立。

**Q: 调用详情里的流式日志怎么看？**
v1.8.0 起详情弹窗分「客户端记录 / 上游记录 / 原始数据」三个标签页：前两个标签页把请求体/响应体结构化解析（消息卡片、工具调用参数格式化、Token 用量），流式 SSE 响应自动**重组为最终结果**（与非流式同构，底部标注总帧数；原始逐帧文本在「原始数据」标签页查看）。跨格式调用时两侧格式不同（如客户端 Responses、上游 Anthropic），两个标签页各自独立识别解析。超过 1MB 截断的数据会显示警示条并回退原文展示。

**Q: 日志库会无限增长吗？**
可控制。v1.8.1 起提供两种手段：①设置页关闭「记录调用日志」（默认即关闭），新调用不再写入；②日志页「删除全部日志」一键清空——删除库文件及 `-wal` / `-shm` 伴生文件后原路径重建空库，磁盘空间即时释放（SQLite 删除行不缩小文件，删除重建才能腾出空间）。也可逐条删除，或定期备份后删除 `data.db`（服务停止时），或在设置页修改日志路径切换到新库。

**Q: 忘记管理员密码怎么办？**
停止服务 → 将 `config.yml` 中 `admin.password` 改为 `""` → 重启，控制台会打印自动生成的新密码。运行中手工清空该字段也会触发热加载重新生成。

**Q: 部署在 Nginx 反向代理之后，IP 白名单怎么配置？**
设置页将「IP 层级」调为 1（一层代理），此时从 `X-Forwarded-For` 首个 IP 获取客户端真实 IP；两层代理配 2，依此类推。

**Q: curl 能调通的上游，代理/测试 Key 返回 403？**
部分上游网关（如腾讯元宝 bot）按 User-Agent 拦截 Go 默认客户端（`Go-http-client/1.1`），返回 403「错误请求」——与 Key、IP、X-Forwarded-For 是否正确无关（v1.8.6 实测复现）。解决：设置页「默认 User-Agent」配为 curl / 浏览器等常见 UA（全局生效，热加载），或仅在需要的那家供应商自定义请求头里加 `User-Agent`（优先于全局默认）。留空则保持 Go 默认，与旧版本行为一致。

---

## 安全提示

- **日志数据库**：`data.db` 中以明文存储代理 Key（客户端请求头与日志字段）及请求/响应体（各组上限 1MB），请按敏感数据管理（限制文件权限、备份加密、勿随代码分发）；发往上游的认证头在日志中已脱敏（前 10 + 尾 4 字符）；无审计需要时可在设置页关闭「记录调用日志」（默认已关闭，v1.8.1）
- **配置文件**：`config.yml` 含 bcrypt 密码哈希与 JWT secret，请勿外传；怀疑泄露时清空 `jwt.secret` 并重启重新生成（所有登录会失效）
- **监听地址**：默认 `0.0.0.0` 对外开放。仅供本机使用时建议改为 `-host 127.0.0.1` 或修改配置
- **代理 Key**：具备等同你上游账号的消费能力，请仅发给可信客户端，并按需配置 IP 白名单与有效期

> AI生成