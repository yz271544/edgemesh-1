import type { SidebarConfig } from '@vuepress/theme-default'

const guide = {
  text: '指南',
  children: [
    '/zh/guide/README.md',
    '/zh/guide/getting-started.md',
    '/zh/guide/test-case.md',
    '/zh/guide/edge-gateway.md',
    '/zh/guide/security.md',
    '/zh/guide/ssh.md',
  ],
}

const reference = {
  text: '参考',
  children: [
    '/zh/reference/config-items.md',
  ],
}

const advanced = {
  text: '深入',
  children: [
    '/zh/advanced/architecture.md',
    '/zh/advanced/hybirdnat.md',
  ],
}

export const zh: SidebarConfig = {
  '/zh/': [
    guide,
    reference,
  ],
  '/zh/guide/': [
    guide,
    reference,
  ],
  '/zh/advanced/': [
    advanced,
  ]
}
