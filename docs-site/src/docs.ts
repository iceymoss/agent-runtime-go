export type Doc = {
  slug: string;
  index: string;
  title: string;
  shortTitle: string;
  description: string;
  file: string;
  content: string;
};

export type Section = {
  label: string;
  docs: Doc[];
};

const sources = import.meta.glob("../../docs/**/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

type Entry = [slug: string, shortTitle: string, description: string, file: string];

const sectionData: { label: string; entries: Entry[] }[] = [
  {
    label: "入门",
    entries: [
      ["", "文档首页", "按需求找到对应文档，从这里开始。", "README.md"],
      ["quickstart", "快速开始", "10 分钟跑通一个会调工具的真实模型 Agent。", "quickstart.md"],
      ["concepts", "核心概念", "Message、Model、Tool、停止语义与错误分类。", "concepts.md"],
    ],
  },
  {
    label: "核心包",
    entries: [
      ["packages/agent", "agent 根包", "model/tool 循环执行器，一切的起点。", "packages/agent.md"],
      ["packages/openaicompat", "openaicompat", "OpenAI / DeepSeek / Qwen / Ollama 官方适配器。", "packages/openaicompat.md"],
    ],
  },
  {
    label: "模型与提示",
    entries: [
      ["packages/provider", "provider", "多模型目录、选择与工厂。", "packages/provider.md"],
      ["packages/prompt", "prompt", "Prompt 版本化与组合。", "packages/prompt.md"],
      ["packages/context", "context", "历史归一化、token 预算与上下文投影。", "packages/context.md"],
    ],
  },
  {
    label: "会话与持久化",
    entries: [
      ["packages/session", "session", "会话聚合、分支、认领与续跑。", "packages/session.md"],
      ["packages/message", "message", "带 revision CAS 的消息存储契约。", "packages/message.md"],
      ["packages/durable", "durable", "checkpoint、租约、fence：崩溃恢复与接管。", "packages/durable.md"],
    ],
  },
  {
    label: "工具与安全",
    entries: [
      ["packages/tool", "tool", "高级工具生命周期与副作用账本。", "packages/tool.md"],
      ["packages/permission", "permission", "allow / deny / ask 权限决策。", "packages/permission.md"],
      ["packages/skills", "skills", "不受信任的指令目录：加载与快照。", "packages/skills.md"],
      ["packages/mcp", "mcp", "MCP server 发现与工具调用。", "packages/mcp.md"],
    ],
  },
  {
    label: "编排与运维",
    entries: [
      ["packages/subagent", "subagent", "独立预算的子 Agent 运行。", "packages/subagent.md"],
      ["packages/coordinator", "coordinator", "不可变运行定义的解析与精确重建。", "packages/coordinator.md"],
      ["packages/event", "event", "可靠事件、outbox 与重放。", "packages/event.md"],
      ["packages/app", "app", "就绪探针与有界停机。", "packages/app.md"],
      ["packages/agenttest", "agenttest", "适配器一致性测试（test-only）。", "packages/agenttest.md"],
    ],
  },
  {
    label: "进阶",
    entries: [
      ["internals", "运行内部机制", "stream 协议、工具执行、停止与恢复状态机。", "internals.md"],
      ["production", "生产组合", "provider、权限、状态、事件的生产组合模式。", "production.md"],
      ["icoder", "iCoder 教程", "完整 Code Agent 如何组合所有子包。", "icoder.md"],
      ["reference", "速查表", "枚举、错误分类与验证命令。", "reference.md"],
    ],
  },
];

function source(file: string) {
  const key = Object.keys(sources).find((path) => path.endsWith(`/docs/${file}`));
  if (!key) throw new Error(`missing documentation source: ${file}`);
  return sources[key];
}

let position = 0;
export const sections: Section[] = sectionData.map(({ label, entries }) => ({
  label,
  docs: entries.map(([slug, shortTitle, description, file]) => {
    const doc: Doc = {
      slug,
      index: String(position).padStart(2, "0"),
      title: shortTitle,
      shortTitle,
      description,
      file,
      content: source(file),
    };
    position += 1;
    return doc;
  }),
}));

export const docs: Doc[] = sections.flatMap((section) => section.docs);

export function docPath(doc: Doc) {
  return doc.slug ? `/docs/${doc.slug}` : "/";
}

// Every markdown file has a unique basename, so cross-directory relative
// links ("session.md", "../internals.md", "packages/agent.md") all resolve
// through the basename.
const routesByBasename = new Map<string, string>(
  docs.map((doc) => [doc.file.split("/").pop() ?? doc.file, docPath(doc)]),
);

export function documentationLink(href: string) {
  const [pathPart, hash = ""] = href.split("#");
  const clean = pathPart.replace(/^(\.\.\/|\.\/)+/, "");
  const basename = clean.split("/").pop() ?? clean;
  const internal = routesByBasename.get(basename);
  if (internal !== undefined && basename.endsWith(".md")) {
    return hash ? `${internal}#${hash}` : internal;
  }
  if (href.startsWith("../")) {
    return `https://github.com/iceymoss/agent-runtime-go/blob/main/${href.replace(/^(\.\.\/)+/, "")}`;
  }
  return href;
}
