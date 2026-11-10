import DefaultTheme from 'vitepress/theme'
import { h } from 'vue'
import { useData } from 'vitepress'
import './tokens.css'

// The default theme is kept whole: the site re-skins it through the token
// overrides in tokens.css and adds exactly one control — a close button for the
// mobile sidebar drawer (which now opens below the navbar, see the same file).
// No layout component is replaced or copied, so a VitePress upgrade stays a
// one-line dependency bump.
const SidebarClose = {
  name: 'KeelSidebarClose',
  setup() {
    const { lang } = useData()
    // The default theme keeps the drawer state in a Layout-local ref and
    // exposes no close hook; Escape is its documented close path (see
    // theme-default/composables/sidebar.js, useCloseSidebarOnEscape), so the
    // button reuses it instead of reaching into private state.
    function closeSidebar() {
      window.dispatchEvent(new KeyboardEvent('keyup', { key: 'Escape' }))
    }
    return () =>
      h(
        'button',
        { type: 'button', class: 'keel-sidebar-close', onClick: closeSidebar },
        [
          h('span', { 'aria-hidden': 'true' }, '\u2715'),
          h('span', null, lang.value?.startsWith('zh') ? '关闭' : 'Close')
        ]
      )
  }
}

export default {
  extends: DefaultTheme,
  Layout() {
    return h(DefaultTheme.Layout, null, {
      'sidebar-nav-before': () => h(SidebarClose)
    })
  }
}
