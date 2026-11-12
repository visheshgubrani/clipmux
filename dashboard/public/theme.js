// Applies the owner's theme before the first paint (docs/design.md, "Theme").
// An external file because the dashboard's CSP refuses inline scripts.
;(() => {
  try {
    const t = localStorage.getItem('clipmux-theme')
    const dark =
      t === 'dark' || ((!t || t === 'system') && matchMedia('(prefers-color-scheme: dark)').matches)
    document.documentElement.classList.toggle('dark', dark)
  } catch {
    // Storage refused (a private window): the light theme stands.
  }
})()
