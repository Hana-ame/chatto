import { describe, expect, it } from 'vitest';
import { Schema } from '@tiptap/pm/model';
import type { Editor } from '@tiptap/core';
import {
  buildQuoteContent,
  createClipboardContent,
  // 【本地改动】getSerializedMarkdown 服务于内联图片序列化回归测试
  // （inline image serialization）；isHttpMarkdownAutolink 是 upstream 新增。
  // 2026-08-29 合并 upstream：两者都保留。本地原有的
  // normalizeQuoteInsertionContent 已由 upstream 删除（markdown.ts 不再导出该函数，
  // 相关测试也一并移除），因此不能保留此导入，否则 TS 报未定义导出。
  getSerializedMarkdown,
  isHttpMarkdownAutolink,
  prepareMarkdownForEditor
} from './markdown';

describe('isHttpMarkdownAutolink', () => {
  it('accepts only complete angle-bracket HTTP(S) URLs', () => {
    expect(isHttpMarkdownAutolink('<https://example.com/story>')).toBe(true);
    expect(isHttpMarkdownAutolink('<HTTP://example.com/story>')).toBe(true);
    expect(isHttpMarkdownAutolink('<example.com>')).toBe(false);
    expect(isHttpMarkdownAutolink('<https://example.com/story')).toBe(false);
    expect(isHttpMarkdownAutolink('See <https://example.com/story>')).toBe(false);
  });
});

describe('prepareMarkdownForEditor', () => {
  it('escapes HTML-looking prose without changing link destinations', () => {
    expect(
      prepareMarkdownForEditor(
        'Use <section> & [open](https://example.com/?first=1&second=2) safely'
      )
    ).toBe('Use &lt;section> &amp; [open](https://example.com/?first=1&second=2) safely');
  });

  it('preserves HTML-looking text inside inline and fenced code', () => {
    const markdown = ['`<inline> & value`', '', '```html', '<section> & value', '```'].join('\n');

    expect(prepareMarkdownForEditor(markdown)).toBe(markdown);
  });

  it('keeps GFM table source editable without changing its visible text', () => {
    const prepared = prepareMarkdownForEditor('| One | Two |\n| --- | --- |\n| A | B |');

    expect(prepared).toBe('| One | Two |\n| -\u2060-- | --- |\n| A | B |');
  });

  // 【本地改动 2026-10-19 修正】回归测试：编辑框无 image 节点时，escapeImagesForEditor
  // 在 `]` 与 `(` 之间以及 `(` 之后插入 \u2060，同时阻断 marked 的 image token 解析与
  // GFM 裸 URL autolink；普通链接不受影响；序列化时 normalizeSerializedImages 还原。
  // 发现背景：此前仅在 `(` 后插入 \u2060，marked 仍把 `![alt](\u2060url)` 当作 image token
  // 丢弃，且 TipTap 序列化时将 `[`/`]` 转义为 `\[`/`\]` 后被编码为 `&#91;`/`&#93;`。
  it('keeps inline image syntax as literal text via invisible markers', () => {
    const prepared = prepareMarkdownForEditor('see ![alt](https://example.com/a.png) here');
    // markers sit between `]` and `(` and right after `(`
    expect(prepared).toContain(']\u2060(\u2060');
    expect(prepared).toContain('https://example.com/a.png');
  });

  it('does not escape plain links', () => {
    expect(prepareMarkdownForEditor('[alt](https://example.com)')).toBe('[alt](https://example.com)');
  });
});

describe('inline image serialization', () => {
  it('restores the original image source when serializing edited content', () => {
    const fakeEditor = {
      getMarkdown: () => 'see !\\[alt\\]\u2060(\u2060https://example.com/a\\_b.png) here',
      state: {
        doc: { childCount: 1, lastChild: { type: { name: 'paragraph' }, content: { size: 5 } } }
      }
    } as unknown as Editor;
    const serialized = getSerializedMarkdown(fakeEditor);
    expect(serialized).toBe('see ![alt](https://example.com/a_b.png) here');
    expect(serialized).not.toContain('\u2060');
  });

  it('restores typed inline image syntax even when brackets or URLs were escaped or autolinked', () => {
    // 【本地改动 2026-10-19】发现背景：用户在富文本模式手打 `![alt](https://...)` 时，
    // TipTap 会转义 `\[`/`\]` 或把 URL 自动包成 `[url](url)`，导致发出后变成普通文本。
    const escapedEditor = {
      getMarkdown: () => 'look !\\[my\\_cat\\]([https://example.com/my_cat.png](https://example.com/my_cat.png))!',
      state: {
        doc: { childCount: 1, lastChild: { type: { name: 'paragraph' }, content: { size: 5 } } }
      }
    } as unknown as Editor;
    expect(getSerializedMarkdown(escapedEditor)).toBe(
      'look ![my_cat](https://example.com/my_cat.png)!'
    );
  });
});

describe('createClipboardContent', () => {
  it('preserves destination marks while converting Markdown links and line breaks', () => {
    const schema = new Schema({
      nodes: {
        doc: { content: 'block+' },
        paragraph: { content: 'inline*', group: 'block' },
        text: { group: 'inline' },
        hardBreak: { inline: true, group: 'inline' }
      },
      marks: {
        strong: {},
        link: { attrs: { href: {} }, inclusive: false }
      }
    });
    const strong = schema.marks.strong.create();

    const content = createClipboardContent(
      'Before [Chatto](https://chatto.dev)\nafter\n\nSecond',
      schema,
      [strong]
    );

    expect(content.toJSON()).toEqual([
      {
        type: 'paragraph',
        content: [
          { type: 'text', marks: [{ type: 'strong' }], text: 'Before ' },
          {
            type: 'text',
            marks: [{ type: 'strong' }, { type: 'link', attrs: { href: 'https://chatto.dev' } }],
            text: 'Chatto'
          },
          { type: 'hardBreak' },
          { type: 'text', marks: [{ type: 'strong' }], text: 'after' }
        ]
      },
      {
        type: 'paragraph',
        content: [{ type: 'text', marks: [{ type: 'strong' }], text: 'Second' }]
      }
    ]);
  });
});

describe('buildQuoteContent', () => {
  it('builds nested TipTap blockquote content without flattening line breaks', () => {
    expect(
      buildQuoteContent([
        { quoteDepth: 0, text: 'Root' },
        { quoteDepth: 1, text: 'Nested\nline' },
        { quoteDepth: 2, text: 'Deep' },
        { quoteDepth: 0, text: 'Tail' }
      ])
    ).toEqual([
      {
        type: 'paragraph',
        content: [{ type: 'text', text: 'Root' }]
      },
      {
        type: 'blockquote',
        content: [
          {
            type: 'paragraph',
            content: [{ type: 'text', text: 'Nested' }]
          },
          {
            type: 'paragraph',
            content: [{ type: 'text', text: 'line' }]
          },
          {
            type: 'blockquote',
            content: [
              {
                type: 'paragraph',
                content: [{ type: 'text', text: 'Deep' }]
              }
            ]
          }
        ]
      },
      {
        type: 'paragraph',
        content: [{ type: 'text', text: 'Tail' }]
      }
    ]);
  });
});
