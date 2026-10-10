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

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.5+-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)](#快速开始)
[![Release](https://img.shields.io/badge/release-v1.0.0-brightgreen)](https://github.com/jerry19521/llmproxy/releases)

统一管理多个大模型供应商的代理服务：对外暴露 **OpenAI Chat Completions**、**OpenAI Responses API**、**Anthropic Messages API** 三种接口，对内完成身份认证、模型别名解析、智能路由、失败重试与熔断降级、跨格式自动转换，并记录完整调用日志供审计与统计。

单文件部署、零外部依赖（SQLite 为纯 Go 驱动，无需 CGO），前端管理界面已编译进可执行文件。

> GitHub 仓库：[jerry19521/llmproxy](https://github.com/jerry19521/llmproxy)

---

## 目录

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [首次登录](#首次登录)
- [使用指南](#使用指南)
- [配置说明（config.yml）](#配置说明configyml)
- [HTTP 接口参考](#http-接口参考)
- [核心机制说明](#核心机制说明)
- [项目结构](#项目结构)
- [常见问题](#常见问题-faq)
- [安全提示](#安全提示)
- [许可证](#许可证)

---

## 功能特性

### 供应商管理

- **多供应商接入**：增删改查、启用/停用，统一管理 OpenAI、Anthropic、各类国产与三方兼容网关
- **三种接口类型**：每个供应商可选 `OpenAI Chat Completions` / `OpenAI Responses API` / `Anthropic Messages API`，决定上游端点、认证头与请求格式；客户端调用格式与供应商格式无需一致
- **多 API Key 管理**：每供应商可配置多条 Key，支持**逐条测试**与**批量测试**（按接口类型自动构造请求，单次最多 50 条，超出 400 拒绝；整批测试设 10 分钟总预算）
- **自定义请求头**：调用上游时附加（含 `User-Agent`，可覆盖全局默认）
- **模型别名**：如 `gpt-4o` 配置别名 `gpt4`，客户端用别名调用即可路由；大小写不敏感
- **自动获取模型**：从上游模型列表接口拉取可用模型，弹窗勾选后批量导入
- **同供应商内模型名唯一性校验**（大小写不敏感），前端实时提示、后端 400 拒绝

### 代理 Key 管理

- 发放 `sk-` 前缀 32 位随机 Key；自定义 Key 必须符合 `sk-` + 32 位字母数字格式，否则 400 拒绝
- **有效期**、**IP 白名单**（`*` / `192.168.*` 前缀 / 精确 IP；空数组视为全拒，fail-safe）、**可用模型限制**
- 列表脱敏显示（`sk-***xyz`，短 Key 仅保留前 3 位），支持「显示完整 Key」与「一键复制」

### 核心代理转发

- **三种调用端点**：`POST /v1/chat/completions`、`POST /v1/responses`、`POST /v1/messages`、`GET /v1/models`
- **跨格式自动转换**：客户端格式与供应商接口类型任意组合（3 × 3 = 9 种）自动双向转换；同格式保持字节级透传；转换覆盖思考内容（`reasoning_content` / `thinking` 块 / `reasoning` 条目）与图文混排
- **认证兼容**：对话端点除 `Authorization: Bearer` 外，兼容 Anthropic SDK 风格的 `x-api-key` 头
- **模型别名解析**：客户端传别名 → 自动替换为真实模型名发往上游
- **路由策略**：随机（默认）/ 轮询 / 加权随机（按供应商 `weight` 比例随机分配）
- **失败重试**：同供应商内切换 Key 重试（最多 3 次上游请求）
- **跨供应商降级**：某供应商所有 Key 失败后自动切换到其他拥有该模型的供应商
- **冷却熔断**：`(供应商, 模型)` 粒度冷却池（可启用/停用），连续失败达到重试次数后入池
- **流式边收边转发**：`stream: true` 请求上游数据一到即转发客户端，内存累积副本供日志统计

### 调用日志

- **记录开关**（默认关闭）：设置页「记录调用日志」开关（`log.enabled`），关闭后新调用不再写入日志库
- SQLite 异步写入，每次调用最终只写 1 条日志（标记最终状态 + `retry_count` + `attempt_logs`）
- **四组请求/响应数据**完整记录：客户端→代理 / 代理→上游 / 上游→代理 / 代理→客户端，各组入库上限 1MB
- **调用详情三标签页**（客户端记录 / 上游记录 / 原始数据）结构化解析消息卡片、工具调用参数、SSE 流重组视图
- 分页查询（页码上限 100000，page_size 上限 500），支持状态 / 供应商 / API Key / 模型 / 关键词 / 时间范围多维筛选
- 单条删除与一键清空（清空即删除数据库文件重建，磁盘空间即时释放）

### 管理后台

- JWT 登录（HS256），密码 bcrypt 哈希存储；登录接口按 IP 滑窗限流（15 分钟内失败 5 次锁定）
- **仪表盘**：供应商/Key 概览、今日/累计调用、成功率、Token 用量、最近调用
- **统计分析**：时间序列（按天/按小时）、按供应商/按模型/按 Key 维度聚合，本地 Chart.js 离线图表
- **设置页**：基础设置、路由策略、冷却池配置与实时监控（10 秒自动刷新）

### 运维能力

- **配置热加载**：修改 `config.yml` 后约 2 秒自动生效（监听地址/端口、`jwt.secret` 除外）
- **凭证自动生成**：首次启动或密码/secret 为空时自动生成并打印到控制台
- **优雅关闭**：收到 Ctrl+C / SIGTERM 后停止接收新请求，等待在途请求完成（30 秒上限）
- 启动参数覆盖：`-config` / `-port` / `-host`（仅本次运行生效，不写回配置文件）
- 前端资源 `embed` 编译进单一可执行文件（约 12MB），部署只需一个文件

---

## 快速开始

### 环境要求

- 运行：Windows / Linux / macOS（Go 交叉编译产物均可），无其他运行时依赖
- 编译：Go 1.26.5+（与 `go.mod` 一致）

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

不带参数默认编译当前系统；指定平台则交叉编译到 `dist/` 并带平台后缀。可选环境变量：`VERSION=1.2.3` 注入版本号（默认使用源码中的 `buildVersion`）；`GOPROXY` 未设置时默认使用国内镜像。

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
| 📜 调用日志 | 全量调用记录、多维筛选、Token 速率、单条/全部删除、调用详情三标签页 |
| 📈 统计分析 | 时间序列与维度聚合图表 |
| ⚙️ 设置 | 基础设置、路由策略、冷却池配置与监控 |

### 接入供应商

1. **供应商管理 → 新增供应商**
2. 填写**基本信息**：名称、基础地址、**接口类型**、权重
   - **OpenAI Chat Completions**（默认）：基础地址如 `https://api.openai.com/v1`
   - **OpenAI Responses API**：基础地址如 `https://api.openai.com/v1`
   - **Anthropic Messages API**：基础地址如 `https://api.anthropic.com`（末尾不带 `/v1`），自动附加 `x-api-key` + `anthropic-version` 头
3. **API Key 区域**添加调用凭证，可逐条测试连通性
4. （可选）添加**自定义请求头**（含 `User-Agent`）
5. **模型列表**：手动添加模型（名称 + 可选别名），或点击「⤓ 自动获取」从上游拉取勾选导入
6. 在**模型测试**区域对一个模型批量测试所有 Key（测试请求按接口类型自动构造；测试消息在「设置」页配置）

### 发放代理 Key

1. **代理 Key → 新增代理 Key**
2. 填写名称，点击「随机生成」产生 `sk-` Key，或自定义（须为 `sk-` + 32 位字母数字）
3. 按需配置有效期、IP 白名单、可用模型范围
4. 保存后通过「显示 / 复制」把完整 Key 交给客户端

### 客户端调用

客户端将 SDK / HTTP 请求的 `base_url` 指向本代理即可，无需修改其他代码。三种接口格式任选，与供应商实际类型无关（代理自动转换）：

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
# 方式二：OpenAI Responses API（供应商可以是任意类型）
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
  enabled: false          # 是否记录调用日志（默认关闭）；旧配置无该字段同样视为关闭（热加载）
  db_path: "data.db"     # 调用日志 SQLite 路径（热加载，切换后新日志写入新库）
routing:
  strategy: "random"     # random | round_robin | weighted（热加载）
cooldown:
  enabled: true            # 是否启用冷却池（热加载）
  duration_seconds: 300    # 冷却池时长（秒，热加载）；旧配置 duration_minutes 自动迁移为秒
  retry_count: 1           # 连续失败多少轮后才入池（热加载）；期间成功即清零计数
upstream_timeout_seconds: 300   # 上游请求超时（热加载）
test_message: "hi"       # 供应商 Key 测试发送内容（热加载）
default_user_agent: ""   # 发往上游的默认 User-Agent；空 = 不设置（Go 默认）；供应商自定义头中的 UA 优先

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
| POST | `/api/providers/test` | 用表单当前填写的内容测试 Key，无需先保存 |
| GET | `/api/providers/fetch-models?provider_id=` | 已保存供应商自动获取模型 |
| POST | `/api/providers/fetch-models` | 表单模式自动获取模型（新增/编辑页统一） |
| GET/POST | `/api/apikeys` | 代理 Key 列表（脱敏）/ 新增（Key 须为 `sk-` + 32 位字母数字） |
| GET/PUT/DELETE | `/api/apikeys/{id}` | Key 详情（含完整 Key）/ 修改 / 删除 |
| POST | `/api/apikeys/{id}/enable` `/disable` | 启用 / 停用 |
| GET/PUT | `/api/settings` | 读取 / 保存设置（含 `log.enabled` 与 `log.db_path`） |
| GET | `/api/cooldowns` | 冷却池列表 |
| POST | `/api/cooldowns/remove` `/clear` | 移除单条 / 清空冷却池 |
| GET | `/api/logs` | 日志分页查询（参数：page、page_size 上限 500、status、provider、model、apikey_id、keyword、start_time、end_time） |
| GET | `/api/logs/{id}` | 日志详情（含完整请求/响应与 attempt_logs） |
| DELETE | `/api/logs/{id}` | 删除单条日志 |
| DELETE | `/api/logs` | 清空全部日志（删除库文件并原路径重建，磁盘空间即时释放） |
| GET | `/api/models` | 管理端模型列表（含别名映射） |
| GET | `/api/dashboard` | 仪表盘数据 |
| GET | `/api/statistics` | 统计数据（参数：range=today/7d/30d/custom，custom 区间反向 400，dimension 白名单校验） |
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
  ├─ [2] 模型解析：名称或别名（大小写不敏感）→ 候选供应商集合
  │        ├─ 未命中 → 404，并记录失败日志
  │        └─ Key 可用模型校验（两档制）：原始名命中白名单 → 放行全部候选；
  │             否则仅保留自身解析后真实名在白名单内的候选，空则 403
  ├─ [3] 冷却池过滤：排除冷却中的 (供应商, 模型)（冷却池停用时跳过）
  ├─ [4] 循环：路由策略选出供应商
  │        ├─ 构建上游请求：
  │        │    ├─ 同格式 → 别名/大小写命中仅替换 model 字段，其余字节级透传
  │        │    └─ 跨格式 → 请求体转换为供应商格式；stream 随客户端
  │        ├─ 供应商内 Key 随机排序，最多尝试 3 次上游请求
  │        │    ├─ 成功 → 跳到 [8]
  │        │    ├─ 400  → 不重试，归一化 502 返回
  │        │    ├─ 客户端断开 → 不重试，记 failure「客户端断开」
  │        │    └─ 其他失败（超时/401/403/429/5xx）→ 换下一条 Key 重试
  │        └─ 该供应商所有 Key 失败 → 连续失败达到重试次数后加入冷却池 → 换下一家
  ├─ [7] 全部失败 → 503
  └─ [8] 成功：
          ├─ 非流式：同格式原样转发；跨格式转换为客户端格式 JSON
          └─ 流式：边收边转发——同格式字节级实时透传；跨格式实时转换为目标
               客户端格式 SSE
          最终只写 1 条调用日志（含 retry_count 与 attempt_logs，token 用量按格式归一化；
          四组请求/响应数据完整记录）
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

转换覆盖的主要语义：系统提示词（system / instructions / system 字段）、思考内容（CC `reasoning_content` / Anthropic `thinking` 块 / Responses `reasoning` 条目双向映射）、多模态图片（image_url / input_image / base64 source）、工具定义与调用、工具结果、tool_choice、最大输出 Token（Anthropic 侧必填，缺省 4096）、停止序列、温度 / top_p、JSON 模式、usage 三项 Token 统计。个别无对应参数的字段（如 Responses 的 stop、Anthropic 的 JSON 模式）静默丢弃。

### 冷却熔断

- **开关与参数**：可在设置页启用/停用冷却池，配置冷却时长（秒）与失败重试次数；均热加载
- **触发条件**：某供应商某模型的**所有 Key** 均失败（供应商内 Key 重试不触发冷却）；且**连续失败轮数达到「失败重试次数」**（默认 1，即一轮失败即冷却；期间任一次调用成功即清零计数）
- **效果**：冷却期内路由自动绕开该 `(供应商, 模型)` 组合；停用后失败不再入池，已有条目也不再生效
- **恢复**：到期自动恢复；也可在设置页手动移除或清空
- **监控**：设置页实时展示冷却条目与剩余时间，10 秒自动刷新

### 流式边收边转发

`stream: true` 请求边收边转发：上游数据一到即写入客户端并 Flush，客户端获得与直连一致的逐字体验。

- **同格式**：上游 SSE 字节级实时透传
- **跨格式**：上游 SSE 事件流实时转换为目标客户端格式的 SSE（解析器 + 发射器三段式流水线，3×3 组合全覆盖）
- **延迟提交与重试**：首个有效数据到达前不向客户端写任何字节，此阶段失败可重试；开始转发后不再重试
- **伪流式兼容**：上游对 `stream=true` 返回完整 JSON 时自动兜底为「解析 + 一次性合成 SSE」
- **日志与统计**：内存累积上游副本（64MB 上限），流结束后提取 usage、计算生成耗时与输出速率

---

## 项目结构

```
llmproxy/
├── main.go                      # 启动、路由组装、安全响应头、优雅关闭、前端 embed
├── go.mod / go.sum              # Go 模块与依赖
├── config.yml                   # 运行时生成/读取的配置（首次启动自动创建，已 gitignore）
├── data.db                      # 调用日志 SQLite（运行时创建，已 gitignore）
├── build.bat / build.sh         # 当前平台编译与交叉编译脚本
├── internal/
│   ├── config/                  # 配置定义、加载/原子保存、凭证自动生成、热加载轮询
│   ├── auth/                    # JWT(HS256) 签发校验、bcrypt、IP 提取与白名单匹配
│   ├── convert/                 # ★ 三格式互转（IR 中间表示 + 跨格式流式实时转换）
│   ├── core/                    # copy-on-write 运行时快照、模型解析、路由策略、冷却池
│   ├── proxy/                   # 核心转发：认证/别名替换/重试/降级/同格式透传/跨格式转换/流式边收边转发/日志写入
│   ├── logsdb/                  # SQLite 异步写入、分页筛选查询、统计聚合
│   └── api/                     # 管理接口：登录/供应商/代理 Key/设置/冷却池/日志/仪表盘/统计
├── web/                         # 原生 HTML+CSS+JS 管理界面（embed 编译进二进制）
│   ├── index.html               # 单页外壳
│   ├── style.css                # 全局样式
│   ├── api.js / ui.js / app.js  # 请求封装 / 组件工具 / 路由与登录
│   ├── views-*.js               # 仪表盘 / 供应商 / 代理 Key / 日志 / 统计 / 设置
│   ├── log-parser.js            # 日志内容解析引擎（纯函数：三格式请求/响应解析 + SSE 重组）
│   └── chart.umd.js             # Chart.js 本地文件（离线图表）
└── Requirement.md               # 完整功能规格说明书（设计与实现对照）
```

---

## 常见问题 FAQ

**Q: 修改监听端口后为什么没生效？**
监听地址/端口需重启服务生效（其余配置均为热加载）。管理界面修改时会提示「需重启服务后生效」。命令行 `-port` / `-host` 覆盖仅本次运行生效、不写回配置文件。

**Q: 热加载会覆盖我在管理界面的修改吗？**
管理界面所有修改实时写回 `config.yml`，与手工编辑的最终落盘结果一致。界面保存基于最新配置快照原子写入；与手工编辑文件几乎同时发生时，后保存的一方覆盖先保存的一方。

**Q: 客户端传的模型名和上游收到的一样吗？**
- 客户端传**真实模型名**（完全一致，含大小写）且与供应商同格式 → 请求体字节级原样转发
- 客户端传**别名**或仅大小写不同 → 仅 `model` 字段替换为真实名，其余字段原样保留
- **跨格式**调用 → 请求体经转换器重建，模型名始终为解析后的真实名
- 代理 Key 的**可用模型限制**（两档制）：白名单配真实名、客户端用别名调用同样放行（反之亦然）

**Q: 客户端调用格式和供应商接口类型不一致时会怎样？**
代理自动双向转换，无需任何额外配置。例如供应商是 Anthropic 类型，客户端照样可以用 OpenAI Chat Completions / Responses API 格式调用；反之亦然。同格式请求则保持字节级透传，零转换损耗。转换不支持的参数（如 Responses 的 stop 序列）会静默丢弃，不会报错。

**Q: 日志里出现「客户端断开」是什么原因？**
客户端主动断开了连接（取消/超时/关闭页面）。流式代理在首个数据到达前断开不会触发重试，避免无谓烧光 Key 配额；日志可直接用于排查客户端侧超时设置。若长生成任务期间仍偶发，通常是客户端自身设置了请求超时（如 60s AbortSignal），可调大其超时或调小任务规模。

**Q: 跨格式流式的 usage 统计可靠吗？**
跨格式流式时上游也以流式请求，OpenAI 类型供应商会附带 `stream_options.include_usage`（官方标准字段）；Responses / Anthropic 上游天然在流末携带 usage。个别不支持该字段的网关可能 Token 计 0；上游对 stream=true 返回完整 JSON 时自动兜底按非流式解析，usage 提取在 SSE 模式全 0 时自动回退整体 JSON 提取。

**Q: 调用详情里的流式日志怎么看？**
详情弹窗分「客户端记录 / 上游记录 / 原始数据」三个标签页：前两个标签页把请求体/响应体结构化解析（消息卡片、工具调用参数格式化、Token 用量），流式 SSE 响应自动**重组为最终结果**（与非流式同构，底部标注总帧数；原始逐帧文本在「原始数据」标签页查看）。超过 1MB 截断的数据会显示警示条并回退原文展示。

**Q: 日志库会无限增长吗？**
可控制。①设置页关闭「记录调用日志」（默认即关闭），新调用不再写入；②日志页「删除全部日志」一键清空——删除库文件及 `-wal` / `-shm` 伴生文件后原路径重建空库，磁盘空间即时释放。也可逐条删除，或定期备份后删除 `data.db`（服务停止时），或在设置页修改日志路径切换到新库。

**Q: 忘记管理员密码怎么办？**
停止服务 → 将 `config.yml` 中 `admin.password` 改为 `""` → 重启，控制台会打印自动生成的新密码。运行中手工清空该字段也会触发热加载重新生成。

**Q: 部署在 Nginx 反向代理之后，IP 白名单怎么配置？**
设置页将「IP 层级」调为 1（一层代理），此时从 `X-Forwarded-For` 首个 IP 获取客户端真实 IP；两层代理配 2，依此类推。

**Q: curl 能调通的上游，代理/测试 Key 返回 403？**
部分上游网关按 User-Agent 拦截 Go 默认客户端（`Go-http-client/1.1`），返回 403「错误请求」——与 Key、IP、X-Forwarded-For 是否正确无关。解决：设置页「默认 User-Agent」配为 curl / 浏览器等常见 UA（全局生效，热加载），或仅在需要的那家供应商自定义请求头里加 `User-Agent`（优先于全局默认）。留空则保持 Go 默认。

---

## 安全提示

- **日志数据库**：`data.db` 中以明文存储代理 Key（客户端请求头与日志字段）及请求/响应体（各组上限 1MB），请按敏感数据管理（限制文件权限、备份加密、勿随代码分发）；发往上游的认证头在日志中已脱敏（前 10 + 尾 4 字符）；无审计需要时可在设置页关闭「记录调用日志」（默认已关闭）
- **配置文件**：`config.yml` 含 bcrypt 密码哈希与 JWT secret，请勿外传；怀疑泄露时清空 `jwt.secret` 并重启重新生成（所有登录会失效）
- **监听地址**：默认 `0.0.0.0` 对外开放。仅供本机使用时建议改为 `-host 127.0.0.1` 或修改配置
- **代理 Key**：具备等同你上游账号的消费能力，请仅发给可信客户端，并按需配置 IP 白名单与有效期

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 开源。

> AI生成
