import { defineConfig, type DefaultTheme } from "vitepress";
import { withMermaid } from "vitepress-plugin-mermaid";
import { fileURLToPath } from "node:url";

const packageNames = [
  "agent",
  "openaicompat",
  "provider",
  "prompt",
  "context",
  "session",
  "message",
  "durable",
  "tool",
  "permission",
  "skills",
  "mcp",
  "subagent",
  "coordinator",
  "event",
  "app",
  "agenttest",
] as const;

function buildRewrites(prefix = "") {
  const p = prefix ? `${prefix}/` : "";
  const rewrites: Record<string, string> = {
    [`${p}README.md`]: `${p}index.md`,
    [`${p}quickstart.md`]: `${p}docs/quickstart.md`,
    [`${p}concepts.md`]: `${p}docs/concepts.md`,
    [`${p}internals.md`]: `${p}docs/internals.md`,
    [`${p}production.md`]: `${p}docs/production.md`,
    [`${p}icoder.md`]: `${p}docs/icoder.md`,
    [`${p}reference.md`]: `${p}docs/reference.md`,
  };
  for (const name of packageNames) {
    rewrites[`${p}packages/${name}.md`] = `${p}docs/packages/${name}.md`;
  }
  return rewrites;
}

const zhSidebar: DefaultTheme.Sidebar = [
  {
    text: "入门",
    items: [
      { text: "介绍", link: "/" },
      { text: "快速开始", link: "/docs/quickstart" },
      { text: "核心概念", link: "/docs/concepts" },
    ],
  },
  {
    text: "核心包",
    items: [
      { text: "agent", link: "/docs/packages/agent" },
      { text: "openaicompat", link: "/docs/packages/openaicompat" },
    ],
  },
  {
    text: "模型与提示",
    collapsed: false,
    items: [
      { text: "provider", link: "/docs/packages/provider" },
      { text: "prompt", link: "/docs/packages/prompt" },
      { text: "context", link: "/docs/packages/context" },
    ],
  },
  {
    text: "会话与持久化",
    collapsed: false,
    items: [
      { text: "session", link: "/docs/packages/session" },
      { text: "message", link: "/docs/packages/message" },
      { text: "durable", link: "/docs/packages/durable" },
    ],
  },
  {
    text: "工具与安全",
    collapsed: false,
    items: [
      { text: "tool", link: "/docs/packages/tool" },
      { text: "permission", link: "/docs/packages/permission" },
      { text: "skills", link: "/docs/packages/skills" },
      { text: "mcp", link: "/docs/packages/mcp" },
    ],
  },
  {
    text: "编排与运维",
    collapsed: false,
    items: [
      { text: "subagent", link: "/docs/packages/subagent" },
      { text: "coordinator", link: "/docs/packages/coordinator" },
      { text: "event", link: "/docs/packages/event" },
      { text: "app", link: "/docs/packages/app" },
      { text: "agenttest", link: "/docs/packages/agenttest" },
    ],
  },
  {
    text: "进阶",
    items: [
      { text: "运行内部机制", link: "/docs/internals" },
      { text: "生产组合", link: "/docs/production" },
      { text: "iCoder 教程", link: "/docs/icoder" },
      { text: "速查表", link: "/docs/reference" },
    ],
  },
];

const enSidebar: DefaultTheme.Sidebar = [
  {
    text: "Getting started",
    items: [
      { text: "Introduction", link: "/en/" },
      { text: "Quickstart", link: "/en/docs/quickstart" },
      { text: "Core concepts", link: "/en/docs/concepts" },
    ],
  },
  {
    text: "Core packages",
    items: [
      { text: "agent", link: "/en/docs/packages/agent" },
      { text: "openaicompat", link: "/en/docs/packages/openaicompat" },
    ],
  },
  {
    text: "Models & prompts",
    collapsed: false,
    items: [
      { text: "provider", link: "/en/docs/packages/provider" },
      { text: "prompt", link: "/en/docs/packages/prompt" },
      { text: "context", link: "/en/docs/packages/context" },
    ],
  },
  {
    text: "Sessions & persistence",
    collapsed: false,
    items: [
      { text: "session", link: "/en/docs/packages/session" },
      { text: "message", link: "/en/docs/packages/message" },
      { text: "durable", link: "/en/docs/packages/durable" },
    ],
  },
  {
    text: "Tools & safety",
    collapsed: false,
    items: [
      { text: "tool", link: "/en/docs/packages/tool" },
      { text: "permission", link: "/en/docs/packages/permission" },
      { text: "skills", link: "/en/docs/packages/skills" },
      { text: "mcp", link: "/en/docs/packages/mcp" },
    ],
  },
  {
    text: "Orchestration & ops",
    collapsed: false,
    items: [
      { text: "subagent", link: "/en/docs/packages/subagent" },
      { text: "coordinator", link: "/en/docs/packages/coordinator" },
      { text: "event", link: "/en/docs/packages/event" },
      { text: "app", link: "/en/docs/packages/app" },
      { text: "agenttest", link: "/en/docs/packages/agenttest" },
    ],
  },
  {
    text: "Advanced",
    items: [
      { text: "Runtime internals", link: "/en/docs/internals" },
      { text: "Production patterns", link: "/en/docs/production" },
      { text: "iCoder tutorial", link: "/en/docs/icoder" },
      { text: "Reference", link: "/en/docs/reference" },
    ],
  },
];

export default withMermaid(
  defineConfig({
    title: "Agent Runtime for Go",
    titleTemplate: ":title | Agent Runtime for Go",
    srcDir: "../docs",
    outDir: "../cmd/docs/dist",
    cleanUrls: true,
    appearance: true,
    rewrites: {
      ...buildRewrites(),
      ...buildRewrites("en"),
    },
    lastUpdated: true,
    head: [
      ["meta", { name: "theme-color", content: "#5b5bd6" }],
      ["link", { rel: "icon", type: "image/svg+xml", href: "/logo.svg" }],
    ],
    markdown: {
      lineNumbers: true,
    },
    vite: {
      publicDir: fileURLToPath(new URL("../public", import.meta.url)),
      resolve: {
        dedupe: ["vue"],
        alias: [
          { find: /^vue\/server-renderer$/, replacement: fileURLToPath(new URL("../node_modules/@vue/server-renderer/dist/server-renderer.esm-bundler.js", import.meta.url)) },
          { find: /^vue$/, replacement: fileURLToPath(new URL("../node_modules/vue/dist/vue.runtime.esm-bundler.js", import.meta.url)) },
        ],
      },
    },
    locales: {
      root: {
        label: "简体中文",
        lang: "zh-CN",
        description: "面向 Go 的可组合 Agent Runtime 开发文档",
        themeConfig: {
          siteTitle: "Agent Runtime for Go",
          logo: { src: "/logo.svg", alt: "" },
          nav: [
            { text: "指南", link: "/docs/quickstart", activeMatch: "^/docs/(quickstart|concepts|internals|production|icoder)" },
            { text: "包参考", link: "/docs/packages/agent", activeMatch: "^/docs/packages/" },
            { text: "速查表", link: "/docs/reference" },
          ],
          sidebar: zhSidebar,
          outline: { level: [2, 3], label: "本页目录" },
          search: {
            provider: "local",
            options: {
              translations: {
                button: { buttonText: "搜索文档", buttonAriaLabel: "搜索文档" },
                modal: {
                  noResultsText: "没有找到相关结果",
                  resetButtonTitle: "清除查询",
                  footer: { selectText: "选择", navigateText: "切换", closeText: "关闭" },
                },
              },
            },
          },
          socialLinks: [{ icon: "github", link: "https://github.com/iceymoss/agent-runtime-go" }],
          editLink: {
            pattern: "https://github.com/iceymoss/agent-runtime-go/edit/main/docs/:path",
            text: "在 GitHub 上编辑此页",
          },
          lastUpdated: { text: "最后更新于" },
          docFooter: { prev: "上一篇", next: "下一篇" },
          returnToTopLabel: "返回顶部",
          sidebarMenuLabel: "文档目录",
          darkModeSwitchLabel: "外观",
          lightModeSwitchTitle: "切换到浅色主题",
          darkModeSwitchTitle: "切换到深色主题",
        },
      },
      en: {
        label: "English",
        lang: "en-US",
        link: "/en/",
        description: "Composable agent runtime documentation for Go",
        themeConfig: {
          siteTitle: "Agent Runtime for Go",
          logo: { src: "/logo.svg", alt: "" },
          nav: [
            { text: "Guide", link: "/en/docs/quickstart", activeMatch: "^/en/docs/(quickstart|concepts|internals|production|icoder)" },
            { text: "Packages", link: "/en/docs/packages/agent", activeMatch: "^/en/docs/packages/" },
            { text: "Reference", link: "/en/docs/reference" },
          ],
          sidebar: enSidebar,
          outline: { level: [2, 3], label: "On this page" },
          search: {
            provider: "local",
          },
          socialLinks: [{ icon: "github", link: "https://github.com/iceymoss/agent-runtime-go" }],
          editLink: {
            pattern: "https://github.com/iceymoss/agent-runtime-go/edit/main/docs/:path",
            text: "Edit this page on GitHub",
          },
          lastUpdated: { text: "Last updated" },
          docFooter: { prev: "Previous", next: "Next" },
          returnToTopLabel: "Back to top",
          sidebarMenuLabel: "Menu",
          darkModeSwitchLabel: "Appearance",
          lightModeSwitchTitle: "Switch to light theme",
          darkModeSwitchTitle: "Switch to dark theme",
        },
      },
    },
    mermaid: {
      theme: "neutral",
      securityLevel: "strict",
    },
  }),
);
