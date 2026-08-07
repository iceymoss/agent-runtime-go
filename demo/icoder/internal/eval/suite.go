package eval

import "path/filepath"

func CodingSuite(root string) []Task {
	fixture := func(name string) string { return filepath.Join(root, "testdata", name) }
	return []Task{
		{ID: "single-file-fix", Prompt: "修复 Add，使测试通过，并运行最小测试。", Fixture: fixture("single-file"), Assertions: []Assertion{{Kind: "file_contains", Path: "math.go", Contains: "return left + right"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "multi-file-change", Prompt: "实现 Greeter，删除 obsolete.txt，并运行测试。", Fixture: fixture("multi-file"), Assertions: []Assertion{{Kind: "file_contains", Path: "greeter.go", Contains: "Hello, "}, {Kind: "file_not_exists", Path: "obsolete.txt"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "preserve-existing", Prompt: "修复实现，但不要修改 protected.txt。", Fixture: fixture("preserve"), Assertions: []Assertion{{Kind: "file_contains", Path: "protected.txt", Contains: "DO NOT CHANGE"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "compile-fix", Prompt: "修复当前编译错误并运行测试。", Fixture: fixture("compile-fix"), Assertions: []Assertion{{Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "rename-symbol", Prompt: "将 OldName 重命名为 CurrentName，更新所有调用方并运行测试。", Fixture: fixture("rename"), Assertions: []Assertion{{Kind: "file_contains", Path: "name.go", Contains: "func CurrentName"}, {Kind: "file_not_contains", Path: "name.go", Contains: "OldName"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "add-regression-test", Prompt: "为 Normalize 的空白输入行为补回归测试并确保测试通过。", Fixture: fixture("add-test"), Assertions: []Assertion{{Kind: "file_contains", Path: "normalize_test.go", Contains: "TestNormalizeWhitespace"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "configuration-default", Prompt: "将默认 Timeout 修复为 30，并运行测试。", Fixture: fixture("config"), Assertions: []Assertion{{Kind: "file_contains", Path: "config.go", Contains: "Timeout: 30"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
		{ID: "code-review", Prompt: "审查实现，把发现写入 REVIEW.md，包含具体风险。不要修改实现。", Fixture: fixture("review"), Assertions: []Assertion{{Kind: "file_contains", Path: "REVIEW.md", Contains: "division by zero"}, {Kind: "file_contains", Path: "divide.go", Contains: "return left / right"}}},
		{ID: "project-understanding", Prompt: "解释 Store 到 Service 的调用关系，写入 ARCHITECTURE.md。", Fixture: fixture("architecture"), Assertions: []Assertion{{Kind: "file_contains", Path: "ARCHITECTURE.md", Contains: "Service"}, {Kind: "file_contains", Path: "ARCHITECTURE.md", Contains: "Store"}}},
		{ID: "scoped-instructions", Prompt: "遵循 AGENTS.md 修复 pkg.Value 并运行测试。", Fixture: fixture("instructions"), Assertions: []Assertion{{Kind: "file_contains", Path: "protected.txt", Contains: "DO NOT CHANGE"}, {Kind: "file_contains", Path: "pkg/value.go", Contains: "return 7"}, {Kind: "command", Program: "go", Args: []string{"test", "./..."}}}},
	}
}
