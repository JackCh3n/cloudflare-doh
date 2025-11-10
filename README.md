# Cloudflare DoH 转发代理

![使用演示](https://i.imgur.com/hu6F10P.gif)

这是一个基于 Cloudflare Workers 的 DNS over HTTPS (DoH) 转发代理服务。本服务可以根据路径将请求转发到不同的 DoH 提供商，同时保留查询参数。

## 功能特点

- 基于路径的请求转发：根据请求路径将请求转发到对应的 DoH 服务提供商
- 自定义路径映射：可以通过 Cloudflare Worker 的环境变量配置路径映射
- 保留查询参数：转发时会保留原始请求中的查询参数
- 轻量级实现：简单高效的实现方式，易于部署和维护

## 工作原理

该 Worker 根据请求的路径前缀确定转发目标，然后将请求转发到相应的 DoH 服务提供商。例如，当访问 `doh.example.com/google/query-dns?name=example.com` 时，该请求会被转发到 `dns.google/dns-query?name=example.com`。

### 默认路径映射

Worker 内置了以下默认映射规则：

- `/google/query-dns` → `dns.google/dns-query`（Google 的 DoH 服务）
- `/cloudflare/query-dns` → `one.one.one.one/dns-query`（Cloudflare 的 DoH 服务）

## 配置说明

### 基础配置

Worker 可以使用默认配置直接部署使用。

### 自定义配置

可以在 Cloudflare Workers 控制台中添加名为 `DOMAIN_MAPPINGS` 的环境变量来自定义路径映射规则。该变量接受符合以下格式的 JSON 字符串：

```json
{
	"/path-prefix": {
		"targetDomain": "target.domain.com",
		"pathMapping": {
			"/source-path": "/target-path"
		}
	}
}
```

例如，若要添加对 Quad9 DoH 服务的支持，配置可能如下：

```json
{
	"/google": {
		"targetDomain": "dns.google",
		"pathMapping": {
			"/query-dns": "/dns-query"
		}
	},
	"/cloudflare": {
		"targetDomain": "one.one.one.one",
		"pathMapping": {
			"/query-dns": "/dns-query"
		}
	},
	"/quad9": {
		"targetDomain": "dns.quad9.net",
		"pathMapping": {
			"/query-dns": "/dns-query"
		}
	}
}
```

## 部署方法

### 方法一：使用 Cloudflare Workers

1. 登录到 [Cloudflare 控制台](https://dash.cloudflare.com/)
1. 进入 Workers and Pages, 点击"创建"
1. 选择 Worker, 输入服务名称并选择"Hello World"模板
1. 将 `_worker.js` 中的代码粘贴到编辑器中
1. (可选) 在"变量和机密"部分添加 `DOMAIN_MAPPINGS` 变量来自定义路径映射
1. 点击"部署"按钮

### 方法二：使用 Cloudflare Pages

1. Fork 本库
1. 登录到 [Cloudflare 控制台](https://dash.cloudflare.com/)
1. 进入 Workers and Pages, 点击"创建"
1. 选择 Pages, "连接到 Git"，并连接到您的 Fork 库
1. （可选）在"变量和机密"部分添加 `DOMAIN_MAPPINGS` 变量来自定义路径映射
1. 点击"保存并部署"

部署完成后，Cloudflare Pages 会自动检测 `_worker.js` 文件并将其用作 Worker 函数。

## 使用示例

假设您已将此 Worker 部署到 `doh-proxy.workers.dev`，您可以通过以下方式使用：

- 使用 Google 的 DoH 服务：

  ```
  https://doh-proxy.workers.dev/google/query-dns?name=example.com
  ```

- 使用 Cloudflare 的 DoH 服务：
  ```
  https://doh-proxy.workers.dev/cloudflare/query-dns?name=example.com
  ```

## 注意事项

- 该服务仅转发请求，不会修改或存储您的 DNS 查询内容
- 请确保遵守各 DoH 服务提供商的使用政策
- 此服务适合个人或小规模使用，对于大规模部署，请考虑各提供商的使用限制
- Cloudflare 免费版用户每日的免费请求数量为 **10w** 次, 仅够个人使用, 注意避免暴漏 DoH 连接
- 关闭 Cloudflare 代理可以大幅降低延迟, DoH 服务通常不需要代理

## 许可协议

本项目采用 [MIT 许可协议](LICENSE)。您可以自由地使用、修改和分发本代码，但需要在您的项目中包含原始许可证和版权声明。

## 赞助商

[宁屏去广告](https://www.nullprivate.com) - 可定制化个人 DNS 解析服务

主要特性:

- 智能广告及追踪器过滤
- 内置DDNS 支持
- 防沉迷配置
- 支持按规则路由上游DNS

详情请访问 [Null Private](https://www.nullprivate.com) 了解更多。

