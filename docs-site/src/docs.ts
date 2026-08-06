export type Chapter = {
  slug: string;
  index: string;
  title: string;
  shortTitle: string;
  description: string;
  content: string;
};

const sources = import.meta.glob("../../docs/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const chapterData = [
  ["", "00", "Agent Runtime for Go 文档", "文档首页", "从运行循环到生产组合，按源码理解整个 SDK。", "README.md"],
  ["introduction", "01", "简介：定位、能力与边界", "简介", "先确认 runtime 负责什么，以及刻意不负责什么。", "01-introduction.md"],
  ["quick-start", "02", "快速开始：最小可运行 Agent", "快速开始", "运行本地模型，再完成一次真实的工具循环。", "02-quick-start.md"],
  ["overview", "03", "总览：核心概念与包地图", "总览", "建立核心类型、运行模式和可选模块的整体认识。", "03-overview.md"],
  ["architecture", "04", "架构：端口、适配器与组合", "架构", "理解依赖倒置、不可变 generation 和组合边界。", "04-architecture.md"],
  ["runtime-internals", "05", "实现原理：运行循环与 Durable 边界", "实现原理", "深入流协议、工具执行、停止语义和恢复状态机。", "05-runtime-internals.md"],
  ["root-package", "06", "根包核心内容", "根包", "逐项查阅 package agent 的公开 API 与示例。", "06-root-package.md"],
  ["subpackages", "07", "子包职责与接入指南", "子包", "覆盖仓库全部子包、接入方式和易混淆边界。", "07-subpackages.md"],
  ["production-patterns", "08", "生产组合模式", "生产模式", "将 provider、权限、状态、事件和恢复组合成服务。", "08-production-patterns.md"],
  ["icoder-tutorial", "09", "iCoder 端到端教程", "iCoder 示例", "运行完整 Code Agent，观察 Skills、MCP、SQLite 与工具链。", "09-icoder-tutorial.md"],
  ["reference", "10", "速查与术语", "速查", "包选择、状态枚举、错误分类与验证命令。", "10-reference.md"],
] as const;

function source(file: string) {
  const key = Object.keys(sources).find((path) => path.endsWith(`/docs/${file}`));
  if (!key) throw new Error(`missing documentation source: ${file}`);
  return sources[key];
}

export const chapters: Chapter[] = chapterData.map(
  ([slug, index, title, shortTitle, description, file]) => ({
    slug,
    index,
    title,
    shortTitle,
    description,
    content: source(file),
  }),
);

export function chapterPath(chapter: Chapter) {
  return chapter.slug ? `/docs/${chapter.slug}` : "/";
}

const markdownRoutes = new Map<string, string>(
  chapterData.map(([slug, , , , , file]) => [file, slug ? `/docs/${slug}` : "/"]),
);

export function documentationLink(href: string) {
  const clean = href.replace(/^\.\//, "");
  const internal = markdownRoutes.get(clean);
  if (internal) return internal;
  if (href.startsWith("../")) {
    return `https://github.com/iceymoss/agent-runtime-go/blob/main/${href.replace(/^\.\.\//, "")}`;
  }
  return href;
}
