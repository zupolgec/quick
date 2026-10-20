// quick-cli.com: static assets, plus Markdown for agents that ask for it.
// GET /docs/cli with "Accept: text/markdown" returns /docs/cli.md.
const mdPath = (path) => {
  if (path === '/') return '/index.md';
  if (path === '/docs' || path === '/docs/') return '/docs/index.md';
  if (path.startsWith('/docs/') && !path.includes('.')) return path.replace(/\/$/, '') + '.md';
  return null;
};

const wantsMarkdown = (accept) => {
  if (!accept) return false;
  const types = accept.split(',').map((p) => {
    const [type, ...params] = p.trim().split(';');
    const q = params.map((s) => s.trim()).find((s) => s.startsWith('q='));
    return { type: type.trim().toLowerCase(), q: q ? parseFloat(q.slice(2)) : 1 };
  });
  const q = (t) => Math.max(0, ...types.filter((x) => x.type === t).map((x) => x.q));
  return q('text/markdown') > 0 && q('text/markdown') >= q('text/html');
};

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const md = mdPath(url.pathname);
    if (md && (request.method === 'GET' || request.method === 'HEAD') && wantsMarkdown(request.headers.get('Accept'))) {
      const res = await env.ASSETS.fetch(new URL(md, url));
      if (res.ok) {
        const headers = new Headers(res.headers);
        headers.set('Content-Type', 'text/markdown; charset=utf-8');
        headers.set('Vary', 'Accept');
        headers.set('Content-Location', md);
        return new Response(res.body, { status: 200, headers });
      }
    }
    const res = await env.ASSETS.fetch(request);
    if (!md) return res;
    const headers = new Headers(res.headers);
    headers.append('Vary', 'Accept');
    headers.set('Link', `<${md}>; rel="alternate"; type="text/markdown", </llms.txt>; rel="describedby"; type="text/plain"`);
    return new Response(res.body, { status: res.status, statusText: res.statusText, headers });
  },
};
