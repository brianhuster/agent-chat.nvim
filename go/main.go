package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/neovim/go-client/nvim"
)

// AcpSession represents a single ACP session tied to a buffer
type AcpSession struct {
	bufnr       int
	conn        *acp.ClientSideConnection
	sessionID   acp.SessionId
	ctx         context.Context
	cancel      context.CancelFunc
	cmd         *exec.Cmd
	autoApprove bool
}

// SessionManager manages multiple ACP sessions
type SessionManager struct {
	mu       sync.Mutex
	sessions map[int]*AcpSession
}

type acpClientImpl struct {
	session *AcpSession
}

var vim Vim

// RequestPermission handles permission requests from ACP
func (c *acpClientImpl) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	// If auto-approve is enabled, automatically select first allow option
	if c.session.autoApprove {
		for _, o := range params.Options {
			if o.Kind == acp.PermissionOptionKindAllowOnce || o.Kind == acp.PermissionOptionKindAllowAlways {
				return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: o.OptionId}}}, nil
			}
		}
		if len(params.Options) > 0 {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: params.Options[0].OptionId}}}, nil
		}
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}

	// Build interactive menu
	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}

	opts := []string{}
	for _, o := range params.Options {
		opts = append(opts, o.Name)
	}

	choice, err := vim.uiSelect(opts, selectOpts{Title: fmt.Sprintf("Permission request: %s", title)})

	if err != nil {
		fmt.Printf("Error displaying permission prompt: %v\n", err)
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}

	// choice is 1-indexed, 0 means cancelled or invalid
	if choice < 1 || choice > len(params.Options) {
		c.session.appendToBuffer("\n[Permission denied]\n")
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}

	// Get the selected option
	selectedOption := params.Options[choice-1]
	c.session.appendToBuffer(fmt.Sprintf("\n[Permission granted: %s]\n", selectedOption.Name))

	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: selectedOption.OptionId}}}, nil
}

// SessionUpdate handles streaming updates from ACP
func (c *acpClientImpl) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	u := params.Update
	switch {
	case u.AgentMessageChunk != nil:
		content := u.AgentMessageChunk.Content
		if content.Text != nil {
			c.session.appendToBuffer(content.Text.Text)
		}
	case u.ToolCall != nil:
		c.session.appendToBuffer(fmt.Sprintf("\n🔧 %s (%s)\n", u.ToolCall.Title, u.ToolCall.Status))

		// Display tool call content if available
		for _, tc := range u.ToolCall.Content {
			if tc.Content != nil && tc.Content.Content.Text != nil {
				c.session.appendToBuffer(tc.Content.Content.Text.Text)
			}
			if tc.Diff != nil {
				// Use vim.diff to generate a proper unified diff
				c.session.showDiff(tc.Diff.Path, tc.Diff.OldText, tc.Diff.NewText)
			}
		}
	case u.ToolCallUpdate != nil:
		// Only show status updates if there's meaningful content or a title change
		hasContent := len(u.ToolCallUpdate.Content) > 0
		hasTitle := u.ToolCallUpdate.Title != nil

		if hasTitle && u.ToolCallUpdate.Status != nil {
			c.session.appendToBuffer(fmt.Sprintf("\n🔧 %s (%s)\n", *u.ToolCallUpdate.Title, *u.ToolCallUpdate.Status))
		} else if hasTitle {
			c.session.appendToBuffer(fmt.Sprintf("\n🔧 %s\n", *u.ToolCallUpdate.Title))
		} else if u.ToolCallUpdate.Status != nil && hasContent {
			// Only show status if there's content to display
			c.session.appendToBuffer(fmt.Sprintf("\n🔧 %s\n", *u.ToolCallUpdate.Status))
		}

		// Display content updates if available
		for _, tc := range u.ToolCallUpdate.Content {
			if tc.Content != nil && tc.Content.Content.Text != nil {
				c.session.appendToBuffer(tc.Content.Content.Text.Text)
			}
			if tc.Diff != nil {
				// Use vim.diff to generate a proper unified diff
				c.session.showDiff(tc.Diff.Path, tc.Diff.OldText, tc.Diff.NewText)
			}
		}
	case u.Plan != nil:
		c.session.appendToBuffer("[Plan update]\n")
	case u.AgentThoughtChunk != nil:
		thought := u.AgentThoughtChunk.Content
		if thought.Text != nil {
			c.session.appendToBuffer(fmt.Sprintf("[Thought] %s\n", thought.Text.Text))
		}
	case u.AvailableCommandsUpdate != nil:
		// TODO
	case u.UserMessageChunk != nil:
		// Silent for user messages
	case u.CurrentModeUpdate != nil:
	}
	return nil
}

// WriteTextFile implements file writing capability
func (c *acpClientImpl) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	buf, err := vim.bufnr(params.Path, false)
	if err == nil && buf != -1 {
		content := []byte(params.Content)
		lines := bytes.Split(content, []byte("\n"))
		if err := vim.api.SetBufferLines(buf, 0, -1, false, lines); err != nil {
			return acp.WriteTextFileResponse{}, fmt.Errorf("set buffer lines for %s: %w", params.Path, err)
		}
		c.session.appendToBuffer(fmt.Sprintf("[Wrote %d bytes to buffer %s]\n", len(params.Content), params.Path))
		return acp.WriteTextFileResponse{}, nil
	} else {
		dir := filepath.Dir(params.Path)
		if dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return acp.WriteTextFileResponse{}, fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(params.Path, []byte(params.Content), 0o644); err != nil {
			return acp.WriteTextFileResponse{}, fmt.Errorf("write %s: %w", params.Path, err)
		}
		c.session.appendToBuffer(fmt.Sprintf("[Wrote %d bytes to %s]\n", len(params.Content), params.Path))
		return acp.WriteTextFileResponse{}, nil
	}
}

// ReadTextFile implements file reading capability
func (c *acpClientImpl) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if buf, err := vim.bufnr(params.Path, false); err == nil && buf != -1 {
		var start, end int
		if params.Line != nil && *params.Line > 0 {
			start = *params.Line - 1
		} else {
			start = 0
		}
		if params.Limit != nil && *params.Limit > 0 {
			end = start + *params.Limit
		} else {
			end = -1
		}
		lines, err := vim.api.BufferLines(buf, start, end, false)
		if err != nil {
			return acp.ReadTextFileResponse{}, fmt.Errorf("get buffer lines for %s: %w", params.Path, err)
		}
		content := string(bytes.Join(lines, []byte("\n")))
		c.session.appendToBuffer(fmt.Sprintf("[Read %s (%d bytes) from buffer]\n", params.Path, len(content)))
		return acp.ReadTextFileResponse{Content: content}, nil
	} else {
		b, err := os.ReadFile(params.Path)
		if err != nil {
			return acp.ReadTextFileResponse{}, fmt.Errorf("read %s: %w", params.Path, err)
		}
		content := string(b)
		if params.Line != nil || params.Limit != nil {
			lines := strings.Split(content, "\n")
			start := 0
			if params.Line != nil && *params.Line > 0 {
				start = min(max(*params.Line-1, 0), len(lines))
			}
			end := len(lines)
			if params.Limit != nil && *params.Limit > 0 {
				if start+*params.Limit < end {
					end = start + *params.Limit
				}
			}
			content = strings.Join(lines[start:end], "\n")
		}
		c.session.appendToBuffer(fmt.Sprintf("[Read %s (%d bytes)]\n", params.Path, len(content)))
		return acp.ReadTextFileResponse{Content: content}, nil
	}
}

// Terminal methods (no-op implementations)
func (c *acpClientImpl) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "term-1"}, nil
}

func (c *acpClientImpl) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "Sorry, terminal support is not available yet", Truncated: false}, nil
}

func (c *acpClientImpl) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *acpClientImpl) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (c *acpClientImpl) KillTerminalCommand(ctx context.Context, params acp.KillTerminalCommandRequest) (acp.KillTerminalCommandResponse, error) {
	return acp.KillTerminalCommandResponse{}, nil
}

// SessionManager methods exposed to Lua

type AcpNewSessionOpts struct {
	Env map[string]string         `json:"env" msgpack:"env"`
	Mcp map[string]map[string]any `json:"mcp" msgpack:"mcp"`
}

func ConvertMcpConfigToMcpServer(name string, config map[string]any) (*acp.McpServer, error) {
	// Detect transport type
	t, _ := config["type"].(string)

	switch t {
	case "http", "sse":
		// Map headers - initialize to empty slice to avoid nil
		headers := make([]acp.HttpHeader, 0)
		if rawHeaders, ok := config["headers"].(map[string]any); ok {
			for k, v := range rawHeaders {
				strVal, _ := v.(string)
				headers = append(headers, acp.HttpHeader{Name: k, Value: strVal})
			}
		}

		serverName := name
		if n, ok := config["name"].(string); ok {
			serverName = n
		}

		if t == "http" {
			return &acp.McpServer{
				Http: &acp.McpServerHttp{
					Name:    serverName,
					Type:    "http",
					Url:     config["url"].(string),
					Headers: headers,
				},
			}, nil
		} else { // sse
			return &acp.McpServer{
				Sse: &acp.McpServerSse{
					Name:    serverName,
					Type:    "sse",
					Url:     config["url"].(string),
					Headers: headers,
				},
			}, nil
		}

	default:
		// Default to stdio
		// Initialize to empty slice to avoid nil
		args := make([]string, 0)
		if cmdSlice, ok := config["cmd"].([]any); ok && len(cmdSlice) > 1 {
			for _, a := range cmdSlice[1:] {
				if str, ok := a.(string); ok {
					args = append(args, str)
				}
			}
		}

		var command string
		if cmdSlice, ok := config["cmd"].([]any); ok && len(cmdSlice) > 0 {
			if str, ok := cmdSlice[0].(string); ok {
				command = str
			}
		}

		// Initialize to empty slice to avoid nil
		env := make([]acp.EnvVariable, 0)
		if rawEnv, ok := config["env"].(map[string]any); ok {
			for k, v := range rawEnv {
				if strVal, ok := v.(string); ok {
					env = append(env, acp.EnvVariable{Name: k, Value: strVal})
				}
			}
		}

		serverName := name
		if n, ok := config["name"].(string); ok {
			serverName = n
		}

		return &acp.McpServer{
			Stdio: &acp.McpServerStdio{
				Name:    serverName,
				Command: command,
				Args:    args,
				Env:     env,
			},
		}, nil
	}
}

// AcpNewSession initializes an ACP connection for a buffer
func (m *SessionManager) AcpNewSession(bufnr int, agent_cmd []string, opts AcpNewSessionOpts) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[bufnr]; exists {
		return nil, fmt.Errorf("ACP session already exists for buffer %d", bufnr)
	}

	session := &AcpSession{
		bufnr:       bufnr,
		autoApprove: false,
	}

	session.ctx, session.cancel = context.WithCancel(context.Background())

	// Start the agent process
	cmd := exec.CommandContext(session.ctx, agent_cmd[0], agent_cmd[1:]...)
	cmd.Stderr = os.Stderr

	// Set environment variables from opts.env if provided
	if opts.Env != nil {
		cmd.Env = os.Environ()
		for key, value := range opts.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe error: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", agent_cmd[0], err)
	}
	session.cmd = cmd

	client := &acpClientImpl{session: session}
	session.conn = acp.NewClientSideConnection(client, stdin, stdout)

	// Initialize
	initRes, err := session.conn.Initialize(session.ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapability{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	})
	if err != nil {
		session.cleanup()
		if re, ok := err.(*acp.RequestError); ok {
			if b, mErr := json.MarshalIndent(re, "", "  "); mErr == nil {
				return nil, fmt.Errorf("initialize error: %s", string(b))
			}
			return nil, fmt.Errorf("initialize error (%d): %s", re.Code, re.Message)
		}
		return nil, fmt.Errorf("initialize error: %w", err)
	}

	// Create new session
	cwd, err := os.Getwd()
	if err != nil {
		session.cleanup()
		return nil, fmt.Errorf("getwd error: %w", err)
	}

	var mcpServers []acp.McpServer
	for name, config := range opts.Mcp {
		srv, err := ConvertMcpConfigToMcpServer(name, config)
		if err != nil {
			session.cleanup()
			return nil, fmt.Errorf("invalid MCP server config for %s: %w", name, err)
		}
		mcpServers = append(mcpServers, *srv)
	}

	supportHttpMcp := initRes.AgentCapabilities.McpCapabilities.Http
	supportSseMcp := initRes.AgentCapabilities.McpCapabilities.Sse

	// if not support http or sse, filter them out
	filteredMcpServers := make([]acp.McpServer, 0)
	for _, srv := range mcpServers {
		if srv.Http != nil && !supportHttpMcp {
			continue
		}
		if srv.Sse != nil && !supportSseMcp {
			continue
		}
		filteredMcpServers = append(filteredMcpServers, srv)
	}
	mcpServers = filteredMcpServers

	newSess, err := session.conn.NewSession(session.ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: mcpServers,
	})
	if err != nil {
		session.cleanup()
		if re, ok := err.(*acp.RequestError); ok {
			if b, mErr := json.MarshalIndent(re, "", "  "); mErr == nil {
				return nil, fmt.Errorf("newSession error: %s", string(b))
			}
			return nil, fmt.Errorf("newSession error (%d): %s", re.Code, re.Message)
		}
		return nil, fmt.Errorf("newSession error: %w", err)
	}
	session.sessionID = newSess.SessionId

	modes := acp.SessionModeState{}
	if newSess.Modes != nil {
		modes = *newSess.Modes
	}
	vim.api.ExecLua(`require('acp').set_and_show_prompt_buf(...)`, nil, bufnr, map[string]any{"modes": modes, "session_id": session.sessionID})

	m.sessions[bufnr] = session
	return nil, nil
}

func (m *SessionManager) AcpSendPrompt(bufnr int, prompt string) (any, error) {
	if prompt == "" {
		return nil, fmt.Errorf("no prompt provided")
	}

	m.mu.Lock()
	session, exists := m.sessions[bufnr]
	m.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no ACP session for buffer %d", bufnr)
	}

	_, err := session.conn.Prompt(session.ctx, acp.PromptRequest{
		SessionId: session.sessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
	})
	if err != nil {
		if re, ok := err.(*acp.RequestError); ok {
			if b, mErr := json.MarshalIndent(re, "", "  "); mErr == nil {
				session.appendToBuffer(fmt.Sprintf("Error: %s\n", string(b)))
			} else {
				session.appendToBuffer(fmt.Sprintf("Error (%d): %s\n", re.Code, re.Message))
			}
			return nil, err
		}
		session.appendToBuffer(fmt.Sprintf("Error: %v\n", err))
		return nil, err
	}

	return nil, nil
}

// AcpCancel cancels the current prompt for a buffer
func (m *SessionManager) AcpCancel(bufnr int) (any, error) {
	m.mu.Lock()
	session, exists := m.sessions[bufnr]
	m.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no ACP session for buffer %d", bufnr)
	}

	err := session.conn.Cancel(session.ctx, acp.CancelNotification{SessionId: session.sessionID})
	if err != nil {
		fmt.Printf("Cancel error: %v", err)
		return nil, err
	}
	session.appendToBuffer("Cancelled.\n")
	return nil, nil
}

// AcpSetMode sets the mode for an ACP session
func (m *SessionManager) AcpSetMode(bufnr int, modeId string) (any, error) {
	m.mu.Lock()
	session, exists := m.sessions[bufnr]
	m.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("no ACP session for buffer %d", bufnr)
	}

	// Call setSessionMode on the agent
	_, err := session.conn.SetSessionMode(session.ctx, acp.SetSessionModeRequest{
		SessionId: session.sessionID,
		ModeId:    acp.SessionModeId(modeId),
	})
	if err != nil {
		fmt.Printf("Set mode error: %v\n", err)
		return nil, err
	}

	return modeId, nil
}

func (s *AcpSession) cleanup() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.conn = nil
	s.sessionID = ""
	s.ctx = nil
	s.cancel = nil
	s.cmd = nil
}

func (s *AcpSession) appendToBuffer(text string) {
	err := vim.api.ExecLua(`return require('acp').append_text(...)`, nil, s.bufnr, text)
	if err != nil {
		log.Printf("Error appending to buffer: %v\n", err)
	}
}

func (s *AcpSession) showDiff(path string, oldText *string, newText string) {
	var old string
	if oldText != nil {
		old = *oldText
	}

	var diff string
	err := vim.api.ExecLua(`return vim.text.diff(...)`, &diff, old, newText)

	if err != nil {
		log.Printf("Error generating diff: %v\n", err)
		return
	}

	if diff != "" {
		s.appendToBuffer("\n```diff\n")
		s.appendToBuffer(fmt.Sprintf("--- %s\n+++ %s\n", path, path))
		s.appendToBuffer(diff)
		s.appendToBuffer("\n```\n")
	}
}

func main() {
	// Turn off timestamps in output.
	log.SetFlags(0)

	// Direct writes by the application to stdout garble the RPC stream.
	// Redirect the application's direct use of stdout to stderr.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	var err error

	// Create a client connected to stdio. Configure the client to use the
	// standard log package for logging.
	api, err := nvim.New(os.Stdin, stdout, stdout, log.Printf)
	if err != nil {
		log.Fatal(err)
	}
	vim = Vim{api: api}

	// Create session manager
	manager := &SessionManager{
		sessions: make(map[int]*AcpSession),
	}

	// Register RPC handlers
	vim.api.RegisterHandler("AcpNewSession", manager.AcpNewSession)
	vim.api.RegisterHandler("AcpSendPrompt", manager.AcpSendPrompt)
	vim.api.RegisterHandler("AcpCancel", manager.AcpCancel)
	vim.api.RegisterHandler("AcpSetMode", manager.AcpSetMode)

	// Serve RPC requests
	if err := vim.api.Serve(); err != nil {
		log.Fatal(err)
	}
}
