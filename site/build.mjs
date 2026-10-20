// Builds dist/ for quick-cli.com: the static homepage, the docs rendered from
// content/docs/*.md, a Markdown twin of every page, llms.txt, llms-full.txt,
// skill.md (from the CLI itself) and the install scripts (from the server).
import { cp, mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { Marked } from 'marked';

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, '..');
const dist = join(here, 'dist');
const site = 'https://quick-cli.com';

// Sidebar order. Every file in content/docs must be listed here.
const order = ['index', 'cli', 'sites', 'access', 'ci-and-agents', 'custom-domains', 'self-hosting'];

const parse = (src) => {
  const m = src.match(/^---\n([\s\S]*?)\n---\n/);
  const meta = {};
  for (const line of (m ? m[1] : '').split('\n')) {
    const i = line.indexOf(':');
    if (i > 0) meta[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { meta, body: m ? src.slice(m[0].length) : src };
};

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const slugify = (s) => s.toLowerCase().replace(/<[^>]+>/g, '').replace(/&[a-z]+;/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
const urlOf = (slug) => (slug === 'index' ? '/docs/' : `/docs/${slug}`);
const mdUrlOf = (slug) => `/docs/${slug}.md`;

const marked = new Marked({
  gfm: true,
  renderer: {
    heading({ tokens, depth }) {
      const text = this.parser.parseInline(tokens);
      if (depth === 1) return `<h1>${text}</h1>\n`;
      const id = slugify(text);
      return `<h${depth} id="${id}"><a class="anchor" href="#${id}">${text}</a></h${depth}>\n`;
    },
    code({ text, lang }) {
      const label = lang ? ` data-lang="${esc(lang)}"` : '';
      return `<div class="code"${label}><pre><code>${esc(text)}</code></pre><button class="copy" type="button">Copy</button></div>\n`;
    },
  },
});

const page = ({ slug, meta, html, docs }) => {
  const toc = [...html.matchAll(/<h2 id="([^"]+)"><a class="anchor" href="#[^"]+">(.*?)<\/a><\/h2>/g)];
  const i = order.indexOf(slug);
  const prev = docs[i - 1];
  const next = docs[i + 1];
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(meta.title)} · quick docs</title>
<meta name="description" content="${esc(meta.description)}">
<link rel="canonical" href="${site}${urlOf(slug)}">
<link rel="alternate" type="text/markdown" href="${mdUrlOf(slug)}" title="This page as Markdown">
<meta property="og:title" content="${esc(meta.title)} · quick docs">
<meta property="og:description" content="${esc(meta.description)}">
<meta name="theme-color" content="#0e1018">
<link rel="icon" href="/favicon.png" type="image/png">
<link rel="preload" href="/fonts/hanken-grotesk-latin.woff2" as="font" type="font/woff2" crossorigin>
<link rel="stylesheet" href="/docs.css">
</head>
<body class="docs">
<header class="docs-top">
  <a class="docs-mark" href="/" aria-label="quick home">${logo}</a>
  <nav aria-label="Site">
    <a class="link" href="/docs/">Docs</a>
    <a class="link" href="/llms.txt">llms.txt</a>
    <a class="link" href="https://github.com/zupolgec/quick">GitHub</a>
  </nav>
</header>
<div class="docs-body">
  <nav class="docs-nav" aria-label="Docs">
    <ul>
${docs.map((d) => `      <li><a href="${urlOf(d.slug)}"${d.slug === slug ? ' aria-current="page"' : ''}>${esc(d.meta.title)}</a></li>`).join('\n')}
    </ul>
  </nav>
  <main class="prose">
${html}
    <p class="page-md"><a class="link-plain" href="${mdUrlOf(slug)}">This page as Markdown</a></p>
    <nav class="pager" aria-label="Previous and next">
      ${prev ? `<a href="${urlOf(prev.slug)}"><span>Previous</span>${esc(prev.meta.title)}</a>` : '<span></span>'}
      ${next ? `<a href="${urlOf(next.slug)}"><span>Next</span>${esc(next.meta.title)}</a>` : '<span></span>'}
    </nav>
  </main>
  ${toc.length > 1 ? `<nav class="docs-toc" aria-label="On this page">
    <p>On this page</p>
    <ul>
${toc.map(([, id, t]) => `      <li><a href="#${id}">${t}</a></li>`).join('\n')}
    </ul>
  </nav>` : ''}
</div>
<script src="/docs.js" defer></script>
</body>
</html>
`;
};

const logo = (await readFile(join(here, 'content/logo.svg'), 'utf8')).trim();

await rm(dist, { recursive: true, force: true });
await cp(join(here, 'public'), dist, { recursive: true });
// The product logo, inlined so it can take the colour of each field.
for (const f of ['index.html', '404.html']) {
  const p = join(dist, f);
  await writeFile(p, (await readFile(p, 'utf8')).replaceAll('{{logo}}', logo));
}
await mkdir(join(dist, 'docs'), { recursive: true });

// The grotesk, self-hosted: the same file the quick server embeds.
await mkdir(join(dist, 'fonts'), { recursive: true });
await cp(join(repo, 'cmd/quick-server/fonts/hanken-grotesk-latin.woff2'), join(dist, 'fonts/hanken-grotesk-latin.woff2'));

// Install scripts: the same files every quick server serves.
await cp(join(repo, 'cmd/quick-server/install/install.sh'), join(dist, 'install.sh'));
await cp(join(repo, 'cmd/quick-server/install/install.ps1'), join(dist, 'install.ps1'));

// skill.md: written by the CLI itself, so it never drifts from what `quick skill` installs.
const skillDir = join(tmpdir(), `quick-skill-${process.pid}`);
execFileSync('go', ['run', './cmd/quick', 'skill', '--dir', skillDir], { cwd: repo, stdio: 'ignore', env: { ...process.env, QUICK_NO_SKILL_AUTOUPDATE: '1' } });
await cp(join(skillDir, 'SKILL.md'), join(dist, 'skill.md'));
await rm(skillDir, { recursive: true, force: true });

// Docs.
const files = (await readdir(join(here, 'content/docs'))).filter((f) => f.endsWith('.md')).map((f) => f.slice(0, -3));
const missing = files.filter((f) => !order.includes(f));
if (missing.length) throw new Error(`add to the docs order: ${missing.join(', ')}`);
const docs = [];
for (const slug of order) {
  const src = await readFile(join(here, 'content/docs', `${slug}.md`), 'utf8');
  docs.push({ slug, src, ...parse(src) });
}
for (const d of docs) {
  const html = marked.parse(d.body).replace(/<table>/g, '<div class="table"><table>').replace(/<\/table>/g, '</table></div>');
  const out = d.slug === 'index' ? join(dist, 'docs/index.html') : join(dist, 'docs', `${d.slug}.html`);
  await writeFile(out, page({ ...d, html, docs }));
  await writeFile(join(dist, 'docs', `${d.slug}.md`), d.body.trimStart());
}

// Homepage twin.
const home = parse(await readFile(join(here, 'content/index.md'), 'utf8'));
await writeFile(join(dist, 'index.md'), home.body.trimStart());

// llms.txt (https://llmstxt.org) and llms-full.txt.
const llms = `# quick

> ${home.meta.description}

quick is a CLI (\`quick\`) and a server (\`quick-server\`). Users run \`quick login --server <their company's server>\` once, then \`quick deploy <site> <folder>\`. Every page below is Markdown; HTML pages also answer \`Accept: text/markdown\`.

## Docs

${docs.map((d) => `- [${d.meta.title}](${site}${mdUrlOf(d.slug)}): ${d.meta.description}`).join('\n')}

## Agents

- [Agent skill](${site}/skill.md): the SKILL.md installed by \`quick skill\`, for Claude Code, Codex, Gemini, Cursor and others
- [Install script](${site}/install.sh): \`curl -fsSL ${site}/install.sh | sh\` (Windows: \`irm ${site}/install.ps1 | iex\`)

## Optional

- [All docs in one file](${site}/llms-full.txt)
- [Source code](https://github.com/zupolgec/quick): Go, MIT licence
`;
await writeFile(join(dist, 'llms.txt'), llms);
const full = [`# quick: full documentation\n\n> ${home.meta.description}\n`, ...docs.map((d) => `<!-- ${site}${urlOf(d.slug)} -->\n\n${d.body.trim()}\n`)].join('\n---\n\n');
await writeFile(join(dist, 'llms-full.txt'), full);

// Stylesheets inlined into every page, so nothing blocks the first render.
const inlineCSS = async (dir) => {
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) { await inlineCSS(p); continue; }
    if (!e.name.endsWith('.html')) continue;
    let html = await readFile(p, 'utf8');
    for (const [tag, name] of html.matchAll(/<link rel="stylesheet" href="\/([\w-]+\.css)">/g)) {
      html = html.replace(tag, `<style>${(await readFile(join(here, 'public', name), 'utf8')).trim()}</style>`);
    }
    await writeFile(p, html);
  }
};
await inlineCSS(dist);

await writeFile(join(dist, 'sitemap.xml'), `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>${site}/</loc></url>
${docs.map((d) => `  <url><loc>${site}${urlOf(d.slug)}</loc></url>`).join('\n')}
</urlset>
`);

console.log(`built ${docs.length} docs pages into dist/`);
