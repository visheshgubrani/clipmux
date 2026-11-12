import * as React from 'react'

export type Theme = 'light' | 'dark' | 'system'

const KEY = 'clipmux-theme'
const media = () => window.matchMedia('(prefers-color-scheme: dark)')

function read(): Theme {
  try {
    const t = localStorage.getItem(KEY)
    return t === 'light' || t === 'dark' ? t : 'system'
  } catch {
    return 'system'
  }
}

function apply(t: Theme) {
  document.documentElement.classList.toggle('dark', t === 'dark' || (t === 'system' && media().matches))
}

/** Light, Dark or System, remembered per browser (public/theme.js applies it before paint). */
export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = React.useState<Theme>(read)
  React.useEffect(() => {
    apply(theme)
    if (theme !== 'system') return
    const m = media()
    const onChange = () => apply('system')
    m.addEventListener('change', onChange)
    return () => m.removeEventListener('change', onChange)
  }, [theme])
  const set = (t: Theme) => {
    try {
      localStorage.setItem(KEY, t)
    } catch {
      // Private windows may refuse storage; the choice lasts this visit.
    }
    setTheme(t)
  }
  return [theme, set]
}
