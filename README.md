# bps-local

将本机的 Responses API 请求转换为 ChatGPT Excel / Basispoints（BPS）请求的 Go 命令行桥接器。客户端通过 HTTP 发送请求、通过 SSE 接收事件；桥接器默认通过 WebSocket 连接 BPS 上游。

```text
Responses 客户端 → http://127.0.0.1:8790/v1/responses → BPS WebSocket
```

本目录是 **2026-11-06 提交 `8736c9f` 的独立 CLI 源码快照**，不包含 CPA 插件或管理服务。BPS 属于产品内部接口，本项目并非 OpenAI 官方项目；接口、授权方式和模型权限可能变化，代码可构建不代表目前的上游请求一定成功。

## 快速开始

安装 Go 1.26.0 或更高版本，进入本目录构建：

```bash
go build -o bps-local ./cmd/bps-local
```

以下示例使用本机 `7890` 代理端口，请替换成自己的代理地址。先完成独立 BPS 授权：

```bash
./bps-local --login --proxy http://127.0.0.1:7890
```

浏览器打开后，由你完成登录与授权。默认保存到 `~/.bps-local/oauth.json`；若文件已存在，程序拒绝覆盖，可用 `--auth` 指定另一个文件。程序不会自动读取或修改 Codex 的授权文件。

启动桥接器，保持该终端运行：

```bash
./bps-local --proxy http://127.0.0.1:7890
```

在另一终端调用本地接口。将 `YOUR_MODEL` 换成该账号实际有权使用的模型：

```bash
curl -N http://127.0.0.1:8790/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"YOUR_MODEL","input":"只回复 OK","stream":true,"reasoning":{"effort":"medium"}}'
```

本地接口仅监听回环地址，不要求客户端提供 API Key。它返回流式事件；应检查最终事件和内容，HTTP 连接成功不等于生成成功。

## 使用范围

- 支持 Responses 文本输入、客户端工具调用转换，以及上游事件到本地 SSE 的转换。
- 每个进程使用一份独立 OAuth 文件；没有多账号管理界面、账号轮询或模型列表接口。
- 多轮上下文由调用方在每次请求中提供；请求负载由调用方控制。
- 独立 CLI 的图片输入使用 HTTPS 图片 URL；不提供内联图片的公开托管地址。
- 不执行 Excel 工作簿操作，不提供上游托管的搜索、文件搜索或图像生成工具。
- OAuth 文件可保存 Access Token、Refresh Token 和有效期等元数据，但 **CLI 不自动使用 Refresh Token 刷新授权**。登录完成和 `--check` 均不验证真实生成权限。

Windows 构建使用 `go build -o bps-local.exe ./cmd/bps-local`。附带的启动脚本默认代理是 `12450`，使用 `7890` 时请显式覆盖：

```powershell
.\bps-local.exe --login --proxy http://127.0.0.1:7890
.\bps.ps1 -Proxy http://127.0.0.1:7890
```

完整参数、上下文示例及故障排查见 [使用说明](docs/USAGE.md)。`--upstream-transport sse` 是源码保留的旧兼容选项，日常使用保持默认 `ws`。

## 多账号管理版

`cmd/bps-server` 提供多账号管理版：Web 控制台、多账号设备授权与负载均衡、API Key 管理和用量统计，支持 Docker Compose 部署。部署与使用见 [管理版说明](docs/WEB-MANAGEMENT.md)。

```bash
go build -o bps-server ./cmd/bps-server
```

## 许可与来源

项目许可、第三方声明及依赖许可原文统一见根目录 [LICENSE](LICENSE)。

## 社区

[LINUX DO](https://linux.do/) · 项目发布与交流。
