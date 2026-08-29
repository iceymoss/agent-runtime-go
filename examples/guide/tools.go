package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
)

// Ops is the application's own domain. The agent reaches it only through tools,
// so every rule about what may happen lives here rather than in a prompt.
type Ops struct {
	logs      map[string][]string
	restarted []string
}

func NewOps() *Ops {
	return &Ops{logs: map[string][]string{
		"checkout": {
			"12:01:03 ERROR payment gateway timeout after 30s",
			"12:01:04 ERROR payment gateway timeout after 30s",
			"12:01:09 WARN  connection pool exhausted (50/50)",
		},
		"search": {"12:00:00 INFO  index rebuilt in 4.2s"},
	}}
}

// --- chapter 2: a read-only tool ---

type ReadLogsInput struct {
	Service string `json:"service" description:"服务名，如 checkout"`
	Level   string `json:"level,omitempty" description:"只返回该级别及以上：INFO / WARN / ERROR"`
}

// ReadLogsTool is the shape most tools have: a function plus an input struct,
// with the JSON Schema generated from the struct.
func ReadLogsTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("read_logs", "读取一个服务最近的日志。",
		func(_ context.Context, in ReadLogsInput) (agent.ToolResult, error) {
			lines, ok := ops.logs[in.Service]
			if !ok {
				// The model chose a service that does not exist. It can fix that
				// by choosing another, so this is a result, not a failure.
				return agent.ToolResult{
					IsError: true,
					Content: fmt.Sprintf("未知服务 %q，已知的有: %s", in.Service, strings.Join(ops.services(), ", ")),
				}, nil
			}
			if in.Level != "" {
				lines = filterByLevel(lines, in.Level)
			}
			return agent.ToolResult{Content: strings.Join(lines, "\n")}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent))
}

// --- chapter 2 and 7: a tool with a side effect ---

type RestartInput struct {
	Service string `json:"service" description:"要重启的服务名"`
	Reason  string `json:"reason" description:"重启原因，会记入审计"`
}

// RestartTool changes the world, which is why chapter 7 puts an approval in
// front of it and why it declares that it must never be replayed blindly.
func RestartTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("restart_service", "重启一个服务。这会中断正在处理的请求。",
		func(ctx context.Context, in RestartInput) (agent.ToolResult, error) {
			if _, ok := ops.logs[in.Service]; !ok {
				return agent.ToolResult{IsError: true, Content: "未知服务 " + in.Service}, nil
			}
			if err := ops.restart(ctx, in.Service); err != nil {
				// The model cannot fix an orchestrator that is down. Ending the
				// attempt with the cause intact is the only honest outcome.
				return agent.ToolResult{}, fmt.Errorf("restart %s: %w", in.Service, err)
			}
			return agent.ToolResult{Content: "已重启 " + in.Service + "（原因：" + in.Reason + "）"}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyNever))
}

func (o *Ops) services() []string {
	names := make([]string, 0, len(o.logs))
	for name := range o.logs {
		names = append(names, name)
	}
	return names
}

func (o *Ops) restart(ctx context.Context, service string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
	}
	o.restarted = append(o.restarted, service)
	return nil
}

var levelRank = map[string]int{"INFO": 0, "WARN": 1, "ERROR": 2}

func filterByLevel(lines []string, level string) []string {
	want, ok := levelRank[strings.ToUpper(level)]
	if !ok {
		return lines
	}
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		for name, rank := range levelRank {
			if strings.Contains(line, name) && rank >= want {
				kept = append(kept, line)
				break
			}
		}
	}
	return kept
}
