// Reading a frame's body as undifferentiated grey is the difference between
// seeing the code and scanning it, and both pages that show source need that.
// It lives on its own so neither page has to load the other's renderer.

// highlight is a deliberately small tokenizer: comments, strings, numbers and
// keywords, for the languages this tool parses. It is not a parser and does not
// pretend to be — but reading a frame's body as undifferentiated grey is the
// difference between seeing the code and scanning it.
const KEYWORDS = {
  go: 'break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false error string int int64 bool byte rune any',
  python: 'and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False self',
  javascript: 'async await break case catch class const continue default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while yield null undefined true false',
  typescript: 'async await break case catch class const continue default delete do else export extends finally for from function if import in instanceof interface let new of return static super switch this throw try type typeof var void while yield null undefined true false',
  yaml: 'true false null yes no',
};

function languageOf(path) {
  const ext = (path || '').split('.').pop().toLowerCase();
  if (ext === 'go') return 'go';
  if (ext === 'py' || ext === 'pyi') return 'python';
  if (ext === 'ts' || ext === 'tsx') return 'typescript';
  if (['js', 'jsx', 'mjs', 'cjs'].includes(ext)) return 'javascript';
  if (['yaml', 'yml', 'toml', 'json', 'env', 'ini'].includes(ext)) return 'yaml';
  return '';
}

// codeTokens splits one line into labelled pieces: strings, numbers, keywords,
// comments, and — the reason this exists apart from highlighting — identifiers.
//
// An identifier is the unit a reader points at. "Where does n come from" and
// "where does cutoff go" are questions about a name, so the name has to survive
// as something the page can attach a click to rather than being flattened into
// the same text node as the punctuation around it.
function codeTokens(line, lang) {
  const words = new Set((KEYWORDS[lang] || '').split(' '));
  const lineComment = lang === 'python' || lang === 'yaml' ? '#' : '//';
  const out = [];

  // A comment runs to end of line, but only outside a string.
  let inStr = 0, commentAt = -1;
  for (let j = 0; j < line.length; j++) {
    const c = line[j];
    if (inStr) { if (c === '\\') j++; else if (c === inStr) inStr = 0; continue; }
    if (c === '"' || c === "'" || c === '`') { inStr = c; continue; }
    if (line.startsWith(lineComment, j)) { commentAt = j; break; }
  }
  const body = commentAt >= 0 ? line.slice(0, commentAt) : line;
  const comment = commentAt >= 0 ? line.slice(commentAt) : '';

  const token = /("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`(?:[^`\\]|\\.)*`|\b\d[\d_.]*\b|[A-Za-z_$][\w$]*)/g;
  let i = 0, m;
  while ((m = token.exec(body)) !== null) {
    if (m.index > i) out.push({ kind: '', text: body.slice(i, m.index) });
    const t = m[0];
    if (/^["'`]/.test(t)) out.push({ kind: 'hl-str', text: t });
    else if (/^\d/.test(t)) out.push({ kind: 'hl-num', text: t });
    else if (words.has(t)) out.push({ kind: 'hl-kw', text: t });
    else out.push({ kind: 'id', text: t });
    i = m.index + t.length;
  }
  if (i < body.length) out.push({ kind: '', text: body.slice(i) });
  if (comment) out.push({ kind: 'hl-com', text: comment });
  return out;
}

function highlight(code, lang) {
  const out = document.createDocumentFragment();
  for (const line of code.split('\n')) {
    for (const t of codeTokens(line, lang)) {
      // An identifier is not coloured on its own; only the page that makes them
      // clickable has a reason to treat them differently from punctuation.
      if (!t.kind || t.kind === 'id') {
        out.appendChild(document.createTextNode(t.text));
        continue;
      }
      const el = document.createElement('span');
      el.className = t.kind;
      el.textContent = t.text;
      out.appendChild(el);
    }
    out.appendChild(document.createTextNode('\n'));
  }
  return out;
}
