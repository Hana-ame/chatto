import MarkdownIt from 'markdown-it';
import type StateInline from 'markdown-it/lib/rules_inline/state_inline.mjs';
import type StateCore from 'markdown-it/lib/rules_core/state_core.mjs';
import type StateBlock from 'markdown-it/lib/rules_block/state_block.mjs';
import type Token from 'markdown-it/lib/token.mjs';
import tlds from 'tlds';
import { classifyMessageBodyChatLink } from '$lib/messageLinks';

type CodeHighlightingModule = typeof import('$lib/codeHighlighting');

/**
 * Disabled markdown-it rules - we only allow a subset of markdown syntax.
 */
const DISABLED_RULES = [
  // Block-level
  'lheading',
  'hr',
  'reference',
  // Inline
  // 【本地改动】重新启用 image 规则以渲染消息正文内联 ![]() 图片；实际 <img> 由下方 image 渲染器走图片代理重写（见 IMAGE_PROXY_BASE / proxyImageSource）。
  'html_inline',
  // Backslash escapes turn `\_` into a literal `_`, which eats the arms of
  // common kaomoji like ¯\_(ツ)_/¯. Chat users type literal backslashes far
  // more often than they need CommonMark escapes; code spans still work for
  // escaping markdown chars when needed.
  'escape'
] as const;

const ALPHANUMERIC = /[a-zA-Z0-9]/;
const MAX_TABLE_COLUMNS = 64;
const MAX_TABLE_ROWS = 256;
const MAX_TABLE_CELLS = 4_096;
const MAX_TABLE_CELLS_PER_MESSAGE = 8_192;
const tableCellCountKey = Symbol('tableCellCount');

type MarkdownBlockRule = (
  state: StateBlock,
  startLine: number,
  endLine: number,
  silent: boolean
) => boolean;

function getBlockLine(state: StateBlock, line: number): string {
  const start = state.bMarks[line] + state.tShift[line];
  return state.src.slice(start, state.eMarks[line]);
}

function splitEscapedTableRow(row: string): string[] {
  const cells: string[] = [];
  let lastPosition = 0;
  let current = '';
  let escaped = false;

  for (let position = 0; position < row.length; position++) {
    const char = row[position];
    if (char === '|' && !escaped) {
      cells.push(current + row.slice(lastPosition, position));
      current = '';
      lastPosition = position + 1;
    } else if (char === '|' && escaped) {
      current += row.slice(lastPosition, position - 1);
      lastPosition = position;
    }

    escaped = char === '\\';
  }
  cells.push(current + row.slice(lastPosition));

  return cells;
}

function tableColumnCount(state: StateBlock, startLine: number, endLine: number): number | null {
  if (startLine + 2 > endLine) return null;

  const delimiterLine = getBlockLine(state, startLine + 1);
  if (!/^[|:\-\t ]+$/.test(delimiterLine)) return null;

  const delimiters = delimiterLine.split('|');
  if (delimiters[0]?.trim() === '') delimiters.shift();
  if (delimiters.at(-1)?.trim() === '') delimiters.pop();
  if (delimiters.length === 0 || delimiters.some((cell) => !/^:?-+:?$/.test(cell.trim()))) {
    return null;
  }

  const headerLine = getBlockLine(state, startLine).trim();
  if (!headerLine.includes('|')) return null;

  const headers = splitEscapedTableRow(headerLine);
  if (headers[0] === '') headers.shift();
  if (headers.at(-1) === '') headers.pop();

  return headers.length === delimiters.length ? headers.length : null;
}

function countTableCells(
  state: StateBlock,
  startLine: number,
  endLine: number,
  columnCount: number
): number {
  let rowCount = 1;
  const terminatorRules = state.md.block.ruler.getRules('blockquote');

  for (let line = startLine + 2; line < endLine; line++) {
    if (state.sCount[line] < state.blkIndent) break;
    if (state.sCount[line] - state.blkIndent >= 4) break;
    if (terminatorRules.some((rule) => rule(state, line, endLine, true))) break;
    if (!getBlockLine(state, line).trim()) break;

    rowCount++;
    if (rowCount > MAX_TABLE_ROWS || rowCount * columnCount > MAX_TABLE_CELLS) {
      return MAX_TABLE_CELLS + 1;
    }
  }

  return rowCount * columnCount;
}

function boundedTableRule(tableRule: MarkdownBlockRule): MarkdownBlockRule {
  return (state, startLine, endLine, silent) => {
    const columnCount = tableColumnCount(state, startLine, endLine);
    if (columnCount === null) return false;
    if (columnCount > MAX_TABLE_COLUMNS) return false;

    const cellCount = countTableCells(state, startLine, endLine, columnCount);
    const renderedCellCount = (state.env[tableCellCountKey] as number | undefined) ?? 0;
    if (
      cellCount > MAX_TABLE_CELLS ||
      renderedCellCount + cellCount > MAX_TABLE_CELLS_PER_MESSAGE
    ) {
      return false;
    }

    const parsed = tableRule(state, startLine, endLine, silent);
    if (parsed && !silent) state.env[tableCellCountKey] = renderedCellCount + cellCount;
    return parsed;
  };
}

/**
 * Inline rule that consumes `*` or `_` marker runs as literal text when they
 * are not at a word boundary. A word boundary requires exactly one side of
 * the run to be alphanumeric. This neuters intraword emphasis like
 * `foo*bar*baz` and punctuation-flanked markers like `_(ツ)_`, while
 * preserving normal `*italic*`, `_italic_`, and `**bold**`.
 */
function wordBoundaryEmphasis(state: StateInline, silent: boolean): boolean {
  const start = state.pos;
  const marker = state.src.charCodeAt(start);
  if (marker !== 0x2a /* * */ && marker !== 0x5f /* _ */) return false;

  let runEnd = start + 1;
  while (runEnd < state.posMax && state.src.charCodeAt(runEnd) === marker) {
    runEnd++;
  }
  const runLength = runEnd - start;

  const before = start > 0 ? state.src[start - 1] : '';
  const after = runEnd < state.src.length ? state.src[runEnd] : '';
  const beforeAlnum = ALPHANUMERIC.test(before);
  const afterAlnum = ALPHANUMERIC.test(after);

  // Single-marker intraword runs are definitely literal (`snake_case`,
  // `foo*bar*baz`). Double-marker runs are still allowed so bold can end next
  // to a following word (`**bold**text`).
  const intraword = runLength === 1 && beforeAlnum && afterAlnum;
  // Kaomoji-like: punctuation on both sides AND neither direction crosses an
  // alphanumeric before hitting a same-marker run or the input boundary. The
  // bidirectional check distinguishes a true kaomoji marker (e.g. the trailing
  // `_` in `_(ツ)_/¯` — only punctuation back to the opener and only
  // punctuation forward to end of input) from a closer of a real emphasis
  // run that happens to be followed by punctuation/another emphasis (e.g.
  // the closing `**` in `**foo:** **bar**` — alnum `o` is right behind it).
  let kaomojiLike = false;
  if (!beforeAlnum && !afterAlnum) {
    let forwardOK = true;
    for (let i = runEnd; i < state.posMax; i++) {
      if (state.src.charCodeAt(i) === marker) break;
      if (ALPHANUMERIC.test(state.src[i])) {
        forwardOK = false;
        break;
      }
    }
    if (forwardOK) {
      kaomojiLike = true;
      for (let i = start - 1; i >= 0; i--) {
        if (state.src.charCodeAt(i) === marker) break;
        if (ALPHANUMERIC.test(state.src[i])) {
          kaomojiLike = false;
          break;
        }
      }
    }
  }
  if (intraword || kaomojiLike) {
    if (!silent) state.pending += state.src.slice(start, runEnd);
    state.pos = runEnd;
    return true;
  }

  return false;
}

let md: MarkdownIt | null = null;
let codeHighlighting: CodeHighlightingModule | null = null;

// ===== LaTeX 公式渲染 via KaTeX — 【本地改动 2026-09-01】 =====
// 目的：聊天消息支持 LaTeX 数学公式，$...$ 行内、$$...$$ 独立行。
// 思路：markdown-it 自定义 inline 规则（math_inline）捕获 $...$ / $$...$$，
// 仅把安全占位符 <span class="math" data-latex="..."> 作为 html_inline token
// 塞进 token 流；renderMarkdown 做后处理：首次遇到公式时懒加载 katex（JS + CSS），把占位符
// 替换为 KaTeX 生成的 HTML（已自带 <span class="katex"> 外壳）。
// 安全：只用 katex 默认渲染（未启用 mhchem / html 插件，输出不含 <script> /
// javascript: URL），throwOnError=false 防恶意/畸形输入导致崩溃（恶意输入渲染
// 为 TeX 错误框而非报错），输出仍经 MarkdownHtml.svelte 的 Trusted Types 通道。
// 懒加载：首屏 bundle 零开销；首次公式渲染时才拉 katex JS 与 CSS，后续缓存。
// UX 取舍：$...$（行内）要求内容至少含一个字母或 LaTeX 运算符（\\^_{}&%），
// 否则视为普通文本——避免聊天中 $10 / $$5 等金额被误识别为公式；$$...$$
// （独立行）总是公式，因 $$ 在聊天中罕作金额。
// 边界：未启用 mhchem（下标/上标、分式、希腊字母等基础 LaTeX 全部可用）；
// \begin{equation}...\end{equation} 等块级环境未实现；$$ 只支持单行。
// 踩坑：markdown-it 的 escape 规则已禁用（见 DISABLED_RULES），反斜杠不作为
// 转义，$ 的配对需手动处理（不能依赖 state.escape）；silent 模式下不得修改
// state.pending，仅推进 state.pos 并返回 true。
const MATH_PLACEHOLDER_RE = /<span class="math" data-latex="([^"]*)" data-math-type="(\w+)"><\/span>/g;

let katexRenderer: ((latex: string, opts: { throwOnError: boolean; displayMode: boolean }) => string) | null = null;
let katexLoading = false;

function hasMathyContent(latex: string): boolean {
  // Inline $...$ 需含字母或 LaTeX 运算符才触发公式模式；纯数字/空白/标点视为
  // 普通文本（$10、$5.00 等金额），避免聊天中金额被误识别。$$...$$ 不受此限。
  return /[a-zA-Z\\^_{}&%]/.test(latex);
}

function decodeHtmlEntities(value: string): string {
  // 仅解码 escapeHtml() 生成的标准实体；我们控制编码端，无需通用解析器。
  return value
    .replaceAll('&amp;', '&')
    .replaceAll('&lt;', '<')
    .replaceAll('&gt;', '>')
    .replaceAll('&quot;', '"')
    .replaceAll('&#39;', "'");
}

function mathInline(state: StateInline, silent: boolean): boolean {
  const start = state.pos;
  if (start >= state.posMax) return false;
  const first = state.src.charCodeAt(start);
  if (first !== 0x24) return false; // not $

  // Escaped `\$`（escape 规则虽禁用，防御性处理）。
  if (start > 0 && state.src.charCodeAt(start - 1) === 0x5c) return false;

  // 判断是 $$...$$（独立行）还是 $...$（行内），并扫描闭合符。
  if (start + 1 < state.posMax && state.src.charCodeAt(start + 1) === 0x24) {
    // $$...$$ 独立行公式：消费开头的 $$，向后找下一个未转义的 $$ 作为闭合。
    let close = start + 2;
    while (close < state.posMax) {
      const c = state.src.charCodeAt(close);
      if (c === 0x24 && close + 1 < state.posMax && state.src.charCodeAt(close + 1) === 0x24) {
        break;
      }
      if (c === 0x5c) close++; // 跳过反斜杠转义
      close++;
    }
    if (close >= state.posMax) return false; // 无闭合 $$
    const latex = state.src.slice(start + 2, close);
    if (silent) {
      state.pos = close + 2;
      return true;
    }
    if (!hasMathyContent(latex)) {
      // 纯数字/符号：视为普通文本，原样输出整段（含 $$ 边界）。
      state.pending += state.src.slice(start, close + 2);
      state.pos = close + 2;
      return true;
    }
    // 【本地改动 2026-09-01 修复】用 state.push 推 html_inline token：state.pending 里的
    // 原始 HTML 会被 markdown-it 转义成 &lt;span&gt;（测试实测），占位符正则匹配不上、
    // 残留到最终 HTML；html_inline token 内容作为原始 HTML 落地，占位符可被
    // replaceMathPlaceholders 正确替换。
    const placeholder = state.push('html_inline', '', 0);
    placeholder.content = `<span class="math" data-latex="${escapeHtml(latex)}" data-math-type="display"></span>`;
    state.pos = close + 2;
    return true;
  }

  // $...$ 行内公式：下一个 $ 且其后非 $ 的即为闭合。
  let close = start + 1;
  while (close < state.posMax) {
    const c = state.src.charCodeAt(close);
    if (c === 0x24) break;
    if (c === 0x5c) close++; // 跳过反斜杠转义
    close++;
  }
  if (close >= state.posMax) return false; // 无闭合 $
  const latex = state.src.slice(start + 1, close);
  if (silent) {
    state.pos = close + 1;
    return true;
  }
  if (!hasMathyContent(latex)) {
    // 内容不含字母/运算符：视为普通文本，仅输出开头 $（让剩余内容被重新解析，
    // 从而允许 $10 $a^2$ 这类序列中后面的公式仍被正确捕获）。
    state.pending += state.src.slice(start, start + 1);
    state.pos = start + 1;
    return true;
  }
  const inlinePlaceholder = state.push('html_inline', '', 0);
  inlinePlaceholder.content = `<span class="math" data-latex="${escapeHtml(latex)}" data-math-type="inline"></span>`;
  state.pos = close + 1;
  return true;
}

async function ensureKatexReady(): Promise<void> {
  if (katexRenderer) return;
  if (katexLoading) {
    // 等待进行中的加载完成。
    while (katexLoading && !katexRenderer) {
      await new Promise((r) => setTimeout(r, 10));
    }
    return;
  }
  katexLoading = true;
  try {
    // CSS 懒加载：浏览器环境 Vite 会注入 <link>；Node 测试环境（server 项目）
    // 不支持动态 import CSS，静默跳过——公式仍能渲染，仅缺样式（测试不关心样式）。
    try {
      await import('katex/dist/katex.min.css');
    } catch {
      /* CSS unavailable in this environment; katex HTML output still valid. */
    }
    const katexModule = await import('katex');
    katexRenderer = katexModule.renderToString;
  } finally {
    katexLoading = false;
  }
}

async function replaceMathPlaceholders(html: string): Promise<string> {
  if (!html.includes('class="math"')) return html;
  await ensureKatexReady();
  if (!katexRenderer) return html;
  return html.replace(MATH_PLACEHOLDER_RE, (_match, escapedLatex, blockType) => {
    const latex = decodeHtmlEntities(escapedLatex);
    const displayMode = blockType === 'display';
    const render = katexRenderer!;
    return render(latex, { throwOnError: false, displayMode });
  });
}

type LowlightText = {
  type: 'text';
  value: string;
};

type LowlightElement = {
  type: 'element';
  tagName: string;
  properties?: Record<string, unknown>;
  children?: LowlightNode[];
};

type LowlightNode =
  | LowlightText
  | LowlightElement
  | {
      type: string;
      children?: LowlightNode[];
    };

function escapeHtml(value: string): string {
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');
}

function escapeAttribute(value: string): string {
  return escapeHtml(value).replaceAll("'", '&#39;');
}

function renderClassName(value: unknown): string | null {
  if (Array.isArray(value)) {
    const classes = value.filter((item): item is string => typeof item === 'string');
    return classes.length > 0 ? classes.join(' ') : null;
  }

  return typeof value === 'string' && value.length > 0 ? value : null;
}

function renderElementOpen(node: LowlightElement): string {
  const className = renderClassName(node.properties?.className);
  const classAttribute = className ? ` class="${escapeAttribute(className)}"` : '';
  return `<${node.tagName}${classAttribute}>`;
}

function isLowlightText(node: LowlightNode): node is LowlightText {
  return node.type === 'text';
}

function isLowlightElement(node: LowlightNode): node is LowlightElement {
  return node.type === 'element';
}

function renderLowlightLines(nodes: LowlightNode[]): string[] {
  const lines = [''];

  function append(value: string) {
    lines[lines.length - 1] += value;
  }

  function renderNode(node: LowlightNode, activeOpen: string, activeClose: string) {
    if (isLowlightText(node)) {
      const parts = node.value.replaceAll('\t', '    ').split('\n');

      for (let i = 0; i < parts.length; i++) {
        if (i > 0) {
          append(activeClose);
          lines.push(activeOpen);
        }
        append(escapeHtml(parts[i]));
      }
      return;
    }

    if (isLowlightElement(node)) {
      const open = renderElementOpen(node);
      const close = `</${node.tagName}>`;
      append(open);

      for (const child of node.children ?? []) {
        renderNode(child, `${activeOpen}${open}`, `${close}${activeClose}`);
      }

      append(close);
      return;
    }

    for (const child of 'children' in node ? (node.children ?? []) : []) {
      renderNode(child, activeOpen, activeClose);
    }
  }

  for (const node of nodes) {
    renderNode(node, '', '');
  }

  return lines;
}

function renderPlainCodeLines(code: string): string[] {
  return code.replaceAll('\t', '    ').split('\n').map(escapeHtml);
}

function renderCodeFence(code: string, rawLanguage: string): string {
  const displayLanguage = normalizeCodeLanguage(rawLanguage);
  const resolvedLanguage = codeHighlighting?.resolveCodeLanguage(displayLanguage);
  const displayCode = code.replace(/\r?\n$/, '');
  const lines =
    resolvedLanguage && codeHighlighting?.lowlight.registered(displayLanguage)
      ? renderLowlightLines(
          (
            codeHighlighting.lowlight.highlight(displayLanguage, displayCode) as {
              children: LowlightNode[];
            }
          ).children
        )
      : resolvedLanguage && codeHighlighting?.lowlight.registered(resolvedLanguage)
        ? renderLowlightLines(
            (
              codeHighlighting.lowlight.highlight(resolvedLanguage, displayCode) as {
                children: LowlightNode[];
              }
            ).children
          )
        : renderPlainCodeLines(displayCode);
  const lineHtml = lines.map((line) => `<span class="line">${line}</span>`).join('');

  return `<pre class="hljs" data-language="${escapeAttribute(displayLanguage)}"><code class="language-${escapeAttribute(displayLanguage)}">${lineHtml}</code></pre>`;
}

function normalizeCodeLanguage(language: string | null | undefined): string {
  const token = language
    ?.trim()
    .toLowerCase()
    .match(/[a-z0-9+#_.-]+/)?.[0];
  return token || 'text';
}

function extractFenceLanguages(markdown: string): string[] {
  const languages = new Set<string>();
  const fencePattern = /^[ \t]*(```|~~~)[ \t]*([^\s`~]*)/gm;
  let match: RegExpExecArray | null;

  while ((match = fencePattern.exec(markdown))) {
    languages.add(normalizeCodeLanguage(match[2]));
  }

  return [...languages];
}

function isLineBreakToken(token: Token): boolean {
  return token.type === 'softbreak' || token.type === 'hardbreak';
}

function isWhitespaceOnlyInlineSegment(tokens: Token[]): boolean {
  return tokens.every((token) => {
    if (typeof token.type !== 'string') return false;
    if (token.type === 'text' || token.type === 'text_special') {
      return token.content.trim().length === 0;
    }
    return token.type.endsWith('_open') || token.type.endsWith('_close');
  });
}

function lineAfterBreakIsWhitespaceOnly(tokens: Token[], idx: number): boolean {
  let lineEnd = idx + 1;
  while (lineEnd < tokens.length && !isLineBreakToken(tokens[lineEnd])) lineEnd++;
  return isWhitespaceOnlyInlineSegment(tokens.slice(idx + 1, lineEnd));
}

function renderChatLineBreak(tokens: Token[], idx: number): string {
  return lineAfterBreakIsWhitespaceOnly(tokens, idx) ? '' : '<br>\n';
}

function renderParagraphOpen(tokens: Token[], idx: number): string {
  return tokens[idx].hidden ? '<span class="list-item-content">' : '<p>';
}

function renderParagraphClose(tokens: Token[], idx: number): string {
  return tokens[idx].hidden ? '</span>\n' : '</p>\n';
}

function normalizeInlineNonBreakingSpaces(state: StateCore): void {
  for (let i = 0; i < state.tokens.length; i++) {
    const token = state.tokens[i];
    if (token.type !== 'inline') continue;
    for (const child of token.children ?? []) {
      if (child.type === 'text' || child.type === 'text_special') {
        child.content = child.content.replaceAll('\u00A0', ' ');
      }
    }
    if (isWhitespaceOnlyInlineSegment(token.children ?? [])) {
      if (state.tokens[i - 1]?.type === 'paragraph_open') state.tokens[i - 1].hidden = true;
      if (state.tokens[i + 1]?.type === 'paragraph_close') state.tokens[i + 1].hidden = true;
    }
  }
}

async function ensureFenceLanguagesLoaded(languages: string[]): Promise<void> {
  if (languages.length === 0) return;

  codeHighlighting ??= await import('$lib/codeHighlighting');
  await codeHighlighting.ensureCodeLanguagesLoaded(languages);
}

/**
 * Base URL of Chatto's image proxy. Every inline `![]()` image is rewritten
 * through it so the viewer's IP/Referer is hidden from the original source
 * host. The proxy re-fetches the original using the `proxy_host` and
 * `proxy_scheme` params; the original path/query are preserved and any
 * fragment stays at the end.
 */
// 【本地改动】内联图片统一走该图片代理：隐藏观看者的 IP/Referer，避免消息正文直接暴露原始图片 host。
// 目的：让 `![alt](https://外链)` 渲染成图片但由代理去取图。思路：用 URL + searchParams 保留原始
// path/query，并追加 proxy_host / proxy_scheme，fragment 留在最末尾（与用户约定一致）。踩坑：带端口的
// host 会被 searchParams 编码成 host%3Aport（`:` → `%3A`），代理端按 query param 正常解码即可。
// 边界：仅影响消息正文 inline image；现有附件（走独立签名 URL + MessageAttachments 渲染）完全不受影响。
const IMAGE_PROXY_BASE = 'https://proxy.moonchan.xyz';

// 【本地改动】把原始图片 src 重写为代理 URL；非 http(s)（含 javascript:/data:/相对路径/ftp:/mailto: 等）
// 一律返回 '#'，与下方 image 渲染器的安全兜底一致。注意 markdown-it 自带 validateLink 已先拦掉
// javascript:/vbscript:/file:/data: 等危险协议，这里再锁死 http(s)。
function proxyImageSource(src: string): string {
  let original: URL;
  try {
    original = new URL(src);
  } catch {
    return '#';
  }
  if (original.protocol !== 'http:' && original.protocol !== 'https:') {
    return '#';
  }

  // 【本地改动 2026-08-31】src 本就指向图片代理自身时不再套一层代理，原样直通。
  // 踩坑：此前无条件改写会把 proxy_host 写成 proxy.moonchan.xyz（自引用）——
  // 浏览器请求代理、代理再请求自己，形成环/404/超时，用户看到裂图；典型场景是
  // 把已渲染过的代理 URL（或手工粘贴的代理地址）贴回消息，以及复制再贴。
  // 思路：只按 hostname 判定（不校验 scheme/path）——代理自己域名上的任意
  // http(s) 路径都归代理处理，无需二次代理；保留用户原始 query 与 fragment，
  // 若原本就带 proxy_host 等参数也原样保留（不被覆盖）。
  // 边界：不影响非代理输入的既有改写路径；也不改变代理域名的 SSRF 语义
  // （proxy_host 指向任意 host 的能力在普通改写路径里本来就存在，与本分支无关）。
  const proxyHostname = new URL(IMAGE_PROXY_BASE).hostname;
  if (original.hostname === proxyHostname) {
    return src;
  }

  const proxy = new URL(IMAGE_PROXY_BASE);
  proxy.pathname = original.pathname;
  proxy.search = original.search;
  proxy.searchParams.set('proxy_host', original.host);
  proxy.searchParams.set('proxy_scheme', original.protocol === 'https:' ? 'https' : 'http');
  proxy.hash = original.hash;
  return proxy.toString();
}

/**
 * Initialize the markdown-it instance.
 * Called once on first render.
 */
function initialize(): void {
  if (md) return;

  md = new MarkdownIt({
    html: false, // Disable HTML tags in source
    linkify: true, // Auto-convert URLs to links
    breaks: true, // Convert \n to <br>
    highlight: renderCodeFence
  });

  // Update linkify-it's TLD list so bare-domain URLs with newer TLDs
  // (.dev, .app, .io, etc.) are auto-linked
  md.linkify.tlds(tlds);

  // markdown-it pads short table rows to the header width. Without a guard, a
  // tiny table source can therefore expand into hundreds of thousands of DOM
  // nodes. Resolve the built-in rule through the public ruler API, then bound
  // it before token allocation while preserving its normal parsing behavior.
  const tableRules = new MarkdownIt().block.ruler;
  tableRules.enableOnly(['table']);
  const tableRule = tableRules.getRules('')[0] as MarkdownBlockRule | undefined;
  if (!tableRule) throw new Error('markdown-it table rule is unavailable');
  md.block.ruler.at('table', boundedTableRule(tableRule), {
    alt: ['paragraph', 'reference']
  });

  // Disable unwanted syntax - only keep what we explicitly want
  md.disable([...DISABLED_RULES]);

  // 【本地改动 2026-09-01】数学公式：注册 math_inline 规则，在 emphasis 之前捕获
  // $...$ / $$...$$ 为安全占位符。见上方 math_inline 注释块了解安全/UX 取舍。
  md.inline.ruler.before('emphasis', 'math_inline', mathInline);

  // Restrict `*` and `_` emphasis to word boundaries. Prevents intraword
  // emphasis (e.g. `snake_case`, `foo*bar*baz`) and emphasis between
  // punctuation (e.g. the underscores in `¯\_(ツ)_/¯`) from being parsed
  // as italics. Inserted before the `emphasis` rule so non-boundary marker
  // runs are consumed as literal text.
  md.inline.ruler.before('emphasis', 'word_boundary_emphasis', wordBoundaryEmphasis);

  // CommonMark decodes entities in prose but leaves them literal in code. Turn
  // decoded NBSPs into collapsible spaces only in ordinary inline text so long
  // `&nbsp;` runs cannot create giant blank message rows without corrupting
  // code samples that intentionally contain the entity source.
  md.core.ruler.after('inline', 'normalize_non_breaking_spaces', normalizeInlineNonBreakingSpaces);
  md.renderer.rules.softbreak = renderChatLineBreak;
  md.renderer.rules.hardbreak = renderChatLineBreak;
  md.renderer.rules.table_open = () => '<div class="table-scroll" tabindex="0"><table>\n';
  md.renderer.rules.table_close = () => '</table></div>\n';
  // Markdown-it hides paragraph tags in tight lists. Keep their inline content
  // grouped so ordered-list grid markers do not turn each inline element into
  // a separate grid row.
  md.renderer.rules.paragraph_open = renderParagraphOpen;
  md.renderer.rules.paragraph_close = renderParagraphClose;

  // Customize link rendering for security
  const defaultLinkRender =
    md.renderer.rules.link_open ||
    function (tokens, idx, options, _env, self) {
      return self.renderToken(tokens, idx, options);
    };

  md.renderer.rules.link_open = function (tokens, idx, options, env, self) {
    const token = tokens[idx];
    const hrefIndex = token.attrIndex('href');
    let allowedSameTabChatLink = false;

    if (hrefIndex >= 0) {
      const href = token.attrs![hrefIndex][1];

      // Only allow http and https URLs
      if (!href.startsWith('http://') && !href.startsWith('https://')) {
        // Replace dangerous URLs with empty href
        token.attrs![hrefIndex][1] = '#';
      } else {
        allowedSameTabChatLink = classifyMessageBodyChatLink(href) !== null;
      }
    }

    // External and non-allow-listed links open out-of-band. Known same-origin
    // chat routes intentionally keep normal same-tab navigation semantics.
    if (!allowedSameTabChatLink) {
      token.attrSet('target', '_blank');
      token.attrSet('rel', 'noopener noreferrer');
    }

    return defaultLinkRender(tokens, idx, options, env, self);
  };

  // 【本地改动】内联图片渲染：先把 src 经代理重写（proxyImageSource），再加固输出标签。
  // 思路：沿用 link_open 的安全处理——只放 http(s) 源、去掉可选 title 防 tooltip 注入、加
  // loading=lazy / referrerpolicy=no-referrer / rel=noopener noreferrer。踩坑：markdown-it 会把属性里的
  // `&` 转义成 `&amp;`，所以单测断言要避开跨参数的 `&`、只校验各片段。输出仍只经 MarkdownHtml 的
  // Trusted Types 注入点，安全边界不变。
  // Customize image rendering: route every inline image through the Chatto image
  // proxy (hides the viewer from the source host) and harden the emitted tag.
  const defaultImageRender =
    md.renderer.rules.image ||
    function (tokens, idx, options, _env, self) {
      return self.renderToken(tokens, idx, options);
    };

  md.renderer.rules.image = function (tokens, idx, options, env, self) {
    const token = tokens[idx];
    const srcIndex = token.attrIndex('src');
    if (srcIndex >= 0) {
      token.attrs![srcIndex][1] = proxyImageSource(token.attrs![srcIndex][1]);
    }
    // Drop the optional title to avoid tooltip injection/abuse.
    const titleIndex = token.attrIndex('title');
    if (titleIndex >= 0) token.attrs!.splice(titleIndex, 1);
    token.attrSet('loading', 'lazy');
    token.attrSet('referrerpolicy', 'no-referrer');
    token.attrSet('rel', 'noopener noreferrer');
    return defaultImageRender(tokens, idx, options, env, self);
  };
}

/**
 * Renders markdown to HTML.
 */
export async function renderMarkdown(body: string): Promise<string> {
  try {
    await ensureFenceLanguagesLoaded(extractFenceLanguages(body));
    initialize();

    let html = md!.render(body);
    html = await replaceMathPlaceholders(html);
    return html;
  } catch (err) {
    console.error('[Markdown] renderMarkdown failed:', err, { bodyLength: body.length });
    throw err;
  }
}
