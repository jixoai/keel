import DefaultTheme from 'vitepress/theme'
import './tokens.css'

// The default theme is kept whole: the site only re-skins it through the
// token overrides in tokens.css. No layout components are replaced or copied,
// so a VitePress upgrade stays a one-line dependency bump.
export default {
  extends: DefaultTheme
}
