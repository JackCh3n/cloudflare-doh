/**
 * Cloudflare Workers / Cloudflare Pages 入口
 *
 * 仅做平台适配置，路由与转发逻辑全部在 src/core.js 中，
 * 便于与其他平台（如腾讯 EdgeOne）共享同一份实现。
 */

import { proxyRequest } from './src/core.js';

export default {
	async fetch(request, env, ctx) {
		return proxyRequest(request, { env });
	},
};
