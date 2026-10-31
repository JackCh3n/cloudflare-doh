/**
 * 腾讯 EdgeOne Pages 入口（边缘中间件 / Middleware）
 *
 * 与 Cloudflare 版共用 src/core.js 中的路由与转发逻辑：
 *   - 命中路径映射 → 以 fetch 反向代理到对应 DoH 服务（保留查询串与方法体）
 *   - 未命中 → 返回伪装 404 页面
 *
 * 部署：edgeone makers deploy -n <项目名>
 * 本地调试：edgeone makers dev（http://127.0.0.1:8088/）
 */

import { proxyRequest } from './src/core.js';

/**
 * EdgeOne Pages 中间件（V8 运行时，无 Node.js 内置模块）
 * @param {Object} context EdgeOne 中间件上下文，含 request / env 等
 * @returns {Promise<Response>}
 */
export function middleware(context) {
	return proxyRequest(context.request, { env: context.env });
}

export default middleware;
