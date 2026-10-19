import { describe, it, expect } from 'vitest';
import { renderInlineMarkdown, renderMarkdown } from './markdown';
import {
  canHighlightCodeLanguage,
  ensureCodeLanguageLoaded,
  isCodeLanguageLoaded,
  lowlight
} from './codeHighlighting';

function tableSource(columns: number, bodyRows: number): string {
  return [
    Array.from({ length: columns }, (_, index) => `h${index}`).join('|'),
    Array.from({ length: columns }, () => '---').join('|'),
    ...Array.from({ length: bodyRows }, () => '|')
  ].join('\n');
}

describe('renderInlineMarkdown', () => {
  it('keeps formatting and safe links without block markup or line breaks', () => {
    const html = renderInlineMarkdown('**News**\n\n*Today*\r\n[Details](https://example.com)');
    expect(html).toContain('<strong>News</strong>');
    expect(html).toContain('<em>Today</em>');
    expect(html).toContain('href="https://example.com"');
    expect(html).toContain('rel="noopener noreferrer"');
    expect(html).not.toMatch(/<(?:p|br|h[1-6]|ul|ol)\b|[\r\n]/);
  });

  it('escapes source HTML and rejects unsafe links', () => {
    const html = renderInlineMarkdown('<img src=x onerror=alert(1)> [bad](javascript:alert(1))');
    expect(html).toContain('&lt;img');
    expect(html).not.toContain('<img');
    expect(html).not.toContain('href="javascript:');
  });

  it('keeps markdown images and dollar expressions as inline text in single-line mode', () => {
    // 【本地改动 2026-10-19】发现背景：renderInlineMarkdown 是同步函数（用于顶栏 MOTD），
    // 此前未区分单行模式，导致 ![]() 输出块级 <img> 撑破顶栏，且 $...$ 输出空占位
    // <span class="math"></span> 吞掉文字。
    const html = renderInlineMarkdown('![Banner](https://example.com/banner.png) $a^2 + b^2$');
    expect(html).not.toContain('<img');
    expect(html).not.toContain('class="math"');
    expect(html).toContain('Banner');
    expect(html).toContain('$a^2 + b^2$');
  });
});

describe('renderMarkdown', () => {
  describe('GFM tables', () => {
    it('renders a pipe table with semantic sections', async () => {
      const html = await renderMarkdown('| Name | Role |\n| --- | --- |\n| Ada | Admin |');

      expect(html).toContain('<div class="table-scroll" tabindex="0"><table>');
      expect(html).toContain('<thead>');
      expect(html).toContain('<th>Name</th>');
      expect(html).toContain('<tbody>');
      expect(html).toContain('<td>Ada</td>');
      expect(html).toContain('</table></div>');
    });

    it('renders tables without outer pipes', async () => {
      const html = await renderMarkdown('Name | Role\n--- | ---\nAda | Admin');

      expect(html).toContain('<table>');
      expect(html).toContain('<td>Ada</td>');
    });

    it('honours GFM column alignment', async () => {
      const html = await renderMarkdown(
        '| Left | Centre | Right |\n| :--- | :---: | ---: |\n| a | b | c |'
      );

      expect(html).toContain('<th style="text-align:left">Left</th>');
      expect(html).toContain('<th style="text-align:center">Centre</th>');
      expect(html).toContain('<th style="text-align:right">Right</th>');
      expect(html).toContain('<td style="text-align:center">b</td>');
    });

    it('renders inline Markdown and escaped pipes inside cells', async () => {
      const html = await renderMarkdown(
        '| Value | Notes |\n| --- | --- |\n| **bold** | one \\| two |\n| `a\\|b` | [link](https://example.com) |'
      );

      expect(html).toContain('<strong>bold</strong>');
      expect(html).toContain('<td>one | two</td>');
      expect(html).toContain('<code>a|b</code>');
      expect(html).toContain('href="https://example.com"');
    });

    it('leaves table-like text literal without a valid delimiter row', async () => {
      const html = await renderMarkdown('| Name | Role |\n| Ada | Admin |');

      expect(html).not.toContain('<table>');
    });

    it('does not expand adversarial short rows into an oversized table', async () => {
      const body = tableSource(256, 256);

      const html = await renderMarkdown(body);

      expect(html).not.toContain('<table>');
      expect(html).not.toContain('<td>');
      expect(html.length).toBeLessThan(20_000);
    });

    it('accepts the column limit and rejects one additional column', async () => {
      const atLimit = await renderMarkdown(tableSource(64, 1));
      const overLimit = await renderMarkdown(tableSource(65, 1));

      expect(atLimit).toContain('<table>');
      expect(overLimit).not.toContain('<table>');
    });

    it('accepts the row limit and rejects one additional row', async () => {
      // The header counts as one of the 256 rows.
      const atLimit = await renderMarkdown(tableSource(2, 255));
      const overLimit = await renderMarkdown(tableSource(2, 256));

      expect(atLimit).toContain('<table>');
      expect(overLimit).not.toContain('<table>');
    });

    it('accepts the per-table cell limit and rejects one additional row of cells', async () => {
      // 64 columns × 64 rows, including the header, is exactly 4,096 cells.
      const atLimit = await renderMarkdown(tableSource(64, 63));
      const overLimit = await renderMarkdown(tableSource(64, 64));

      expect(atLimit).toContain('<table>');
      expect(overLimit).not.toContain('<table>');
    });

    it('limits cumulative table cells across one message', async () => {
      const maximumTable = tableSource(64, 63);
      const html = await renderMarkdown(
        `${maximumTable}\n\n${maximumTable}\n\n| Extra | Table |\n| --- | --- |\n| x | y |`
      );

      expect(html.match(/<table>/g)).toHaveLength(2);
      expect(html).toContain('| Extra | Table |');
    });

    it('applies table expansion limits inside blockquotes', async () => {
      const lines = tableSource(128, 128).split('\n');
      const html = await renderMarkdown(lines.map((line) => `> ${line}`).join('\n'));

      expect(html).not.toContain('<table>');
      expect(html).not.toContain('<td>');
    });
  });

  describe('invisible spacing', () => {
    it('collapses lines made from encoded non-breaking spaces', async () => {
      const html = await renderMarkdown(`before\n${'&nbsp;\n'.repeat(500)}after`);

      expect(html).toContain('before');
      expect(html).toContain('after');
      expect(html).not.toContain('&nbsp;');
      expect(html.match(/<br>/g)).toHaveLength(1);
    });

    it('preserves word separation when normalizing non-breaking spaces', async () => {
      const html = await renderMarkdown('bread&nbsp;and&#160;butter');

      expect(html).toContain('<p>bread and butter</p>');
    });

    it('removes paragraphs made only from encoded non-breaking spaces', async () => {
      const html = await renderMarkdown(`before\n\n${'&nbsp;\n\n'.repeat(500)}after`);

      expect(html.match(/<p>/g)).toHaveLength(2);
      expect(html).toContain('<p>before</p>');
      expect(html).toContain('<p>after</p>');
    });

    it('preserves non-breaking-space entity source in code', async () => {
      const html = await renderMarkdown('`&nbsp;`\n\n```text\n&nbsp;\n```');

      expect(html).toContain('<code>&amp;nbsp;</code>');
      expect(html).toContain('<span class="line">&amp;nbsp;</span>');
    });

    it('preserves entity spellings Markdown-it does not decode', async () => {
      const html = await renderMarkdown('&NBSP;\n&#00000160;');

      expect(html).toContain('&amp;NBSP;');
      expect(html).toContain('&amp;#00000160;');
    });
  });

  describe('literal backslashes', () => {
    it('preserves the backslash in the shrug kaomoji', async () => {
      const html = await renderMarkdown('¯\\_(ツ)_/¯');
      expect(html).toContain('¯\\_(ツ)_/¯');
      expect(html).not.toContain('<em>');
    });

    it('preserves backslashes in Windows-style paths', async () => {
      const html = await renderMarkdown('C:\\Users\\foo');
      expect(html).toContain('C:\\Users\\foo');
    });
  });

  describe('emphasis at word boundaries', () => {
    it('renders `*italic*` as italic', async () => {
      const html = await renderMarkdown('*italic*');
      expect(html).toContain('<em>italic</em>');
    });

    it('renders `_italic_` as italic', async () => {
      const html = await renderMarkdown('_italic_');
      expect(html).toContain('<em>italic</em>');
    });

    it('renders `**bold**` as bold', async () => {
      const html = await renderMarkdown('**bold**');
      expect(html).toContain('<strong>bold</strong>');
    });

    it('renders italic surrounded by other text', async () => {
      const html = await renderMarkdown('hello *world* foo');
      expect(html).toContain('<em>world</em>');
    });

    it('renders `_...moo_` as italic with leading punctuation', async () => {
      const html = await renderMarkdown('_...moo_');
      expect(html).toContain('<em>...moo</em>');
    });

    it('renders `*...moo*` as italic with leading punctuation', async () => {
      const html = await renderMarkdown('*...moo*');
      expect(html).toContain('<em>...moo</em>');
    });

    it('renders `**...bold**` as bold with leading punctuation', async () => {
      const html = await renderMarkdown('**...bold**');
      expect(html).toContain('<strong>...bold</strong>');
    });

    it('renders `**foo:**` as bold with trailing colon', async () => {
      const html = await renderMarkdown('**foo:**');
      expect(html).toContain('<strong>foo:</strong>');
    });

    it('renders `__foo:__` as bold with trailing colon', async () => {
      const html = await renderMarkdown('__foo:__');
      expect(html).toContain('<strong>foo:</strong>');
    });

    it('renders `*foo:*` as italic with trailing colon', async () => {
      const html = await renderMarkdown('*foo:*');
      expect(html).toContain('<em>foo:</em>');
    });

    it('renders `_foo:_` as italic with trailing colon', async () => {
      const html = await renderMarkdown('_foo:_');
      expect(html).toContain('<em>foo:</em>');
    });

    it('renders `**foo:** bar` as bold followed by text', async () => {
      const html = await renderMarkdown('**foo:** bar');
      expect(html).toContain('<strong>foo:</strong>');
    });

    it('renders `**foo:** bar` inside a list item as bold', async () => {
      const html = await renderMarkdown('- **foo:** bar');
      expect(html).toContain('<strong>foo:</strong>');
    });

    it('renders both halves of `**foo:** bar **baz**` as bold', async () => {
      const html = await renderMarkdown('**foo:** bar **baz**');
      expect(html).toContain('<strong>foo:</strong>');
      expect(html).toContain('<strong>baz</strong>');
    });

    it('renders both halves of `**foo:** **bar**` as bold', async () => {
      const html = await renderMarkdown('**foo:** **bar**');
      expect(html).toContain('<strong>foo:</strong>');
      expect(html).toContain('<strong>bar</strong>');
    });

    it('renders bold text when the closing marker is followed by text', async () => {
      const html = await renderMarkdown('fsdfsd **fsdf**fdsf');
      expect(html).toContain('<strong>fsdf</strong>fdsf');
    });

    it('renders bold text embedded within surrounding text', async () => {
      const html = await renderMarkdown('fsdfsd**fsdf**fdsf');
      expect(html).toContain('fsdfsd<strong>fsdf</strong>fdsf');
    });
  });

  describe('emphasis suppressed when not at word boundaries', () => {
    it('does not italicize underscores between punctuation', async () => {
      const html = await renderMarkdown('_(ツ)_/¯');
      expect(html).toContain('_(ツ)_/¯');
      expect(html).not.toContain('<em>');
    });

    it('does not italicize asterisks between punctuation', async () => {
      const html = await renderMarkdown('*(ツ)*');
      expect(html).toContain('*(ツ)*');
      expect(html).not.toContain('<em>');
    });

    it('does not break snake_case identifiers', async () => {
      const html = await renderMarkdown('snake_case_name');
      expect(html).toContain('snake_case_name');
      expect(html).not.toContain('<em>');
    });

    it('does not italicize intraword asterisks', async () => {
      const html = await renderMarkdown('foo*bar*baz');
      expect(html).toContain('foo*bar*baz');
      expect(html).not.toContain('<em>');
    });
  });

  describe('code spans', () => {
    it('renders inline code', async () => {
      const html = await renderMarkdown('`code`');
      expect(html).toContain('<code>code</code>');
    });

    it('preserves literal markdown chars inside code spans', async () => {
      const html = await renderMarkdown('`*not bold*`');
      expect(html).toContain('<code>*not bold*</code>');
    });

    it('groups inline content in tight list items', async () => {
      const html = await renderMarkdown(
        '1. Connect to `/api/realtime` using `chatto.realtime.v1`.'
      );

      expect(html).toContain(
        '<li><span class="list-item-content">Connect to <code>/api/realtime</code> using <code>chatto.realtime.v1</code>.</span>'
      );
    });
  });

  describe('code blocks', () => {
    it('renders fenced code blocks with lowlight classes', async () => {
      const html = await renderMarkdown('```js\nconst x = 1;\n```');
      expect(html).toContain('<pre class="hljs markdown-code" data-language="js"');
      expect(html).toContain('language-js');
      expect(html).toContain('hljs-keyword');
      expect(html).toContain(
        '<span class="markdown-code-actions"><span class="markdown-code-language">js</span><button'
      );
      expect(html).toContain('data-markdown-copy');
    });

    it('does not render the fence delimiter newline as a blank code line', async () => {
      const html = await renderMarkdown('```js\nconst x = 1;\n```');
      expect(html.match(/class="line"/g)).toHaveLength(1);
    });

    it('does not add whitespace rows between rendered code lines', async () => {
      const html = await renderMarkdown('```js\nconst x = 1;\nconst y = 2;\n```');
      expect(html).not.toContain('</span>\n<span class="line">');
    });

    it('loads alias languages on demand while preserving the original label', async () => {
      const html = await renderMarkdown('```toml\nname = "chatto"\n```');
      expect(html).toContain('data-language="toml"');
      expect(html).toContain('language-toml');
      expect(html).toContain('hljs-attr');
    });

    it('derives aliases from Highlight.js supported language metadata', () => {
      expect(canHighlightCodeLanguage('pas')).toBe(true);
      expect(canHighlightCodeLanguage('notalanguage')).toBe(false);
    });

    it('registers aliases on the shared lowlight instance', async () => {
      await ensureCodeLanguageLoaded('js');
      expect(lowlight.registered('js')).toBe(true);
    });

    it('loads bundled languages lazily when they appear in a fence', async () => {
      expect(isCodeLanguageLoaded('1c')).toBe(false);

      const html = await renderMarkdown('```1c\nПроцедура Тест()\nКонецПроцедуры\n```');

      expect(isCodeLanguageLoaded('1c')).toBe(true);
      expect(html).toContain('data-language="1c"');
      expect(html).toContain('language-1c');
    });

    it('preserves unsupported language labels while rendering plain code', async () => {
      const html = await renderMarkdown('```notalanguage\nname = "chatto"\n```');
      expect(html).toContain('data-language="notalanguage"');
      expect(html).toContain('language-notalanguage');
      expect(html).toContain('name = &quot;chatto&quot;');
    });

    it('keeps the original fenced code as the escaped copy payload', async () => {
      const html = await renderMarkdown('```text\n\t<a title="x">&</a>\n```');

      expect(html).toContain('data-copy-source="\t&lt;a title=&quot;x&quot;&gt;&amp;&lt;/a&gt;\n"');
      expect(html).toContain('&lt;a title=&quot;x&quot;&gt;&amp;&lt;/a&gt;');
      expect(html).not.toContain('<a title="x">');
    });

    it('adds a copy button to indented blocks but not inline code', async () => {
      const html = await renderMarkdown('    first\n    second\n\n`inline`');

      expect(html).toContain('data-copy-source="first\nsecond\n"');
      expect(html).toContain('<span class="markdown-code-actions"><button');
      expect(html.match(/data-markdown-copy/g)).toHaveLength(1);
      expect(html).toContain('<code>inline</code>');
    });

    // 【本地改动，2026-10-03 恢复】回归测试：内联图片经代理重写、非 http(s) 降级
    // 为 #、javascript: 不产 <img>。发现背景：支持消息正文 ![]() 内联图片，且所有
    // 图片统一走 IMAGE_PROXY_BASE 代理（隐藏观看者 IP/Referer）。
    describe('inline images', () => {
      it('rewrites an http(s) image through the proxy, preserving path and original query', async () => {
        const html = await renderMarkdown('![cat](https://images.example.com/photos/cat.png?token=abc)');
        expect(html).toContain('<img src="https://proxy.moonchan.xyz/photos/cat.png?token=abc');
        expect(html).toContain('proxy_host=images.example.com');
        expect(html).toContain('proxy_scheme=https');
        expect(html).toContain('alt="cat"');
      });

      it('adds proxy_host and proxy_scheme when there is no original query', async () => {
        const html = await renderMarkdown('![cat](https://images.example.com/cat.png)');
        expect(html).toContain('<img src="https://proxy.moonchan.xyz/cat.png?proxy_host=images.example.com');
        expect(html).toContain('proxy_scheme=https');
      });

      it('keeps the fragment at the end, after the proxy params', async () => {
        const html = await renderMarkdown('![cat](https://images.example.com/cat.png#section)');
        expect(html).toContain('#section');
        expect(html.indexOf('#section')).toBeGreaterThan(html.indexOf('proxy_scheme=https'));
      });

      it('records the original scheme for http images', async () => {
        const html = await renderMarkdown('![cat](http://images.example.com/cat.png)');
        expect(html).toContain('proxy_scheme=http');
      });

      it('hardens the emitted img tag', async () => {
        const html = await renderMarkdown('![cat](https://images.example.com/cat.png)');
        expect(html).toContain('loading="lazy"');
        expect(html).toContain('referrerpolicy="no-referrer"');
        expect(html).toContain('rel="noopener noreferrer"');
      });

      it('neuters non-http(s) image sources to #', async () => {
        const html = await renderMarkdown('![x](/relative/cat.png)');
        expect(html).toContain('src="#"');
        expect(html).not.toContain('/relative/cat.png');
      });

      it('does not emit an image for javascript: sources', async () => {
        const html = await renderMarkdown('![x](javascript:alert(1))');
        expect(html).not.toContain('<img');
      });

      it('constrains image size with max-width and wraps in clean original url link', async () => {
        // 【本地改动 2026-10-19】发现背景：外层 <a> 硬编码 max-width: 50% 导致手机端
        // 图片过小看不清字，而 <img> 设 width: 100% 会导致 PC 端小图/竖图被强制拉伸或
        // 留白。改为 width: auto; height: auto 配合 max-width: 100% 与可显示区高度上限
        // min(70vh, calc(100dvh - 11rem))，保持比例且高宽最大不超过 100% 可显示区。
        const html = await renderMarkdown('![cat](https://images.example.com/cat.png#preview)');
        expect(html).toContain(
          'style="display: block; width: auto; height: auto; max-width: 100%; max-height: min(70vh, calc(100dvh - 11rem)); object-fit: contain; cursor: pointer;"'
        );
        expect(html).toContain(
          '<a href="https://images.example.com/cat.png" target="_blank" rel="noopener noreferrer" class="markdown-image-link" style="display: inline-block; max-width: 100%; vertical-align: bottom;">'
        );
      });

      it('does not wrap non-http(s) image sources such as ftp: in an anchor link', async () => {
        // 【本地改动 2026-10-19】发现背景：ftp:// 等非 http(s) 协议能通过 new URL() 解析，
        // 此前 image 渲染器未检查 proxiedSrc !== '#' 及 urlObj.protocol，导致 <img src="#">
        // 外层仍被包裹 <a href="ftp://...">。
        const html = await renderMarkdown('![x](ftp://images.example.com/cat.png)');
        expect(html).toContain('src="#"');
        expect(html).not.toContain('<a ');
        expect(html).not.toContain('ftp://');
      });

      it('does not emit nested anchor tags when an image is inside a markdown link', async () => {
        // 【本地改动 2026-10-19】发现背景：用户输入 `[![badge](img)](target)` 时，
        // 若 image 渲染器无条件再包一层 `<a href="img">`，会产生非法嵌套 `<a><a><img></a></a>`，
        // HTML5 解析器会强制提前闭合外层 `<a>`，导致点击图片跳往图床而非 target 链接。
        const html = await renderMarkdown(
          '[![badge](https://images.example.com/badge.svg)](https://example.com/docs)'
        );
        expect(html.match(/<a\b/g)).toHaveLength(1);
        expect(html).toContain('href="https://example.com/docs"');
        expect(html).toContain(
          'src="https://proxy.moonchan.xyz/badge.svg?proxy_host=images.example.com&amp;proxy_scheme=https"'
        );
      });
    });

    // 【本地改动，2026-10-03 恢复】回归测试：$...$ / $$...$$ 渲染为 KaTeX 公式，
    // 纯数字/金额不误触发。
    describe('math formulas', () => {
      it('renders an inline formula through KaTeX', async () => {
        const html = await renderMarkdown('公式 $a^2 + b^2 = c^2$ 结束');
        expect(html).toContain('<span class="katex">');
      });

      it('renders a display formula through KaTeX', async () => {
        const html = await renderMarkdown('$$\n\\int_0^1 x^2\\,dx\n$$');
        expect(html).toContain('<span class="katex-display">');
      });

      it('leaves money amounts and currency ranges untouched', async () => {
        // 【本地改动 2026-10-19】发现背景："from $10 to $20" 两个 $ 之间含有字母 "to"，
        // 此前因未校验闭合 $ 后紧跟数字的边界而被误判为行内公式。
        const html = await renderMarkdown('the price is $10.00 or from $10 to $20 with tax');
        expect(html).not.toContain('<span class="katex">');
        expect(html).toContain('$10.00');
        expect(html).toContain('from $10 to $20 with tax');
      });
    });
  });
});
