package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// unexercisedBaseline is the debt this check found when it was introduced:
// declarations no code in this repository mentions, so nothing has ever
// exercised them and their doc comments are the only evidence they work.
//
// It is frozen, not accepted. The list may only shrink: give an entry a test,
// an example, or a demo that uses it, then delete the line. An entry that has
// become referenced also fails the test, so the list cannot quietly rot into a
// rubber stamp.
//
// Nothing new belongs here. A declaration added after this baseline must be
// exercised, or explained in unreferencedAllowlist.
var unexercisedBaseline = map[string]string{
	"ActorKey":                    "permission/contracts.go",
	"BlockerArtifact":             "durable/reconcile.go",
	"BlockerChild":                "durable/reconcile.go",
	"BlockerOperator":             "durable/reconcile.go",
	"CloneParts":                  "message/apply.go",
	"CodeNotReady":                "coordinator/contracts.go",
	"CodePivotConflict":           "context/errors.go",
	"CodeRevisionNotFound":        "context/errors.go",
	"ContentAudio":                "mcp/contracts.go",
	"ContentImage":                "mcp/contracts.go",
	"ContentResource":             "mcp/contracts.go",
	"ContentResourceLink":         "mcp/contracts.go",
	"ContentStructured":           "mcp/contracts.go",
	"DiagnosticConservativeCount": "context/contracts.go",
	"DigestConfig":                "durable/codec.go",
	"DigestInput":                 "durable/codec.go",
	"ErrBusClosed":                "event/errors.go",
	"ErrConsumptionConflict":      "event/errors.go",
	"ErrPivotConflict":            "context/errors.go",
	"ErrPrecedenceConflict":       "skills/errors.go",
	"ErrRecipeNotFound":           "coordinator/contracts.go",
	"ErrResumeUnsupported":        "session/host_contracts.go",
	"ErrServerDisabled":           "mcp/errors.go",
	"ErrSessionFinished":          "session/contracts.go",
	"ErrUnsupportedSchema":        "event/errors.go",
	"FactChildReconcileFailed":    "subagent/contracts.go",
	"FactType":                    "message/contracts.go",
	"FactVersion":                 "message/contracts.go",
	"MutationFact":                "message/contracts.go",
	"NewRoutingConnector":         "mcp/connector.go",
	"NewStdioConnector":           "mcp/stdio.go",
	"ParentKey":                   "permission/contracts.go",
	"PayloadType":                 "message/contracts.go",
	"PayloadVersion":              "message/contracts.go",
	"RedactedInput":               "permission/contracts.go",
	"RuntimeSnapshot":             "coordinator/contracts.go",
	"StatusFinished":              "session/contracts.go",
	"SupersedesRequestKey":        "permission/contracts.go",
	"TransportFactory":            "mcp/contracts.go",
	"WireManifest":                "coordinator/contracts.go",
	"WithMaxPayloadBytes":         "event/memory.go",
}

// unreferencedAllowlist names declarations that exist for callers of this
// library and are deliberately never referenced here.
//
// Every entry is a decision, not an exemption: adding one means saying out loud
// why shipping something untested is acceptable in that particular case.
var unreferencedAllowlist = map[string]string{}

// TestNoUnreferencedDeclarations fails when the library declares something no
// code in this repository ever mentions.
//
// It exists because the same defect kept appearing in different disguises:
// capability flags nothing read, a persistence port nothing called, a stream
// consumer nothing invoked, a conformance suite nothing ran. Those were not four
// incidents but one - nothing proved that a declaration was reachable from a
// real path - and a rule that a human has to remember is a rule that erodes.
// Tests and examples count as references: exercising something in a test is
// exactly the evidence that was missing.
func TestNoUnreferencedDeclarations(t *testing.T) {
	root := repositoryRoot(t)
	files := goFiles(t, root)

	declared := make(map[string]token.Pos)
	declaredIn := make(map[string]string)
	used := make(map[string][]token.Pos)
	fset := token.NewFileSet()

	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		// Only the library declares; the demo module and the examples are pure
		// consumers, and holding them to this rule would say nothing about the
		// public surface.
		if isLibraryFile(relative) {
			for name, pos := range declarations(file) {
				declared[name] = pos
				declaredIn[name] = relative
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok {
				used[ident.Name] = append(used[ident.Name], ident.Pos())
			}
			return true
		})
	}

	var unreferenced []string
	for name, declaration := range declared {
		if _, allowed := unreferencedAllowlist[name]; allowed {
			continue
		}
		referenced := false
		for _, pos := range used[name] {
			if pos != declaration {
				referenced = true
				break
			}
		}
		if _, known := unexercisedBaseline[name]; known {
			continue
		}
		if !referenced {
			unreferenced = append(unreferenced, name+" ("+declaredIn[name]+")")
		}
	}
	sort.Strings(unreferenced)
	if len(unreferenced) > 0 {
		t.Fatalf("declared but referenced nowhere in this repository:\n  %s\n\n"+
			"Each one is either dead and should be deleted, or real and needs a test,\n"+
			"an example, or a demo that exercises it. If it exists only for callers of\n"+
			"the library, add it to unreferencedAllowlist with the reason.",
			strings.Join(unreferenced, "\n  "))
	}
	assertBaselineIsCurrent(t, declared, used)
}

// assertBaselineIsCurrent keeps the debt list from rotting.
//
// A baseline entry that has since gained a test, or that names a declaration
// that no longer exists, must be deleted - otherwise the list stops describing
// reality and starts excusing whatever happens to share a name with it.
func assertBaselineIsCurrent(t *testing.T, declared map[string]token.Pos, used map[string][]token.Pos) {
	t.Helper()
	var stale []string
	for name := range unexercisedBaseline {
		declaration, exists := declared[name]
		if !exists {
			stale = append(stale, name+" (no longer declared)")
			continue
		}
		for _, pos := range used[name] {
			if pos != declaration {
				stale = append(stale, name+" (now exercised)")
				break
			}
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("unexercisedBaseline is out of date; delete these lines:\n  %s",
			strings.Join(stale, "\n  "))
	}
}

// declarations collects the package-level names one file introduces, plus the
// fields of the exported structs it declares.
//
// Fields are included because that is where the capability flags hid: a struct
// can be used constantly while one of its fields is never read by anything.
func declarations(file *ast.File) map[string]token.Pos {
	found := make(map[string]token.Pos)
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			// Methods are reached through interfaces and embedding, which this
			// identifier-level check cannot see; only plain functions are counted.
			if typed.Recv == nil && typed.Name.Name != "init" {
				found[typed.Name.Name] = typed.Name.Pos()
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch typedSpec := spec.(type) {
				case *ast.TypeSpec:
					found[typedSpec.Name.Name] = typedSpec.Name.Pos()
					collectFields(typedSpec, found)
				case *ast.ValueSpec:
					for _, name := range typedSpec.Names {
						if name.Name != "_" {
							found[name.Name] = name.Pos()
						}
					}
				}
			}
		}
	}
	return found
}

func collectFields(spec *ast.TypeSpec, found map[string]token.Pos) {
	structType, ok := spec.Type.(*ast.StructType)
	if !ok || !ast.IsExported(spec.Name.Name) || structType.Fields == nil {
		return
	}
	for _, field := range structType.Fields.List {
		for _, name := range field.Names {
			if ast.IsExported(name.Name) {
				found[name.Name] = name.Pos()
			}
		}
	}
}

// isLibraryFile reports whether a path holds library code, as opposed to a
// consumer of it.
func isLibraryFile(relative string) bool {
	switch {
	case strings.HasPrefix(relative, "demo"+string(filepath.Separator)),
		strings.HasPrefix(relative, "examples"+string(filepath.Separator)),
		strings.HasPrefix(relative, "cmd"+string(filepath.Separator)):
		return false
	}
	return !strings.HasSuffix(relative, "_test.go")
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "dist", "docs-site":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return working
}
