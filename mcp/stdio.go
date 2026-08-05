package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type ArgValidator func([]string) error

type StdioServer struct {
	Executable   string
	FixedArgs    []string
	BaseEnv      map[string]string
	AllowedEnv   []string
	WorkingDir   string
	ValidateArgs ArgValidator
}

type StdioRegistry struct {
	RootDir string
	Servers map[string]StdioServer
}

type stdioConnector struct {
	registry StdioRegistry
	secrets  SecretProvider
}

func NewStdioConnector(registry StdioRegistry, secrets SecretProvider) (Connector, error) {
	cloned, err := validateStdioRegistry(registry)
	if err != nil {
		return nil, err
	}
	return &stdioConnector{registry: cloned, secrets: secrets}, nil
}

func (c *stdioConnector) Connect(ctx context.Context, request ConnectRequest) (Client, error) {
	if request.Config.Transport.Kind != TransportStdio || request.Config.Transport.Stdio == nil {
		return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "transport is not stdio")
	}
	config := request.Config.Transport.Stdio
	entry, allowed := c.registry.Servers[config.ServerKey]
	if !allowed {
		return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "server key is not approved")
	}
	if entry.ValidateArgs != nil {
		if err := entry.ValidateArgs(append([]string(nil), config.Args...)); err != nil {
			return nil, mcpError(ErrTransportDenied, err, "connect stdio", request.Config.ID, "", "arguments rejected")
		}
	} else if len(config.Args) != 0 {
		return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "additional arguments are not allowed")
	}
	environment := make([]string, 0, len(entry.BaseEnv)+len(config.EnvRefs))
	for name, value := range entry.BaseEnv {
		environment = append(environment, name+"="+value)
	}
	allowedEnv := make(map[string]struct{}, len(entry.AllowedEnv))
	for _, name := range entry.AllowedEnv {
		allowedEnv[name] = struct{}{}
	}
	for _, ref := range config.EnvRefs {
		if _, allowed := allowedEnv[ref.Name]; !allowed || c.secrets == nil {
			return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "environment name is not approved")
		}
		if _, fixed := entry.BaseEnv[ref.Name]; fixed {
			return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "fixed environment cannot be overridden")
		}
		value, err := c.secrets.Resolve(ctx, request.Scope.TenantKey, ref.SecretRef)
		if err != nil {
			return nil, mcpError(ErrTransportDenied, err, "connect stdio", request.Config.ID, "", "secret resolution failed")
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, mcpError(ErrTransportDenied, nil, "connect stdio", request.Config.ID, "", "secret value is invalid")
		}
		environment = append(environment, ref.Name+"="+value)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(entry.Executable, append(append([]string(nil), entry.FixedArgs...), config.Args...)...)
	command.Dir = entry.WorkingDir
	command.Env = environment
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, mcpError(ErrReconnectFailed, err, "connect stdio", request.Config.ID, "", "create stdin failed")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, mcpError(ErrReconnectFailed, err, "connect stdio", request.Config.ID, "", "create stdout failed")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, mcpError(ErrReconnectFailed, err, "connect stdio", request.Config.ID, "", "process start failed")
	}
	transport := &stdioTransport{command: command, stdin: stdin, output: bufio.NewReader(stdout)}
	if setter, ok := stdout.(deadlineSetter); ok {
		transport.outputDeadline = setter
	}
	client, err := NewProtocolClient(transport)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	return client, nil
}

type stdioTransport struct {
	mu             sync.Mutex
	command        *exec.Cmd
	stdin          io.WriteCloser
	output         *bufio.Reader
	outputDeadline deadlineSetter
	nextID         uint64
	closed         bool
}

type deadlineSetter interface {
	SetDeadline(time.Time) error
}

func (t *stdioTransport) Send(ctx context.Context, request Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Response{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if writer, ok := t.stdin.(deadlineSetter); ok {
			if err := writer.SetDeadline(deadline); err != nil {
				return Response{}, mcpError(ErrUpstream, err, "stdio deadline", "", "", "set write deadline failed")
			}
		}
		if t.outputDeadline != nil {
			if err := t.outputDeadline.SetDeadline(deadline); err != nil {
				return Response{}, mcpError(ErrUpstream, err, "stdio deadline", "", "", "set read deadline failed")
			}
		}
	}
	t.nextID++
	payload, err := jsoncodec.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: t.nextID, Method: request.Method, Params: request.Params})
	if err != nil {
		return Response{}, err
	}
	payload = append(payload, '\n')
	if _, err := t.stdin.Write(payload); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, mcpError(ErrCallUnknown, err, "stdio send", "", "", "request write failed")
	}
	line, err := t.output.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, mcpError(ErrCallUnknown, err, "stdio receive", "", "", "response read failed")
	}
	var envelope struct {
		ID     uint64         `json:"id"`
		Result []byte         `json:"result"`
		Error  *UpstreamError `json:"error"`
	}
	if err := jsoncodec.Unmarshal(line, &envelope); err != nil || envelope.ID != t.nextID {
		return Response{}, mcpError(ErrUpstream, err, "stdio receive", "", "", "invalid response envelope")
	}
	return Response{Result: append([]byte(nil), envelope.Result...), Error: envelope.Error}, nil
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	var result error
	result = errors.Join(result, t.stdin.Close())
	if t.command.Process != nil {
		if err := t.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, err)
		}
	}
	if err := t.command.Wait(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func validateStdioRegistry(registry StdioRegistry) (StdioRegistry, error) {
	root, err := filepath.Abs(registry.RootDir)
	if err != nil || registry.RootDir == "" {
		return StdioRegistry{}, mcpError(ErrInvalidConfig, err, "stdio registry", "", "", "absolute root directory is required")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return StdioRegistry{}, mcpError(ErrInvalidConfig, err, "stdio registry", "", "", "root directory is unavailable")
	}
	cloned := StdioRegistry{RootDir: root, Servers: make(map[string]StdioServer, len(registry.Servers))}
	for key, entry := range registry.Servers {
		if !identifierPattern.MatchString(key) || !filepath.IsAbs(entry.Executable) {
			return StdioRegistry{}, mcpError(ErrInvalidConfig, nil, "stdio registry", "", "", "server key and executable must be explicit")
		}
		executable, err := filepath.EvalSymlinks(entry.Executable)
		if err != nil {
			return StdioRegistry{}, mcpError(ErrInvalidConfig, err, "stdio registry", "", "", "executable is unavailable")
		}
		info, err := os.Stat(executable)
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return StdioRegistry{}, mcpError(ErrInvalidConfig, err, "stdio registry", "", "", "executable is not executable")
		}
		workingDir := entry.WorkingDir
		if workingDir == "" {
			workingDir = root
		}
		workingDir, err = filepath.Abs(workingDir)
		if err != nil {
			return StdioRegistry{}, err
		}
		workingDir, err = filepath.EvalSymlinks(workingDir)
		if err != nil || !withinRoot(root, workingDir) {
			return StdioRegistry{}, mcpError(ErrTransportDenied, err, "stdio registry", "", "", "working directory escapes root")
		}
		entry.Executable = executable
		entry.WorkingDir = workingDir
		entry.FixedArgs = append([]string(nil), entry.FixedArgs...)
		entry.AllowedEnv = append([]string(nil), entry.AllowedEnv...)
		entry.BaseEnv = cloneStringMap(entry.BaseEnv)
		for name, value := range entry.BaseEnv {
			if !envNamePattern.MatchString(name) || strings.ContainsRune(value, '\x00') {
				return StdioRegistry{}, fmt.Errorf("%w: invalid base environment", ErrInvalidConfig)
			}
		}
		cloned.Servers[key] = entry
	}
	return cloned, nil
}

func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cloneStringMap(value map[string]string) map[string]string {
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

var _ Connector = (*stdioConnector)(nil)
var _ Transport = (*stdioTransport)(nil)
