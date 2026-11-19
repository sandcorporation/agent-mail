// Unit tests for markdown.js: node --test internal/dashboard/markdown.test.cjs
// (markdown_test.go runs them under go test when node is installed).
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { mdRender } = require('./markdown.js');

// A fake document: elements record their tag, attributes and children. Any
// HTML-parsing entry point throws, so a renderer that used one fails here.
function fakeDoc() {
  const forbid = name => { throw new Error('renderer used ' + name); };
  class Node {
    constructor(type) { this.nodeType = type; this.childNodes = []; }
    append(...ns) {
      for (const n of ns) {
        if (!(n instanceof Node)) throw new Error('append of a non-node: ' + typeof n);
        if (n.nodeType === 11) { this.childNodes.push(...n.childNodes); n.childNodes = []; } else this.childNodes.push(n);
      }
    }
    get textContent() { return this.nodeType === 3 ? this.data : this.childNodes.map(n => n.textContent).join(''); }
    set textContent(v) {
      if (this.nodeType === 3) { this.data = String(v); return; }
      const t = new Node(3); t.data = String(v); this.childNodes = [t];
    }
  }
  class Element extends Node {
    constructor(tag) { super(1); this.tagName = tag.toUpperCase(); this.attrs = {}; this.className = ''; }
    setAttribute(k, v) { if (/^on/i.test(k)) forbid('an event attribute ' + k); this.attrs[k] = String(v); }
    set innerHTML(v) { forbid('innerHTML'); }
    set outerHTML(v) { forbid('outerHTML'); }
    insertAdjacentHTML() { forbid('insertAdjacentHTML'); }
  }
  return {
    createElement: tag => new Element(tag),
    createTextNode: s => { const t = new Node(3); t.data = String(s); return t; },
    createDocumentFragment: () => new Node(11),
    write: () => forbid('document.write'),
    createRange: () => forbid('createRange'),
  };
}

function render(src) { return mdRender(src, fakeDoc()); }
function all(n, out = []) { for (const c of n.childNodes) { if (c.nodeType === 1) out.push(c); all(c, out); } return out; }
function tags(n) { return all(n).map(e => e.tagName); }
function shape(n) {
  return n.childNodes.map(c => c.nodeType === 3 ? JSON.stringify(c.data) : c.tagName.toLowerCase() + (c.childNodes.length ? '(' + shape(c) + ')' : '')).join(' ');
}

test('headings, paragraphs and strong', () => {
  const f = render('# Title\n\nFirst line\nsecond line with **bold** text\n\n### Small');
  assert.equal(shape(f), 'h3("Title") p("First line\\nsecond line with " strong("bold") " text") h5("Small")');
});

test('lists, ordered and nested', () => {
  const f = render('- one\n- two\n  - two.a\n  - two.b\n- three\n\n3. c\n4. d');
  assert.equal(shape(f), 'ul(li("one") li("two" ul(li("two.a") li("two.b"))) li("three")) ol(li("c") li("d"))');
  assert.equal(all(f).find(e => e.tagName === 'OL').attrs.start, '3');
});

test('tables with escaped pipes and missing cells', () => {
  const f = render('| a | b |\n|---|:-:|\n| 1 | `x\\|y` |\n| 2 |\n\nafter');
  assert.equal(shape(f), 'div(table(thead(tr(th("a") th("b"))) tbody(tr(td("1") td(code("x|y"))) tr(td("2") td)))) p("after")');
  assert.equal(f.childNodes[0].className, 'md-table');
});

test('fenced and inline code keep their text', () => {
  const f = render('```go\nfunc main() {\n\t<b>x</b>\n}\n```\nuse `<i>` here');
  assert.equal(shape(f), 'pre(code("func main() {\\n    <b>x</b>\\n}")) p("use " code("<i>") " here")');
});

test('an unclosed fence runs to the end', () => {
  assert.equal(shape(render('~~~\na\n\nb')), 'pre(code("a\\n\\nb"))');
});

test('http(s) links become anchors', () => {
  const f = render('[docs](https://mail.example/a?b=1) and https://mail.example/x.');
  const a = all(f).filter(e => e.tagName === 'A');
  assert.equal(a.length, 2);
  assert.equal(a[0].attrs.href, 'https://mail.example/a?b=1');
  assert.equal(a[0].textContent, 'docs');
  assert.equal(a[0].attrs.rel, 'noopener noreferrer');
  assert.equal(a[1].attrs.href, 'https://mail.example/x');
  assert.equal(f.childNodes[0].textContent, 'docs and https://mail.example/x.');
});

test('markup in the source stays text', () => {
  for (const src of [
    '<script>alert(1)</script>',
    '<img src=x onerror=alert(1)>',
    '<a href="javascript:alert(1)">x</a>',
    '- <svg onload=alert(1)>',
    '| <iframe src=x> |\n|---|\n| <b>y</b> |',
    '# <script>alert(2)</script>',
  ]) {
    const f = render(src);
    for (const t of tags(f)) assert.ok(['P', 'UL', 'LI', 'H3', 'DIV', 'TABLE', 'THEAD', 'TBODY', 'TR', 'TH', 'TD'].includes(t), src + ' made ' + t);
    for (const name of ['<script>', '<img', '<a href', '<svg', '<iframe', '<b>']) {
      if (src.includes(name)) assert.ok(f.textContent.includes(name), src + ' lost ' + name);
    }
  }
  assert.equal(render('<script>alert(1)</script>').childNodes[0].textContent, '<script>alert(1)</script>');
  assert.equal(render('<img src=x onerror=alert(1)>').childNodes[0].textContent, '<img src=x onerror=alert(1)>');
});

test('links to other schemes stay their source text', () => {
  for (const href of ['javascript:alert(1)', 'JavaScript:alert(1)', 'data:text/html,x', 'vbscript:x', 'file:///etc/passwd', '/relative', '//mail.example/x']) {
    const src = '[click](' + href + ')';
    const f = render(src);
    assert.equal(all(f).filter(e => e.tagName === 'A').length, 0, href);
    assert.equal(f.textContent, src);
  }
  assert.equal(all(render('java\tscript:alert(1) jav&#x61;script:x')).filter(e => e.tagName === 'A').length, 0);
});

test('empty and null input', () => {
  assert.equal(render('').childNodes.length, 0);
  assert.equal(render(null).childNodes.length, 0);
  assert.equal(shape(render('\r\n\r\nx\r\n')), 'p("x")');
});
