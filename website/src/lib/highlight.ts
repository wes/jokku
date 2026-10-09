import type { HighlighterCore, ThemeRegistration } from 'shiki/core';
import { createHighlighterCore } from 'shiki/core';
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript';

// Code blocks are dark in both site themes, so one theme is enough.
const theme: ThemeRegistration = {
  name: 'jokku',
  type: 'dark',
  colors: { 'editor.background': '#0c0a07', 'editor.foreground': '#e6dccd' },
  settings: [
    { settings: { foreground: '#e6dccd', background: '#0c0a07' } },
    { scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: '#7d6f5e' } },
    { scope: ['entity.name.command', 'support.function.builtin'], settings: { foreground: '#f0a35a' } },
    { scope: ['string.unquoted', 'string.unquoted.plain.out.yaml'], settings: { foreground: '#efe6d8' } },
    { scope: ['constant.other.option'], settings: { foreground: '#8fd0c4' } },
    { scope: ['string.quoted', 'string.interpolated'], settings: { foreground: '#b9d98c' } },
    { scope: ['constant.numeric', 'constant.language'], settings: { foreground: '#f2c94c' } },
    { scope: ['variable', 'punctuation.definition.variable'], settings: { foreground: '#f2c94c' } },
    { scope: ['keyword.operator', 'keyword.control'], settings: { foreground: '#e8963f' } },
    { scope: ['entity.name.tag', 'support.type.property-name'], settings: { foreground: '#f0a35a' } },
    { scope: ['keyword.other', 'keyword', 'storage'], settings: { foreground: '#f0a35a' } },
    { scope: ['punctuation', 'meta.brace'], settings: { foreground: '#9a8b78' } },
  ],
};

const LANGS: Record<string, string> = {
  sh: 'shellscript',
  bash: 'shellscript',
  shell: 'shellscript',
  yaml: 'yaml',
  yml: 'yaml',
  dockerfile: 'dockerfile',
  json: 'json',
};

let highlighter: Promise<HighlighterCore> | undefined;

const getHighlighter = () =>
  (highlighter ??= createHighlighterCore({
    themes: [theme],
    langs: [
      import('shiki/langs/shellscript.mjs'),
      import('shiki/langs/yaml.mjs'),
      import('shiki/langs/dockerfile.mjs'),
      import('shiki/langs/json.mjs'),
    ],
    engine: createJavaScriptRegexEngine(),
  }));

export const isHighlighted = (lang: string) => lang in LANGS;

export async function highlight(code: string, lang: string) {
  const h = await getHighlighter();
  return h.codeToHtml(code, { lang: LANGS[lang] ?? 'text', theme: 'jokku' });
}
