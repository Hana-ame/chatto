import { describe, expect, it } from 'vitest';
import { Schema } from '@tiptap/pm/model';
import type { Editor } from '@tiptap/core';
import {
  buildQuoteContent,
  createClipboardContent,
  getSerializedMarkdown,
  normalizeQuoteInsertionContent,
  prepareMarkdownForEditor
} from './markdown';

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

  // 【本地改动】回归测试：编辑框无 image 节点时，escapeImagesForEditor 用 \u2060 把 ![]() 当纯文本保留，
  // 普通链接不受影响；序列化时 normalizeSerializedImages 还原。发现背景：避免「编辑含内联图的消息」时图片丢失。
  it('keeps inline image syntax as literal text via an invisible marker', () => {
    const prepared = prepareMarkdownForEditor('see ![alt](https://example.com/a.png) here');
    // the marker sits right after the opening paren of the destination
    expect(prepared).toContain('](\u2060');
    expect(prepared).toContain('https://example.com/a.png');
  });

  it('does not escape plain links', () => {
    expect(prepareMarkdownForEditor('[alt](https://example.com)')).toBe('[alt](https://example.com)');
  });
});

describe('inline image serialization', () => {
  it('restores the original image source when serializing edited content', () => {
    const fakeEditor = {
      getMarkdown: () => 'see ![alt](\u2060https://example.com/a.png) here',
      state: {
        doc: { childCount: 1, lastChild: { type: { name: 'paragraph' }, content: { size: 5 } } }
      }
    } as unknown as Editor;
    const serialized = getSerializedMarkdown(fakeEditor);
    expect(serialized).toBe('see ![alt](https://example.com/a.png) here');
    expect(serialized).not.toContain('\u2060');
  });
});

describe('normalizeQuoteInsertionContent', () => {
  it('normalizes plain selected text into top-level quote blocks', () => {
    expect(normalizeQuoteInsertionContent(' First\r\nSecond ')).toEqual([
      { quoteDepth: 0, text: 'First' },
      { quoteDepth: 0, text: 'Second' }
    ]);
  });

  it('normalizes structured quote depth and drops empty blocks', () => {
    expect(
      normalizeQuoteInsertionContent([
        { quoteDepth: 1.9, text: ' Nested\r\nline ' },
        { quoteDepth: -3, text: ' Root ' },
        { quoteDepth: 4, text: '  ' }
      ])
    ).toEqual([
      { quoteDepth: 1, text: 'Nested\nline' },
      { quoteDepth: 0, text: 'Root' }
    ]);
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
