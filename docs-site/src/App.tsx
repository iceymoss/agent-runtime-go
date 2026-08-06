import { isValidElement, useEffect, useState, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import rehypeSlug from "rehype-slug";
import remarkGfm from "remark-gfm";
import { docPath, docs, documentationLink, sections, type Doc } from "./docs";

function currentDoc() {
  const slug = window.location.pathname.replace(/^\/docs\/?/, "").replace(/\/$/, "");
  return docs.find((doc) => doc.slug === slug) ?? docs[0];
}

function navigate(path: string) {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
  window.scrollTo({ top: 0, behavior: "auto" });
}

function MermaidDiagram({ chart }: { chart: string }) {
  const [svg, setSvg] = useState("");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    const id = `diagram-${crypto.randomUUID().replaceAll("-", "")}`;
    import("mermaid")
      .then(({ default: mermaid }) => {
        mermaid.initialize({
          startOnLoad: false,
          theme: "base",
          securityLevel: "strict",
          themeVariables: {
            background: "#f4f7f5",
            primaryColor: "#dceee8",
            primaryTextColor: "#101820",
            primaryBorderColor: "#167d68",
            lineColor: "#52645f",
            secondaryColor: "#e8edf4",
            tertiaryColor: "#fff6db",
            fontFamily: "ui-monospace, monospace",
          },
        });
        return mermaid.render(id, chart);
      })
      .then(({ svg: rendered }) => active && setSvg(rendered))
      .catch(() => active && setFailed(true));
    return () => {
      active = false;
    };
  }, [chart]);

  if (failed) return <pre className="diagram-source"><code>{chart}</code></pre>;
  return <div className="diagram" aria-label="架构图" dangerouslySetInnerHTML={{ __html: svg }} />;
}

function DocLink({ href = "", children }: React.ComponentProps<"a">) {
  const target = documentationLink(href);
  const internal = target === "/" || target.startsWith("/docs/");
  return (
    <a
      href={target}
      onClick={(event) => {
        if (!internal || event.ctrlKey || event.metaKey || event.shiftKey) return;
        event.preventDefault();
        navigate(target);
      }}
    >
      {children}
    </a>
  );
}

function MarkdownPage({ doc }: { doc: Doc }) {
  return (
    <article className="document">
      <div className="chapter-stamp" aria-hidden="true">{doc.index}</div>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeSlug]}
        components={{
          a: DocLink,
          pre({ children }) {
            if (isValidElement<{ className?: string; children?: ReactNode }>(children) && children.props.className === "language-mermaid") {
              return <MermaidDiagram chart={String(children.props.children).replace(/\n$/, "")} />;
            }
            return <pre>{children}</pre>;
          },
        }}
      >
        {doc.content}
      </ReactMarkdown>
    </article>
  );
}

export default function App() {
  const [doc, setDoc] = useState(currentDoc);
  const [menuOpen, setMenuOpen] = useState(false);

  useEffect(() => {
    const onLocation = () => {
      setDoc(currentDoc());
      setMenuOpen(false);
    };
    window.addEventListener("popstate", onLocation);
    return () => window.removeEventListener("popstate", onLocation);
  }, []);

  const docIndex = docs.indexOf(doc);
  const previous = docs[docIndex - 1];
  const next = docs[docIndex + 1];

  return (
    <div className="shell">
      <header className="topbar">
        <a className="brand" href="/" onClick={(event) => { event.preventDefault(); navigate("/"); }}>
          <span className="brand-mark">AR</span>
          <span>agent-runtime-go</span>
        </a>
        <div className="runtime-status"><i /> docs generation <strong>v2</strong></div>
        <a className="source-link" href="https://github.com/iceymoss/agent-runtime-go">GitHub ↗</a>
        <button className="menu-button" onClick={() => setMenuOpen((open) => !open)} aria-expanded={menuOpen}>目录</button>
      </header>

      <aside className={`sidebar ${menuOpen ? "open" : ""}`}>
        {sections.map((section) => (
          <div key={section.label} className="sidebar-section">
            <div className="sidebar-label">{section.label}</div>
            <nav aria-label={section.label}>
              {section.docs.map((item) => (
                <a
                  key={item.index}
                  href={docPath(item)}
                  className={item === doc ? "active" : ""}
                  onClick={(event) => { event.preventDefault(); navigate(docPath(item)); }}
                >
                  <span>{item.index}</span>
                  <div><strong>{item.shortTitle}</strong><small>{item.description}</small></div>
                </a>
              ))}
            </nav>
          </div>
        ))}
      </aside>

      <main>
        <div className="context-line">
          <span>package agent</span><b>/</b><span>{doc.slug || "index"}</span>
        </div>
        <MarkdownPage doc={doc} />
        <nav className="page-nav" aria-label="上一篇和下一篇">
          {previous ? <DocLink href={previous.file}>← {previous.shortTitle}</DocLink> : <span />}
          {next ? <DocLink href={next.file}>{next.shortTitle} →</DocLink> : <span />}
        </nav>
      </main>

      <aside className="rail">
        <div className="rail-block"><span>当前位置</span><strong>{docIndex + 1} / {docs.length}</strong></div>
        <div className="rail-track"><i style={{ height: `${Math.max(4, (docIndex / (docs.length - 1)) * 100)}%` }} /></div>
        <div className="rail-block"><span>阅读目标</span><p>{doc.description}</p></div>
      </aside>
    </div>
  );
}
