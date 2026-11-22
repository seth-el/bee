#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');

const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const scriptStart = html.indexOf('<script>\n') + '<script>\n'.length;
const scriptEnd = html.indexOf('\n</script>', scriptStart);
const script = html.slice(scriptStart, scriptEnd);
const start = script.indexOf('function appendInlineMarkdown');
const end = script.indexOf('function nearBottom');
assert(start >= 0 && end > start, 'renderer function slice not found');

class FakeNode {
  constructor(tagName, text = '') {
    this.tagName = tagName;
    this.childNodes = [];
    this.attributes = Object.create(null);
    this.dataset = Object.create(null);
    this.className = '';
    this.classList = { add: (...names) => { this.className = [this.className, ...names].filter(Boolean).join(' '); } };
    this.textContent = text;
    this.scope = '';
    this.href = '';
    this.target = '';
    this.rel = '';
  }
  appendChild(child) {
    this.childNodes.push(child);
    return child;
  }
  append(...children) {
    children.forEach((child) => this.appendChild(child));
  }
  replaceChildren(...children) {
    this.childNodes = [];
    this.append(...children);
  }
  addEventListener() {}
  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }
  getAttribute(name) {
    return this.attributes[name];
  }
}

const document = {
  createElement: (tagName) => new FakeNode(tagName),
  createTextNode: (text) => new FakeNode('#text', String(text)),
};
const factory = new Function('document', `${script.slice(start, end)}\nreturn { appendInlineMarkdown, parseMarkdownTable, renderMarkdown, renderMarkdownTable, splitMarkdownTableRow };`);
const api = factory(document);

function table(lines, start = 0) {
  return api.parseMarkdownTable(lines, start);
}
function text(node) {
  if (node.tagName === '#text') return node.textContent;
  return node.childNodes.map(text).join('');
}
function findAll(node, tagName, out = []) {
  if (node.tagName === tagName) out.push(node);
  node.childNodes.forEach((child) => findAll(child, tagName, out));
  return out;
}
function render(markdown) {
  const root = new FakeNode('div');
  api.renderMarkdown(root, markdown);
  return root;
}

const nvlink = table([
  '| GPU | NVLink | Bandwidth | Status |',
  '| --- | :--- | ---: | :---: |',
  '| H100 | NVLink 4 | 900 GB/s | ready |',
  '| B200 | NVLink 5 | 1.8 TB/s | **new** |',
]);
assert.deepStrictEqual(nvlink.header, ['GPU', 'NVLink', 'Bandwidth', 'Status']);
assert.deepStrictEqual(nvlink.alignments, ['left', 'left', 'right', 'center']);
assert.strictEqual(nvlink.rows.length, 2);

const inline = table([
  'Name | Detail | Value',
  ':--- | ---: | :---:',
  'GPU | `x|y` and **fast** | \\| 42',
]);
assert.deepStrictEqual(inline.rows[0], ['GPU', '`x|y` and **fast**', '\\| 42']);
assert.deepStrictEqual(api.splitMarkdownTableRow('a \\| b | c'), ['a \\| b', 'c']);
assert.deepStrictEqual(api.splitMarkdownTableRow('a \\\\| b | c'), ['a \\\\', 'b', 'c'], 'even backslashes leave a real delimiter');
assert.deepStrictEqual(api.splitMarkdownTableRow('a \\\\\\| b | c'), ['a \\\\\\| b', 'c'], 'odd backslashes escape the pipe');
assert.deepStrictEqual(table(['Name | Value', '--- | ---', 'literal \\` | right']).rows, [['literal \\`', 'right']], 'escaped backtick stays literal and does not open a code span');
assert.strictEqual(table(['a | b', '| nope | nope |', 'x | y']), null, 'malformed delimiter must fall back');
assert.strictEqual(table(['a | b', '--- | ---', 'x | y | z']), null, 'ragged rows must fall back');
const malformedRendered = render('a | b\n--- | ---\nx | y | z');
assert.strictEqual(findAll(malformedRendered, 'table').length, 0);
assert(text(malformedRendered).includes('x | y | z'));
assert.deepStrictEqual(table(['a | b', '--- | ---', 'prose \\| pipe']).rows, [], 'escaped-only prose should end a table without becoming a row');
assert.strictEqual(table(['a | b', '--- | ---', 'x | y', 'paragraph | with | pipe']), null, 'ragged adjacent pipe prose must fall back');
assert.strictEqual(table(['```', '| a | b |', '| --- | --- |', '| x | y |', '```']), null, 'fence marker cannot start a table');

const rendered = render('| GPU | Status |\n| --- | :---: |\n| H100 | **ready** |');
const wrappers = findAll(rendered, 'div').filter((node) => node.className === 'markdown-table-wrapper');
assert.strictEqual(wrappers.length, 1);
assert.strictEqual(findAll(rendered, 'div').filter((node) => node.className === 'markdown-table-frame').length, 1);
const scrollHints = findAll(rendered, 'span').filter((node) => node.className === 'markdown-table-scroll-hint');
assert.strictEqual(scrollHints.length, 1);
assert.strictEqual(scrollHints[0].textContent, '↔ scroll');
assert.strictEqual(wrappers[0].getAttribute('role'), 'region');
assert.strictEqual(wrappers[0].getAttribute('tabindex'), '0');
assert.strictEqual(wrappers[0].getAttribute('aria-label'), 'Scrollable Markdown table');
const tables = findAll(rendered, 'table');
assert.strictEqual(tables.length, 1);
const heads = findAll(tables[0], 'th');
assert.strictEqual(heads.length, 2);
assert.strictEqual(heads[0].scope, 'col');
assert.strictEqual(heads[1].className, 'markdown-table-align-center');
assert.strictEqual(findAll(tables[0], 'strong').length, 1);
assert.strictEqual(text(rendered).includes('H100'), true);

const adjacent = render('Before\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n- after\n\n```\n| C | D |\n| --- | --- |\n```\n\nAfter');
assert.strictEqual(findAll(adjacent, 'table').length, 1, 'only the non-fenced table should render');
assert.strictEqual(findAll(adjacent, 'ul').length, 1);
assert.strictEqual(findAll(adjacent, 'pre').length, 1);
const immediate = render('Before\nA | B\n--- | ---\n1 | 2');
assert.strictEqual(findAll(immediate, 'p').length, 1, 'prose before an immediate table remains a paragraph');
assert.strictEqual(findAll(immediate, 'table').length, 1, 'an immediate table is not swallowed into the prior paragraph');
const escapedRendered = render('A | B\n--- | ---\nleft \\\\\\| right | `x|y`');
assert(text(escapedRendered).includes('left \\| right'), 'odd backslash run renders one literal backslash plus pipe');
const multiTickRendered = render('A | B\n--- | ---\n``x|y`` | z');
const multiTickCode = findAll(multiTickRendered, 'code');
assert.strictEqual(multiTickCode.length, 1, 'matching multi-backtick spans render as one code node');
assert.strictEqual(multiTickCode[0].textContent, 'x|y');
const blockishRows = render('A | B\n--- | ---\n- item | yes\n# heading | yes\n1. ordered | yes\n> quote | yes');
assert.strictEqual(findAll(blockishRows, 'table').length, 1);
assert.strictEqual(findAll(blockishRows, 'tr').length, 5, 'unpiped rows beginning with block punctuation remain table rows');

const xss = render('| A | B |\n| --- | --- |\n| <script>alert(1)</script> | [x](javascript:alert(1)) |');
assert.strictEqual(findAll(xss, 'script').length, 0);
assert.strictEqual(findAll(xss, 'a').length, 0);
assert(text(xss).includes('<script>alert(1)</script>'));
assert(text(xss).includes('[x](javascript:alert(1))'));

const tooManyColumns = Array.from({ length: 33 }, (_, index) => `c${index}`).join(' | ');
const delimiter = Array.from({ length: 33 }, () => '---').join(' | ');
assert.strictEqual(table([tooManyColumns, delimiter]), null, 'column bound must fail safely');
assert.strictEqual(table(['a | b', '--- | ---', 'x'.repeat(4097) + ' | y']), null, 'cell bound must fail safely');
const manyRows = ['a | b', '--- | ---'];
for (let index = 0; index < 129; index += 1) manyRows.push(`${index} | value`);
assert.strictEqual(table(manyRows), null, 'row bound must fail safely');
const hugeRow = `${'x'.repeat(2048)} | ${'y'.repeat(2048)}`;
const hugeTable = ['a | b', '--- | ---'];
for (let index = 0; index < 20; index += 1) hugeTable.push(hugeRow);
assert.strictEqual(table(hugeTable), null, 'total table bound must fail safely');

console.log('markdown table parser/render tests: PASS');
