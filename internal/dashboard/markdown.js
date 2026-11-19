// mdRender(src, doc) renders a small Markdown subset as DOM nodes and returns
// a DocumentFragment: headings, paragraphs, lists (nested by indentation),
// tables, fenced and inline code, **strong**, and links whose target is
// http(s). Every piece of user text reaches the page through createTextNode
// or textContent; markup in the source (<script>, <img onerror>) stays text,
// and a link to any other scheme (javascript:, data:) stays its source text.
// doc defaults to document; the unit tests (markdown.test.cjs) pass a fake.
function mdRender(src, doc) {
  doc = doc || document;
  const el = (tag, cls) => { const n = doc.createElement(tag); if (cls) n.className = cls; return n; };
  const text = s => doc.createTextNode(s);
  const blank = l => /^\s*$/.test(l);
  const indent = l => l.match(/^ */)[0].length;
  const fenceOpen = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)\s*$/;
  const heading = /^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$/;
  const item = /^( {0,3})([-*+]|\d{1,9}[.)])[ \t]+(.*)$/;
  const tableSep = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/;

  function safeURL(u) {
    try {
      const url = new URL(u);
      return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null;
    } catch (e) {
      return null;
    }
  }
  function link(label, href, parent, source) {
    const u = safeURL(href);
    if (!u) { parent.append(text(source)); return; }
    const a = el('a');
    a.setAttribute('href', u);
    a.setAttribute('target', '_blank');
    a.setAttribute('rel', 'noopener noreferrer');
    a.textContent = label;
    parent.append(a);
  }
  function inline(s, parent) {
    const re = /(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)|\[([^\]\n]+)\]\(\s*([^)\s]+)\s*\)|\*\*(?=\S)([\s\S]*?\S)\*\*|(https?:\/\/[^\s<>()]*[^\s<>().,;:!?'"])/g;
    let last = 0, m;
    while ((m = re.exec(s))) {
      if (m.index > last) parent.append(text(s.slice(last, m.index)));
      if (m[1]) {
        const c = el('code');
        c.textContent = m[2].length > 2 && m[2][0] === ' ' && m[2][m[2].length - 1] === ' ' ? m[2].slice(1, -1) : m[2];
        parent.append(c);
      } else if (m[3] !== undefined) {
        link(m[3], m[4], parent, m[0]);
      } else if (m[5] !== undefined) {
        const b = el('strong');
        inline(m[5], b);
        parent.append(b);
      } else {
        link(m[6], m[6], parent, m[0]);
      }
      last = re.lastIndex;
    }
    if (last < s.length) parent.append(text(s.slice(last)));
  }
  function cells(line) {
    let s = line.trim();
    if (s.startsWith('|')) s = s.slice(1);
    if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
    const out = [];
    let cur = '';
    for (let i = 0; i < s.length; i++) {
      if (s[i] === '\\' && s[i + 1] === '|') { cur += '|'; i++; continue; }
      if (s[i] === '|') { out.push(cur.trim()); cur = ''; continue; }
      cur += s[i];
    }
    out.push(cur.trim());
    return out;
  }
  function startsBlock(lines, i) {
    const l = lines[i];
    return fenceOpen.test(l) || heading.test(l) || item.test(l) ||
      (l.includes('|') && i + 1 < lines.length && tableSep.test(lines[i + 1]) && lines[i + 1].includes('-'));
  }
  function blocks(lines, parent) {
    let i = 0, m;
    while (i < lines.length) {
      const l = lines[i];
      if (blank(l)) { i++; continue; }
      if ((m = l.match(fenceOpen))) {
        const mark = m[1], code = [];
        i++;
        while (i < lines.length) {
          const t = lines[i].trim();
          if (t[0] === mark[0] && /^(`{3,}|~{3,})$/.test(t) && t.length >= mark.length) { i++; break; }
          code.push(lines[i]);
          i++;
        }
        const pre = el('pre'), c = el('code');
        c.textContent = code.join('\n');
        pre.append(c);
        parent.append(pre);
        continue;
      }
      if ((m = l.match(heading))) {
        // The detail title is the page's h2, so # is h3 and the rest follow.
        const h = el('h' + Math.min(6, m[1].length + 2));
        inline(m[2], h);
        parent.append(h);
        i++;
        continue;
      }
      if (l.includes('|') && i + 1 < lines.length && tableSep.test(lines[i + 1]) && lines[i + 1].includes('-')) {
        const head = cells(l), wrap = el('div', 'md-table'), table = el('table'), thead = el('thead'), tr = el('tr');
        for (const c of head) { const th = el('th'); inline(c, th); tr.append(th); }
        thead.append(tr);
        table.append(thead);
        const tbody = el('tbody');
        i += 2;
        while (i < lines.length && !blank(lines[i]) && lines[i].includes('|')) {
          const row = el('tr'), cs = cells(lines[i]);
          for (let k = 0; k < head.length; k++) { const td = el('td'); inline(cs[k] || '', td); row.append(td); }
          tbody.append(row);
          i++;
        }
        table.append(tbody);
        wrap.append(table);
        parent.append(wrap);
        continue;
      }
      if ((m = l.match(item))) {
        const ordered = /\d/.test(m[2]), list = el(ordered ? 'ol' : 'ul'), base = m[1].length;
        if (ordered && parseInt(m[2], 10) !== 1) list.setAttribute('start', String(parseInt(m[2], 10)));
        while (i < lines.length) {
          const im = lines[i].match(item);
          if (!im || im[1].length > base + 1 || /\d/.test(im[2]) !== ordered) break;
          const body = [im[3]], width = im[1].length + im[2].length + 1;
          i++;
          while (i < lines.length) {
            const n = lines[i];
            if (blank(n)) {
              let j = i + 1;
              while (j < lines.length && blank(lines[j])) j++;
              if (j < lines.length && indent(lines[j]) > base + 1) { body.push(''); i++; continue; }
              break;
            }
            if (indent(n) > base + 1) { body.push(n.slice(Math.min(indent(n), width))); i++; continue; }
            if (item.test(n) || startsBlock(lines, i)) break;
            body.push(n.trim()); // lazy continuation
            i++;
          }
          const li = el('li'), box = doc.createDocumentFragment();
          blocks(body, box);
          const kids = box.childNodes ? Array.from(box.childNodes) : [];
          if (kids.length && kids[0].tagName === 'P') {
            // A tight item: the first paragraph's content sits in the li.
            li.append(...Array.from(kids[0].childNodes));
            li.append(...kids.slice(1));
          } else {
            li.append(...kids);
          }
          list.append(li);
          while (i < lines.length && blank(lines[i])) {
            let j = i;
            while (j < lines.length && blank(lines[j])) j++;
            const nm = j < lines.length && lines[j].match(item);
            if (nm && nm[1].length <= base + 1 && /\d/.test(nm[2]) === ordered) i = j; else break;
          }
        }
        parent.append(list);
        continue;
      }
      const para = [l.trim()];
      i++;
      while (i < lines.length && !blank(lines[i]) && !startsBlock(lines, i)) { para.push(lines[i].trim()); i++; }
      const p = el('p');
      inline(para.join('\n'), p);
      parent.append(p);
    }
  }
  const out = doc.createDocumentFragment();
  blocks(String(src == null ? '' : src).replace(/\r\n?/g, '\n').replace(/\t/g, '    ').split('\n'), out);
  return out;
}
if (typeof module !== 'undefined') module.exports = { mdRender };
