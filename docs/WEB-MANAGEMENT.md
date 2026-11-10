# bps-server 管理版使用说明

`bps-server` 是 `bps-local` 的多账号管理版：在保留单账号桥接能力的同时，提供 Web 管理控制台、多账号设备授权与调度、API Key 管理和用量统计。设计文档见 [WEB-MANAGEMENT-IMPLEMENTATION-PLAN.md](WEB-MANAGEMENT-IMPLEMENTATION-PLAN.md)。

## 1. 端口与角色

| 端口（默认） | 用途 | 鉴权 |
|---|---|---|
| `8790` | Responses API（`POST /v1/responses`、`POST /responses`、`GET /v1/models`、`GET /healthz`） | `Authorization: Bearer sk-bps-…` |
| `8791` | 管理控制台与管理 API（`/admin/`） | 管理员密码登录 + Session |

## 2. 本地运行（Windows/Linux 均可）

```bash
go build -o bps-server ./cmd/bps-server
mkdir -p secrets
# Windows PowerShell 生成密码文件：
New-Item -ItemType Directory -Force secrets | Out-Null
-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }) | Set-Content -NoNewline secrets/admin_password
```

```bash
# Linux / macOS
mkdir -p secrets && openssl rand -base64 36 > secrets/admin_password && chmod 600 secrets/admin_password
```

启动（Windows PowerShell 示例）：

```powershell
$env:BPS_DATA_DIR = "./data"
$env:BPS_API_ADDR = "127.0.0.1:8790"
$env:BPS_ADMIN_ADDR = "127.0.0.1:8791"
$env:BPS_ADMIN_PASSWORD_FILE = "./secrets/admin_password"
.\bps-server.exe
```

浏览器打开 `http://127.0.0.1:8791/admin/`，用 `secrets/admin_password` 中的密码登录。

## 3. Docker Compose 部署

```bash
mkdir -p secrets
openssl rand -base64 36 > secrets/admin_password
chmod 600 secrets/admin_password
docker compose config --quiet
docker compose build
docker compose up -d
```

宿主机默认仅从 `127.0.0.1:8790`（API）与 `127.0.0.1:8791`（管理台）访问容器。远程访问必须增加 HTTPS 反向代理，并把 `BPS_COOKIE_SECURE` 设为 `true`。

数据持久化在命名卷 `bps-data` 中：`/data/bps.db`（SQLite）与 `/data/accounts/`（每账号 OAuth 文件）。

容器内访问宿主机代理时，`BPS_LOCAL_PROXY` 不能写 `127.0.0.1`：Docker Desktop 使用 `host.docker.internal`，Linux 需按网络环境使用宿主地址或代理服务名。

## 4. 多账号授权

1. 控制台 →「账号」→「新增账号」，输入备注名称。
2. 弹出的授权面板给出官方授权地址与用户码；**授权地址已自动带入用户码**，直接点「打开授权页」即可，登录后按页面提示确认授权。
3. 授权成功后账号进入「已禁用」状态，在列表中点「启用」后参与调度。
4. Token 过期或上游返回 401 时，账号状态变为「需重新授权」，点「重新授权」生成新凭据文件（不覆盖旧文件）。

> 若授权页显示 `deviceauth_disabled`，通常是打开了**未携带 user_code** 的旧授权地址。请重新在面板中点击「打开授权页」使用当前链接；升级前生成的链接需要重新发起授权任务。

### 授权页报 `deviceauth_disabled` 的排查

浏览器授权页出现「糟糕，出错了！」且 `error_code: deviceauth_disabled` 时，说明**该 ChatGPT 账号关闭了「设备代码登录」**，不是本程序或网络的问题：

1. 打开 ChatGPT，进入 **设置 → 安全**，开启「设备代码登录」。
2. 工作区（Workspace）账号需要管理员在工作区权限中开启该选项。
3. 回到本程序授权面板，重新点「打开授权页」（旧用户码可能已过期，必要时重新发起授权）。

开启该设置只解决登录限制，不保证 BPS 权限或模型可用性。设备码申请与轮询接口本身不受影响，服务端会继续等待至 15 分钟超时。

同一上游账号不允许重复添加。授权任务最长等待 15 分钟，可随时取消。

### 4.1 账号测试

账号列表每行有「测试」按钮，点开弹窗后：

1. **模型**下拉自动加载该账号的上游模型目录（来自 `access?include_models=true`），默认选中账号的默认模型。
2. **提示词**默认 `hi`，可自由修改。
3. 点「发送」发起一次真实生成，结果区展示：返回模型、耗时、输入/输出/合计 token、以及上游真实输出文本（如有推理摘要也会一并显示）。
4. **每次点击「发送」都会先清空上一次结果**，再显示本次结果。
5. 若请求模型与上游实际返回模型不一致（例如 GPT-6 被降级），界面会单独提示。

该测试绕过 API Key 与调度器，直接使用该账号自己的运行时，因此不会影响其他账号的调度状态，也不会产生 API Key 维度的用量记录（但会消耗该账号的上游额度）。

**删除账号**：账号列表每行有「删除」按钮。删除会移除账号记录、磁盘上该账号的全部授权凭据文件，并立即停止其调度；已开始的在途请求会在旧连接上完成。历史用量记录保留，但不再关联该账号（避免破坏统计与违反外键）。删除不可恢复，操作前有二次确认。

## 5. 负载均衡

调度采用**加权最少在途请求**：每次新请求选择 `在途请求数 / 权重` 最小的账号。权重 1-100，默认 1。可用「权重」按钮调整。

- 账号禁用、冷却中或需重新授权时不参与新请求；已开始的流不受影响。
- 上游 429 按 `Retry-After` 冷却；网络类错误按 5 秒起指数退避，上限 5 分钟。
- 请求头携带 `session_id` 时，15 分钟内优先固定在同一账号（软粘性）。
- 任何上游失败都不会自动重发请求，避免重复生成与重复工具调用。

## 6. API Key

- 「API Key」页创建，完整 Key 只在创建时显示一次。
- 客户端调用方式：

```bash
curl -N http://127.0.0.1:8790/v1/responses \
  -H "Authorization: Bearer ${BPS_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-astra","input":"只回复 OK","stream":true,"reasoning":{"effort":"medium"}}'
```

- 禁用立即阻止新请求；撤销不可逆。模型是否可用取决于被选中账号的权限。

**删除 Key**：与「撤销」不同，删除会永久移除 Key 记录（不只是标记失效）。删除后该 Key 立即无法通过鉴权；历史用量记录保留，但不再关联该 Key。操作前有二次确认。

**复制 Key**：列表只显示脱敏前缀（如 `sk-bps-Ab3dEf9x…`）。每行「复制」按钮会向服务端按需索取明文并写入剪贴板，明文不会出现在页面、列表响应或日志中。

明文采用 AES-256-GCM 加密后存入 `api_keys.key_sealed`，密钥文件 `api_key_secret` 独立存放在数据目录（权限 `0600`），不随数据库、镜像或 Git 分发。因此**仅有数据库副本无法还原任何 Key**。

服务端通过 `POST /admin/api/api-keys/{id}/reveal` 返回明文，响应带 `Cache-Control: no-store`，且仅接受 POST——链接、预取或跨站导航都无法取出 Key。

启用加密存储之前创建的 Key 没有密文，点「复制」会提示无法恢复，需重新创建并保存。

### 6.1 查询可用模型（`GET /v1/models`）
用任意有效 API Key 调用，返回**所选账号的真实上游模型目录**（OpenAI 兼容格式）：

```bash
curl -s http://127.0.0.1:8790/v1/models \
  -H "Authorization: Bearer ${BPS_API_KEY}"
```

响应包含 `data[].id`、`label`、`default_effort`、`efforts`（可选推理强度），以及 `default_model`、`plan_type`、`source`、`restricted_models`。

模型列表随账号计划变化，因此不做本地硬编码或长期缓存；每次请求都会向当前可调度账号的上游读取。无可用账号时返回 503 `no_account_available`。

### 6.2 内联图片（base64）支持

上游 BPS 只能抓取 HTTPS 图片 URL，不接受 `data:image/...;base64` 内联数据。启用图片中继后，本服务会自动把内联图片暂存为临时 URL，客户端可以直接发 base64：

```json
{
  "model": "gpt-5.6-luna",
  "input": [{"role": "user", "content": [
    {"type": "input_text", "text": "这张图里有什么？"},
    {"type": "input_image", "image_url": "data:image/png;base64,iVBORw0..."}
  ]}],
  "stream": true
}
```

**启用条件**：设置 `BPS_PUBLIC_BASE_URL` 为上游可访问的 HTTPS 源（例如 `https://bps.codeai.de5.net`）。留空则保持原行为——内联图片返回 400。

**工作方式**：请求体中的 data URL 被替换为 `{BPS_PUBLIC_BASE_URL}/api/bps-images/{token}`，上游按该地址抓取。该地址免鉴权（上游不带 API Key），令牌为不可枚举的 HMAC，10 分钟后失效。

**资源上限**（防止高并发下磁盘被撑满）：

| 限制 | 值 |
|---|---|
| 暂存总预算 | 256 MiB |
| 条目数 | 512 |
| 单图 | 20 MiB / 64 百万像素 |
| 单请求图片总量 | 32 MiB |
| 单请求图片数 | 128 |
| 保留时间 | 10 分钟 |

暂存文件位于数据目录的 `image-relay/` 下，按 TTL 自动清理；进程异常退出后，下次启动会清理超过 TTL 的残留目录。

**容量满时的行为**：按「最久未被访问」淘汰旧图片腾出空间，新请求正常执行，不会因容量满而失败。正在被上游下载的图片不会被淘汰（否则会打断进行中的请求）；仅当所有条目都在下载中时，新请求才返回 503 `basispoints_image_relay_unavailable`。淘汰次数可在总览页「累计淘汰」卡片查看。

## 7. 用量统计

- 「用量」页支持按时间、账号、Key、模型筛选，按天/账号/Key/模型分组汇总。
- Token 数来自上游终端事件中的 `usage`；上游未上报时显示「未上报」，不会计为 0。
- HTTP 200 不等于成功：统计以终端事件（completed/incomplete/failed）与错误分类为准。
- 原始请求记录默认保留 30 天（`BPS_USAGE_RETENTION_DAYS`），每日自动清理。

## 8. 网络代理设置

管理台「设置」页可在线配置出站代理，**保存后立即生效，无需重启**：

- 支持 `http`、`https`、`socks5`、`socks5h`，可带 `user:pass@` 认证；留空表示直连。
- 「检测代理」会先验证代理主机 TCP 可达，再经代理实际访问 BPS 上游（`https://bps.openai.com/...`）。返回 HTTP 401 属预期（检测请求不带凭据），说明链路完全打通。
- 检测结果区分阶段：`dial`（代理连不上）、`tunnel`（代理可连但访问上游失败）、`config`（地址格式错误）。
- 保存后，所有账号运行时会被重建，**后续所有上游调用（BPS 生成与账号设备授权）立即改走新代理**；在途请求继续在旧连接上完成。
- 设置持久化在 SQLite `settings` 表，重启后保留。首次启动若未设置过，会采用 `BPS_LOCAL_PROXY` 环境变量并落库。
- 代理凭据不会出现在管理接口响应与日志中（显示为脱敏形式）。

## 9. 配置项

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `BPS_DATA_DIR` | `/data` | 数据库与凭据目录 |
| `BPS_API_ADDR` | `0.0.0.0:8790` | API 监听地址 |
| `BPS_ADMIN_ADDR` | `0.0.0.0:8791` | 管理台监听地址 |
| `BPS_LOCAL_PROXY` | 空 | 上游代理（http/https/socks5/socks5h） |
| `BPS_UPSTREAM_TRANSPORT` | `ws` | 上游传输（ws 或 sse） |
| `BPS_WS_IDLE_TIMEOUT` | `2m` | 上游事件等待上限 |
| `BPS_ADMIN_PASSWORD_FILE` | `/run/secrets/admin_password` | 管理员密码文件 |
| `BPS_COOKIE_SECURE` | `false` | HTTPS 部署必须设为 true |
| `BPS_SESSION_TTL` | `12h` | 管理会话有效期 |
| `BPS_USAGE_RETENTION_DAYS` | `30` | 请求记录保留天数 |

## 10. 安全注意事项

- 管理台仅限可信网络访问；公网部署必须加 HTTPS 与访问控制。
- 管理员密码只从文件读取，不落库、不进日志；登录失败 10 次锁定 5 分钟。
- API Key 只存 SHA-256 哈希；OAuth Token 只存文件，数据库仅存元数据。
- 会话与授权任务保存在内存，重启后管理员需重新登录，未完成的授权需重新发起。

## 11. 备份与恢复

WAL 模式下不能只复制 `bps.db` 单文件。使用在线备份：

```bash
docker run --rm -v bps-local-public_bps-data:/data alpine sh -c \
  "apk add sqlite >/dev/null && sqlite3 /data/bps.db '.backup /data/bps-backup.db'"
```

PowerShell 中调用时注意引号层级，推荐用单引号包裹 sqlite 参数（如上），避免内嵌转义失败导致 `.backup` 被拆错。

或停止容器后整体复制卷内容（含 `/data/accounts`）。恢复时替换文件并重启容器。备份文件属主是执行备份的容器用户（root）；恢复到 `/data/bps.db` 时注意保持属主为 `bps`（uid 10001），否则服务可能无法写入。

## 12. 与独立 CLI 的关系

`cmd/bps-local` 保持原有单账号行为，可作为管理版故障时的备用路径。两者的 OAuth 文件互相独立，不会互相读取。

### 6.3 模型目录同步与模型映射

**模型目录持久化**

管理台「模型」页展示上游模型列表，该列表**持久化保存**，并且就是 `GET /v1/models` 返回的内容。

点击「同步上游最新」会向当前可调度账号读取一次上游目录并落库。同步失败时保留上一次的快照，不会清空已有列表。

从未同步过时，`/v1/models` 返回 503 `model_catalog_unavailable`，提示先到管理台同步。

**模型映射**

在「模型映射」区配置 `源模型 → 目标模型`：请求命中配置时，发往上游的 `model` 会被替换为目标模型；未配置的模型原样透传。

- 下拉选项来自已同步的目录
- 同一个源模型只能配置一条映射
- 源与目标相同会被忽略（等价于不映射）
- 目标模型不在当前目录时会标注提示，但仍允许保存（上游模型可能变动）
- 保存后立即生效，无需重启

用量页的「请求模型 / 上报模型」两列可直接看出映射效果，例如请求 `gpt-6-astra`、上报 `gpt-5.6-sol`。
