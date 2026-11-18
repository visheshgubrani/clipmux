// The syntax palette for every code block, kept as plain data so the colours
// are reviewable here: violet keywords, graphite comments, ink text, on the
// Screen surface (the dashboard's tokens, D098). Strings take the teal from
// the dashboard's chart series.

function theme(name: string, type: 'light' | 'dark', c: Record<string, string>) {
  return {
    name,
    type,
    colors: { 'editor.background': c.bg, 'editor.foreground': c.ink },
    tokenColors: [
      { scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: c.comment } },
      {
        scope: ['keyword', 'storage', 'storage.type', 'storage.modifier', 'keyword.operator.new'],
        settings: { foreground: c.keyword },
      },
      {
        scope: ['string', 'string.quoted', 'punctuation.definition.string'],
        settings: { foreground: c.string },
      },
      {
        scope: ['constant.numeric', 'constant.language', 'support.constant'],
        settings: { foreground: c.constant },
      },
      { scope: ['entity.name.function', 'support.function'], settings: { foreground: c.ink } },
      { scope: ['entity.name.tag', 'support.class.component'], settings: { foreground: c.keyword } },
      { scope: ['entity.other.attribute-name'], settings: { foreground: c.comment } },
      { scope: ['punctuation', 'meta.brace'], settings: { foreground: c.comment } },
    ],
  }
}

export const lightTheme = theme('clipmux-light', 'light', {
  bg: '#ffffff',
  ink: '#18171e',
  comment: '#6b6a75',
  keyword: '#6d28d9',
  string: '#0f766e',
  constant: '#b45309',
})

export const darkTheme = theme('clipmux-dark', 'dark', {
  bg: '#1a1922',
  ink: '#edecf3',
  comment: '#a3a2ae',
  keyword: '#a78bfa',
  string: '#5eead4',
  constant: '#fbbf24',
})
