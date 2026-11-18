// Copies the dashboard screenshots the landing page shows from
// docs/assets/screenshots (their one home in the repository) into public/.
import { cpSync } from 'node:fs'

cpSync(
  new URL('../../docs/assets/screenshots/', import.meta.url),
  new URL('../public/screenshots/', import.meta.url),
  {
    recursive: true,
  },
)
