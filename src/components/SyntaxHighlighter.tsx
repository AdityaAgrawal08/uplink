"use client";

import hljs from "highlight.js";
import "highlight.js/styles/github-dark.css";

interface Props {
  code: string;
  language: string;
}

// Allow-list the language token: it originates from the uploader's filename,
// so strip everything that could break out of the class attribute.
function safeLanguage(language: string): string {
  return language.toLowerCase().replace(/[^a-z0-9_+-]/g, "").slice(0, 32);
}

export default function SyntaxHighlighter({ code, language }: Props) {
  const lang = safeLanguage(language);
  let highlightedValue = "";
  try {
    // highlight.js HTML-escapes the code content itself, making its output
    // safe to embed once the language token is allow-listed above.
    const result = hljs.highlight(code, { language: lang });
    highlightedValue = `<pre class="hljs"><code class="language-${lang}">${result.value}</code></pre>`;
  } catch {
    const result = hljs.highlightAuto(code);
    highlightedValue = `<pre class="hljs"><code>${result.value}</code></pre>`;
  }

  return <div dangerouslySetInnerHTML={{ __html: highlightedValue }} />;
}
