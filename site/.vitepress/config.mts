import { defineConfig } from 'vitepress'

// The English tree is the site root (/), the Chinese tree lives under /zh/.
// Both locales carry the same page set at the same paths: the parity gate in
// site/scripts/parity.mjs fails the build when a page exists on one side only.
const coreSidebarEn = [
  {
    text: 'Core · Overview',
    items: [
      { text: 'What Keel is', link: '/core/' },
      { text: 'Architecture', link: '/core/architecture' },
      { text: 'Events and protocol', link: '/core/events' },
      { text: 'AgentAPI reference', link: '/core/agentapi' },
      { text: 'Storage and runtime', link: '/core/storage' }
    ]
  },
  {
    text: 'Core · Extending',
    items: [
      { text: 'Extension map', link: '/core/extend/' },
      { text: 'Embed the runtime', link: '/core/extend/embed-runtime' },
      { text: 'Write a service descriptor', link: '/core/extend/service-descriptor' },
      { text: 'Connect an external backend', link: '/core/extend/external-backend' },
      { text: 'Write a provider adapter', link: '/core/extend/provider-adapter' },
      { text: 'Test and accept', link: '/core/extend/testing' }
    ]
  }
]

const harnessSidebarEn = [
  {
    text: 'Harness · Overview',
    items: [
      { text: 'What the Harness is', link: '/harness/' },
      { text: 'Architecture', link: '/harness/architecture' },
      { text: 'Anatomy of a turn', link: '/harness/anatomy-of-a-turn' },
      { text: 'Streaming and partials', link: '/harness/streaming-and-partial' },
      { text: 'Approvals and questions', link: '/harness/approvals-and-questions' },
      { text: 'Recovery and compaction', link: '/harness/recovery-and-compaction' }
    ]
  },
  {
    text: 'Harness · Plugin standard',
    items: [
      { text: 'Plugins', link: '/harness/plugins/' },
      { text: 'Manifest', link: '/harness/plugins/manifest' },
      { text: 'Plugin kinds', link: '/harness/plugins/kinds' },
      { text: 'Lifecycle and effects', link: '/harness/plugins/lifecycle' },
      { text: 'Permissions and audit', link: '/harness/plugins/permissions' },
      { text: 'UI surfaces', link: '/harness/plugins/ui' },
      { text: 'Testing and conformance', link: '/harness/plugins/testing' }
    ]
  },
  {
    text: 'Harness · Operating',
    items: [
      { text: 'Creation mode', link: '/harness/creation-mode' },
      { text: 'Operations', link: '/harness/operations' }
    ]
  }
]

const glossaryEn = [{ text: 'Glossary', link: '/glossary' }]

const coreSidebarZh = [
  {
    text: 'Core · 总览',
    items: [
      { text: 'keel 是什么', link: '/zh/core/' },
      { text: '架构', link: '/zh/core/architecture' },
      { text: '事件与协议', link: '/zh/core/events' },
      { text: 'AgentAPI 参考', link: '/zh/core/agentapi' },
      { text: '存储与运行', link: '/zh/core/storage' }
    ]
  },
  {
    text: 'Core · 二次开发',
    items: [
      { text: '扩展地图', link: '/zh/core/extend/' },
      { text: '嵌入 runtime', link: '/zh/core/extend/embed-runtime' },
      { text: '写一个服务描述符', link: '/zh/core/extend/service-descriptor' },
      { text: '接外部 backend', link: '/zh/core/extend/external-backend' },
      { text: '写 provider 适配器', link: '/zh/core/extend/provider-adapter' },
      { text: '测试与验收', link: '/zh/core/extend/testing' }
    ]
  }
]

const harnessSidebarZh = [
  {
    text: 'Harness · 总览',
    items: [
      { text: 'Harness 是什么', link: '/zh/harness/' },
      { text: '架构', link: '/zh/harness/architecture' },
      { text: '一个回合的解剖', link: '/zh/harness/anatomy-of-a-turn' },
      { text: '流式与 partial', link: '/zh/harness/streaming-and-partial' },
      { text: '审批与问句', link: '/zh/harness/approvals-and-questions' },
      { text: '恢复与压缩', link: '/zh/harness/recovery-and-compaction' }
    ]
  },
  {
    text: 'Harness · 插件标准',
    items: [
      { text: '插件', link: '/zh/harness/plugins/' },
      { text: '清单', link: '/zh/harness/plugins/manifest' },
      { text: '插件种类', link: '/zh/harness/plugins/kinds' },
      { text: '生命周期与副作用', link: '/zh/harness/plugins/lifecycle' },
      { text: '权限与审计', link: '/zh/harness/plugins/permissions' },
      { text: 'UI 承载', link: '/zh/harness/plugins/ui' },
      { text: '测试与 conformance', link: '/zh/harness/plugins/testing' }
    ]
  },
  {
    text: 'Harness · 运维',
    items: [
      { text: '创造模式', link: '/zh/harness/creation-mode' },
      { text: '运维', link: '/zh/harness/operations' }
    ]
  }
]

const glossaryZh = [{ text: '术语表', link: '/zh/glossary' }]

const searchOptionsEn = {
  translations: {
    button: { buttonText: 'Search', buttonAriaLabel: 'Search' },
    modal: {
      displayDetails: 'Display detailed list',
      resetButtonTitle: 'Reset search',
      backButtonTitle: 'Close search',
      noResultsText: 'No results for',
      footer: {
        selectText: 'to select',
        selectKeyAriaLabel: 'enter',
        navigateText: 'to navigate',
        navigateUpKeyAriaLabel: 'up arrow',
        navigateDownKeyAriaLabel: 'down arrow',
        closeText: 'to close',
        closeKeyAriaLabel: 'escape'
      }
    }
  }
}

const searchOptionsZh = {
  translations: {
    button: { buttonText: '搜索文档', buttonAriaLabel: '搜索文档' },
    modal: {
      displayDetails: '显示详情列表',
      resetButtonTitle: '重置搜索',
      backButtonTitle: '关闭搜索',
      noResultsText: '没有找到相关内容',
      footer: {
        selectText: '选择',
        selectKeyAriaLabel: '回车',
        navigateText: '切换',
        navigateUpKeyAriaLabel: '上箭头',
        navigateDownKeyAriaLabel: '下箭头',
        closeText: '关闭',
        closeKeyAriaLabel: 'Esc'
      }
    }
  }
}

export default defineConfig({
  title: 'Keel',
  description:
    'Keel is an open-source, stateless, multi-tenant agent runtime in Go: an event-ledger core plus a Harness product layer.',
  srcDir: 'docs',
  // Output lands in site/dist (the Pages artifact path the workflow uploads).
  outDir: 'dist',
  cleanUrls: true,
  appearance: 'dark',
  lastUpdated: false,
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }],
    ['meta', { name: 'theme-color', content: '#161616' }],
    ['meta', { property: 'og:title', content: 'Keel — ledger core + Harness' }]
  ],
  themeConfig: {
    siteTitle: 'Keel',
    outline: { level: [2, 3] },
    socialLinks: [{ icon: 'github', link: 'https://github.com/jixoai/keel' }],
    footer: {
      message: 'Apache-2.0 (MIT notices kept for files moved from pigo)',
      copyright: 'Keel — open-source agent runtime'
    },
    search: { provider: 'local', options: searchOptionsEn },
    nav: [
      { text: 'Core', link: '/core/' },
      { text: 'Harness', link: '/harness/' },
      { text: 'Glossary', link: '/glossary' },
      { text: 'GitHub', link: 'https://github.com/jixoai/keel' }
    ],
    sidebar: {
      '/core/': coreSidebarEn,
      '/harness/': harnessSidebarEn,
      '/glossary': glossaryEn
    }
  },
  locales: {
    root: {
      label: 'English',
      lang: 'en-US'
    },
    zh: {
      label: '简体中文',
      lang: 'zh-CN',
      link: '/zh/',
      description:
        'Keel 是一个开源的、无状态的、多租户 Go 智能体运行时：事件账本内核 + Harness 产品层。',
      themeConfig: {
        outline: { level: [2, 3], label: '本页目录' },
        docFooter: { prev: '上一页', next: '下一页' },
        darkModeSwitchLabel: '外观',
        sidebarMenuLabel: '目录',
        returnToTopLabel: '回到顶部',
        langMenuLabel: '切换语言',
        footer: {
          message: 'Apache-2.0（自 pigo 搬运的文件保留 MIT 声明）',
          copyright: 'Keel —— 开源智能体运行时'
        },
        search: { provider: 'local', options: searchOptionsZh },
        nav: [
          { text: 'Core', link: '/zh/core/' },
          { text: 'Harness', link: '/zh/harness/' },
          { text: '术语表', link: '/zh/glossary' },
          { text: 'GitHub', link: 'https://github.com/jixoai/keel' }
        ],
        sidebar: {
          '/zh/core/': coreSidebarZh,
          '/zh/harness/': harnessSidebarZh,
          '/zh/glossary': glossaryZh
        }
      }
    }
  }
})
