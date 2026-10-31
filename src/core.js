/**
 * 平台无关的核心逻辑：路径映射解析 + DoH 反向代理
 *
 * 该模块同时被 Cloudflare Worker（_worker.js）与
 * 腾讯 EdgeOne Pages 边缘函数（middleware.js）复用，
 * 因此只能使用 Web 标准 API（fetch / Request / Response / URL / Headers），
 * 不得引入任何 Node.js 内置模块或第三方依赖。
 */

/** 默认路径映射（可按需增删） */
export const DEFAULT_PATH_MAPPINGS = {
	'/google': {
		targetDomain: 'dns.google',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/cloudflare': {
		targetDomain: 'one.one.one.one',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/quad9': {
		targetDomain: 'dns.quad9.net',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/adguard': {
		targetDomain: 'dns.adguard-dns.com',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	// 在此继续添加映射
};

/** 未命中映射时返回的伪装 404 页面（刻意伪装成浏览器断网页） */
export const HOMEPAGE_HTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>404</title>
    <style>
        .main{
            width: 600px;
            height:417px;
            margin: 0 auto;
            margin-top: 125px;
        }
        img{
            margin-bottom: 20px;
        }
        p{
            font-size: 14px;
            color: rgb(32, 33, 36);
        }
        span{
            color:rgb(26, 115, 232);
            cursor: pointer;
        }
        span:hover{
            border-bottom: 1px solid rgb(26, 115, 232);
        }
        button{
            float: right;
            margin-top: 60px;
            background-color: #1970e5;
            color: #fff;
            border-width: 0px;
            padding:10px;
            border-radius: 10%;
            cursor: pointer;
        }
    </style>
</head>
<body>
    <div class="main">
        <img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABIAQMAAABvIyEEAAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAENJREFUeF7tzbEJACEQRNGBLeAasBCza2lLEGx0CxFGG9hBMDDxRy/72O9FMnIFapGylsu1fgoBdkXfUHLrQgdfrlJN1BdYBjQQm3UAAAAASUVORK5CYII=" alt="">
        <p style="font-size:24px;">无法访问此网站</p>
        <p>检查是否有拼写错误。</p>
        <p>如果拼写无误，请<span>尝试运行 Windos 网络诊断</span>。</p>
        <p style="font-size:12px;">DNS_PROBE_FINSHED_NXDOMAIN</p>
        <button>重新加载</button>
    </div>

</body>
</html>`;

/** 需要剥离的请求头（由运行时或目标域名自行决定） */
const DROPPED_REQUEST_HEADERS = ['host', 'content-length'];

/**
 * 读取路径映射配置
 * 优先使用环境变量 DOMAIN_MAPPINGS（JSON 字符串或对象），否则回退默认值
 * @param {Object} [env] 平台环境变量对象（Cloudflare env / EdgeOne context.env）
 * @returns {Object} 路径映射配置
 */
export function getPathMappings(env) {
	try {
		if (env && env.DOMAIN_MAPPINGS) {
			if (typeof env.DOMAIN_MAPPINGS === 'string') {
				return JSON.parse(env.DOMAIN_MAPPINGS);
			}
			return env.DOMAIN_MAPPINGS;
		}
	} catch (error) {
		console.error('Error accessing DOMAIN_MAPPINGS variable:', error);
	}

	return DEFAULT_PATH_MAPPINGS;
}

/**
 * 根据请求 URL 解析转发目标
 * @param {URL} url 原始请求 URL
 * @param {Object} [env] 平台环境变量对象
 * @returns {{prefix: string, targetDomain: string, targetUrl: string}|null} 未命中返回 null
 */
export function resolveTarget(url, env) {
	const path = url.pathname;
	const pathMappings = getPathMappings(env);

	// 最长前缀优先，避免 '/google' 抢先匹配 '/google-v2'
	const pathPrefix = Object.keys(pathMappings)
		.filter((prefix) => path.startsWith(prefix))
		.sort((a, b) => b.length - a.length)[0];

	if (!pathPrefix) {
		return null;
	}

	const mapping = pathMappings[pathPrefix] || {};
	if (!mapping.targetDomain) {
		return null;
	}

	const remainingPath = path.substring(pathPrefix.length);
	const pathMapping = mapping.pathMapping || {};

	let targetPath = remainingPath;
	for (const [sourcePath, destPath] of Object.entries(pathMapping)) {
		if (remainingPath.startsWith(sourcePath)) {
			targetPath = remainingPath.replace(sourcePath, destPath);
			break;
		}
	}

	// 前缀后无剩余路径时补根路径，避免拼出 https://dns.google?name=...
	if (!targetPath) {
		targetPath = '/';
	}

	return {
		prefix: pathPrefix,
		targetDomain: mapping.targetDomain,
		targetUrl: `https://${mapping.targetDomain}${targetPath}${url.search}`,
	};
}

/**
 * 构造伪装 404 响应
 * @returns {Response}
 */
export function camoResponse() {
	return new Response(HOMEPAGE_HTML, {
		status: 404,
		headers: { 'Content-Type': 'text/html; charset=utf-8' },
	});
}

/**
 * 复制请求头，剥离由运行时接管或会引起歧义的头部
 * @param {Headers} headers
 * @returns {Headers}
 */
function forwardableHeaders(headers) {
	const result = new Headers();
	headers.forEach((value, key) => {
		if (!DROPPED_REQUEST_HEADERS.includes(key.toLowerCase())) {
			result.set(key, value);
		}
	});
	return result;
}

/**
 * 核心代理：命中映射则转发到目标 DoH 服务，否则返回伪装 404
 * @param {Request} request 原始请求
 * @param {Object} [options]
 * @param {Object} [options.env] 平台环境变量对象
 * @param {Function} [options.fetchImpl] 自定义 fetch 实现（便于测试）
 * @returns {Promise<Response>}
 */
export async function proxyRequest(request, options = {}) {
	const { env, fetchImpl = fetch } = options;
	const url = new URL(request.url);
	const target = resolveTarget(url, env);

	if (!target) {
		return camoResponse();
	}

	const method = (request.method || 'GET').toUpperCase();
	const init = {
		method,
		headers: forwardableHeaders(request.headers),
		redirect: 'follow',
	};

	// GET/HEAD 无请求体；其余方法统一读取为 ArrayBuffer，兼容各 V8 运行时对流式 body 的差异
	if (method !== 'GET' && method !== 'HEAD') {
		const body = await request.arrayBuffer();
		if (body && body.byteLength > 0) {
			init.body = body;
		}
	}

	return fetchImpl(target.targetUrl, init);
}

export default {
	resolveTarget,
	getPathMappings,
	proxyRequest,
	camoResponse,
	HOMEPAGE_HTML,
	DEFAULT_PATH_MAPPINGS,
};
