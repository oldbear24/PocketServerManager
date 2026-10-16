// Renders the Claude Design artboards in design/source/*.dc.html into plain,
// static HTML pages in design/preview/. Each page is a snapshot of the screen's
// initial state: {{holes}}, <sc-for> and <sc-if> are expanded with the values
// returned by the artboard's Component.renderVals(), event handlers are dropped,
// and links between artboards (Foo.dc.html) are rewritten to Foo.html so the
// previews stay clickable.
//
// Usage: (cd design/tools && npm install && npm run render)

import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import { parseDocument } from 'htmlparser2';
import { screens } from './screens.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const SRC = join(here, '..', 'source');
const OUT = join(here, '..', 'preview');

const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);
const SVG_LEAF = new Set(['path', 'rect', 'circle', 'ellipse', 'line', 'polyline', 'polygon', 'stop', 'use']);
const BOOL_ATTRS = new Set(['checked', 'disabled', 'readonly', 'selected', 'hidden', 'required', 'multiple', 'open']);
const RENAME = { readOnly: 'readonly', className: 'class', htmlFor: 'for', spellCheck: 'spellcheck' };
const HOLE = /\{\{\s*([^}]+?)\s*\}\}/g;

const escText = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const escAttr = (s) => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;');

function lookup(path, scope) {
  if (path === 'true') return true;
  if (path === 'false') return false;
  if (path === 'null') return null;
  if (/^-?\d+(\.\d+)?$/.test(path)) return Number(path);
  if (/^'.*'$|^".*"$/.test(path)) return path.slice(1, -1);
  const parts = path.split('.');
  let v = scope[parts[0]];
  for (let i = 1; i < parts.length; i++) {
    if (v == null) return undefined;
    v = v[parts[i]];
  }
  return v;
}

const wholeHole = (s) => {
  const m = /^\s*\{\{\s*([^}]+?)\s*\}\}\s*$/.exec(s ?? '');
  return m ? m[1] : null;
};

const rewriteLinks = (href) => href.replace(/([A-Za-z0-9_-]+)\.dc\.html/g, '$1.html');

function renderAttrs(attribs, scope) {
  let out = '';
  for (const [raw, value] of Object.entries(attribs)) {
    if (/^on[A-Z]/.test(raw) || raw.startsWith('hint-')) continue;
    const name = RENAME[raw] || raw;
    const exact = wholeHole(value);
    if (BOOL_ATTRS.has(name.toLowerCase())) {
      const v = exact !== null ? lookup(exact, scope) : value;
      if (v === true || v === 'true' || v === '' || v === name) out += ` ${name.toLowerCase()}`;
      continue;
    }
    let rendered;
    if (exact !== null) {
      const v = lookup(exact, scope);
      if (v === false || v == null) continue;
      rendered = escAttr(v);
    } else if (value.includes('{{')) {
      rendered = value.replace(HOLE, (_, p) => {
        const v = lookup(p.trim(), scope);
        return v == null ? '' : escAttr(v);
      });
    } else {
      rendered = value; // literal, already valid HTML
    }
    if (name === 'href') rendered = rewriteLinks(rendered);
    out += ` ${name}="${rendered}"`;
  }
  return out;
}

function renderNodes(nodes, scope, ctx) {
  return nodes.map((n) => renderNode(n, scope, ctx)).join('');
}

function renderNode(n, scope, ctx) {
  if (n.type === 'text') {
    return n.data.replace(HOLE, (_, p) => {
      const v = lookup(p.trim(), scope);
      return v == null ? '' : escText(v);
    });
  }
  if (n.type === 'comment' || n.type === 'directive') return '';
  if (n.type === 'script') return '';
  if (n.type === 'style') return `<style>${n.children.map((c) => c.data).join('')}</style>`;

  switch (n.name) {
    case 'helmet':
      ctx.head.push(renderNodes(n.children, scope, ctx));
      return '';
    case 'sc-for': {
      const list = lookup(wholeHole(n.attribs.list) ?? '', scope) || [];
      const as = n.attribs.as || 'item';
      return list
        .map((item, i) => {
          const s = Object.create(scope);
          s[as] = item;
          s.$index = i;
          return renderNodes(n.children, s, ctx);
        })
        .join('');
    }
    case 'sc-if':
      return lookup(wholeHole(n.attribs.value) ?? '', scope) ? renderNodes(n.children, scope, ctx) : '';
    default: {
      const attrs = renderAttrs(n.attribs, scope);
      if (VOID.has(n.name)) return `<${n.name}${attrs}>`;
      if (SVG_LEAF.has(n.name) && n.children.length === 0) return `<${n.name}${attrs}/>`;
      return `<${n.name}${attrs}>${renderNodes(n.children, scope, ctx)}</${n.name}>`;
    }
  }
}

function find(nodes, pred) {
  for (const n of nodes) {
    if (pred(n)) return n;
    if (n.children) {
      const hit = find(n.children, pred);
      if (hit) return hit;
    }
  }
  return null;
}

function evaluateComponent(code, file) {
  const sandbox = {
    console, Math, JSON, Object, Array, String, Number, Boolean, Date, RegExp,
    navigator: {}, setInterval: () => 0, clearInterval: () => {}, setTimeout: () => 0, clearTimeout: () => {}
  };
  vm.createContext(sandbox);
  vm.runInContext(
    `class DCLogic {
       constructor(props) { this.props = props || {}; this.state = {}; }
       setState(p) { Object.assign(this.state, typeof p === 'function' ? p(this.state) : p); }
       forceUpdate() {}
     }
     ${code}
     globalThis.__Component = Component;`,
    sandbox,
    { filename: file }
  );
  const instance = new sandbox.__Component({});
  return instance.renderVals();
}

function renderFile(screen) {
  const file = `${screen.source}.dc.html`;
  const html = readFileSync(join(SRC, file), 'utf8');
  const doc = parseDocument(html, {
    lowerCaseAttributeNames: false,
    recognizeSelfClosing: true,
    decodeEntities: false
  });
  const root = find(doc.children, (n) => n.name === 'x-dc');
  const script = find(doc.children, (n) => n.name === 'script' && n.attribs && 'data-dc-script' in n.attribs);
  const titleEl = find(doc.children, (n) => n.name === 'title');
  const htmlEl = find(doc.children, (n) => n.name === 'html');
  if (!root || !script) throw new Error(`${file}: missing <x-dc> or data-dc-script`);

  const vals = evaluateComponent(script.children.map((c) => c.data).join(''), file);
  const ctx = { head: [] };
  const body = renderNodes(root.children, Object.assign(Object.create(null), vals), ctx);
  const title = titleEl ? titleEl.children.map((c) => c.data).join('') : screen.title;
  const lang = htmlEl?.attribs?.lang || 'en';

  return `<!doctype html>
<html lang="${lang}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title>
<!-- Static preview generated by design/tools/render.mjs from design/source/${file}. Do not edit by hand. -->
${ctx.head.join('\n').trim()}
</head>
<body>
${body.trim()}
</body>
</html>
`;
}

function renderIndex() {
  const cards = screens
    .map(
      (s) => `<a class="card" href="${s.source}.html">
<img src="../screenshots/${s.slug}.png" alt="" loading="lazy">
<span class="t">${s.title}</span>
<span class="d">${s.description}</span>
</a>`
    )
    .join('\n');
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PocketCloud – UI previews</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500;600&amp;display=swap">
<style>
body{margin:0;background:#0C0E12;color:#E6E8EC;font-family:'Geist',ui-sans-serif,system-ui,sans-serif}
main{max-width:1200px;margin:0 auto;padding:40px 16px 64px}
h1{margin:0 0 6px;font-size:28px;font-weight:600;letter-spacing:-0.02em}
p{margin:0 0 28px;color:#98A1AE}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:16px}
.card{display:flex;flex-direction:column;gap:6px;padding:12px;border:1px solid #1F242D;border-radius:12px;background:#13161C;color:inherit;text-decoration:none}
.card:hover{border-color:#33405A}
.card img{width:100%;aspect-ratio:16/10;object-fit:cover;object-position:top left;border-radius:8px;background:#101318}
.t{font-weight:600}
.d{font-size:13px;color:#98A1AE}
</style>
</head>
<body>
<main>
<h1>PocketCloud – UI previews</h1>
<p>Static snapshots of the design. Screens link to each other like the prototype; interactive bits (filters, wizard progress) live in the source canvas.</p>
<div class="grid">
${cards}
</div>
</main>
</body>
</html>
`;
}

if (!existsSync(OUT)) mkdirSync(OUT, { recursive: true });
for (const screen of screens) {
  writeFileSync(join(OUT, `${screen.source}.html`), renderFile(screen));
  console.log(`rendered ${screen.source}.html`);
}
writeFileSync(join(OUT, 'index.html'), renderIndex());
console.log('rendered index.html');
