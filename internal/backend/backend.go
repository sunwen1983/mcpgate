// Package backend speaks JSON-RPC 2.0 to upstream MCP servers.
//
// Two transports are supported: stdio (a child process exchanging
// newline-delimited JSON-RPC messages) and http (Streamable HTTP POST).
// Backend implementations are safe for concurrent use; stdio requests are
// serialized with a mutex because a child process has a single stdin/stdout
// pair and cannot multiplex concurrent requests.
package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Error   *Error         `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// Backend executes JSON-RPC 2.0 messages against an upstream MCP server and
// returns the parsed response.
type Backend interface {
	// Call sends req and returns the backend's response. The returned
	// error is non-nil for transport failures, timeouts, and malformed
	// responses. A JSON-RPC error object inside an otherwise well-formed
	// response is returned as *Error from the response's Error field,
	// not as a Go error, so callers can forward it verbatim.
	Call(ctx context.Context, req *Request) (*Response, error)
	// Notify forwards a JSON-RPC notification (a message without an id)
	// to the backend. Notifications get no response by definition, so
	// nothing is returned on success. The error is non-nil for transport
	// failures and timeouts only.
	Notify(ctx context.Context, method string, params map[string]any) error
	// Name returns the backend's configured name.
	Name() string
	// Close releases resources (kills the child process for stdio).
	Close() error
}

// New builds the Backend for a backend config entry.
func New(cfg config.Backend) (Backend, error) {
	switch cfg.Transport {
	case "stdio":
		return newStdioBackend(cfg)
	case "http":
		return newHTTPBackend(cfg), nil
	default:
		return nil, fmt.Errorf("unknown transport %q", cfg.Transport)
	}
}

// timeout returns the configured timeout, defaulting to 10s.
func timeout(cfg config.Backend) time.Duration {
	if cfg.Timeout == 0 {
		return 10 * time.Second
	}
	return time.Duration(cfg.Timeout)
}

// ---------------------------------------------------------------------------
// stdio backend
// ---------------------------------------------------------------------------

// stdioBackend manages a child MCP server process.
type stdioBackend struct {
	name    string
	cfg     config.Backend
	timeout time.Duration

	mu     sync.Mutex // serializes requests; one stdin/stdout pair
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func newStdioBackend(cfg config.Backend) (*stdioBackend, error) {
	b := &stdioBackend{name: cfg.Name, cfg: cfg, timeout: timeout(cfg)}
	if err := b.start(); err != nil {
		return nil, err
	}
	return b, nil
}

// start spawns the child process and wires up its stdio pipes.
func (b *stdioBackend) start() error {
	cmd := exec.Command(b.cfg.Command[0], b.cfg.Command[1:]...)
	cmd.Env = os.Environ()
	for k, v := range b.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// The child's stderr goes to our stderr so backend crashes are visible
	// in the gateway log instead of silently vanishing.
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdio backend %s: stdin pipe: %w", b.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdio backend %s: stdout pipe: %w", b.name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("stdio backend %s: start %v: %w", b.name, b.cfg.Command, err)
	}
	b.cmd = cmd
	b.stdin = stdin
	b.stdout = bufio.NewReader(stdout)
	return nil
}

// ensureRunning restarts the child if it has exited.
func (b *stdioBackend) ensureRunning() error {
	if b.cmd != nil && b.cmd.ProcessState == nil {
		return nil
	}
	// Process exited (or never started): clean up and restart.
	_ = b.Close()
	return b.start()
}

func (b *stdioBackend) Name() string { return b.name }

// Notify writes one JSON-RPC notification line to the child process.
// Notifications carry no id, so the server sends no response and nothing
// is read back; writes are serialized with Call on the same mutex so a
// notification can never interleave with an in-flight request.
func (b *stdioBackend) Notify(ctx context.Context, method string, params map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	if err := b.ensureRunning(); err != nil {
		return err
	}
	line, err := json.Marshal(&Request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("stdio backend %s: marshal notification: %w", b.name, err)
	}
	// Guard the blocking write with the context, like Call does.
	done := make(chan struct{})
	defer close(done)
	go watchForCancel(ctx, b.cmd, done)

	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("stdio backend %s: write notification: %w", b.name, err)
	}
	return nil
}

// watchForCancel kills the child process if ctx is done while the
// operation is still in flight (done not closed), unblocking a stuck
// read or write. It is race-free: close(done) is deferred after cancel(),
// so in program order close(done) happens-before cancel(); a watcher that
// observes ctx.Done() closed therefore also observes done closed, and the
// inner select deterministically skips the kill. A completed operation
// can never kill a healthy child.
func watchForCancel(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
		}
	case <-done:
	}
}

// Call writes one JSON-RPC request line and reads one response line.
func (b *stdioBackend) Call(ctx context.Context, req *Request) (*Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	if err := b.ensureRunning(); err != nil {
		return nil, err
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("stdio backend %s: marshal request: %w", b.name, err)
	}
	// Guard the blocking write/read with the context: if the context is
	// cancelled, kill the child so the I/O unblocks.
	done := make(chan struct{})
	defer close(done)
	go watchForCancel(ctx, b.cmd, done)

	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("stdio backend %s: write request: %w", b.name, err)
	}
	respLine, err := b.stdout.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("stdio backend %s: request timed out: %w", b.name, ctx.Err())
		}
		return nil, fmt.Errorf("stdio backend %s: read response: %w", b.name, err)
	}
	var resp Response
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return nil, fmt.Errorf("stdio backend %s: parse response: %w", b.name, err)
	}
	if resp.JSONRPC != "2.0" {
		return nil, fmt.Errorf("stdio backend %s: invalid jsonrpc version %q", b.name, resp.JSONRPC)
	}
	return &resp, nil
}

// Close kills the child process.
func (b *stdioBackend) Close() error {
	if b.cmd == nil || b.cmd.Process == nil {
		return nil
	}
	err := b.cmd.Process.Kill()
	_ = b.cmd.Wait()
	b.cmd = nil
	return err
}

// ---------------------------------------------------------------------------
// http backend
// ---------------------------------------------------------------------------

// httpBackend posts JSON-RPC requests to a Streamable HTTP MCP endpoint.
type httpBackend struct {
	name    string
	cfg     config.Backend
	timeout time.Duration
	client  *http.Client
}

func newHTTPBackend(cfg config.Backend) *httpBackend {
	return &httpBackend{
		name:    cfg.Name,
		cfg:     cfg,
		timeout: timeout(cfg),
		client:  &http.Client{},
	}
}

func (b *httpBackend) Name() string { return b.name }

// Notify POSTs a JSON-RPC notification (no id) to the Streamable HTTP
// endpoint. Per the MCP spec, servers answer notifications with 202
// Accepted and no body; 200 is accepted too for lenient servers. The
// response body, if any, is drained and ignored.
func (b *httpBackend) Notify(ctx context.Context, method string, params map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	body, err := json.Marshal(&Request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("http backend %s: marshal notification: %w", b.name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("http backend %s: build notification request: %w", b.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	switch b.cfg.Auth.Type {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+b.cfg.Auth.Token)
	case "basic":
		creds := b.cfg.Auth.Username + ":" + b.cfg.Auth.Password
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http backend %s: notification request failed: %w", b.name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("http backend %s: notification got unexpected status %d", b.name, resp.StatusCode)
	}
	return nil
}

// Call POSTs the request and parses the JSON-RPC response.
func (b *httpBackend) Call(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("http backend %s: marshal request: %w", b.name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http backend %s: build request: %w", b.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	switch b.cfg.Auth.Type {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+b.cfg.Auth.Token)
	case "basic":
		creds := b.cfg.Auth.Username + ":" + b.cfg.Auth.Password
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http backend %s: request failed: %w", b.name, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("http backend %s: read response: %w", b.name, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("http backend %s: unexpected status %d: %s", b.name, resp.StatusCode, truncate(respBody, 200))
	}
	// An MCP server may answer a Streamable HTTP POST with an SSE stream
	// instead of a plain JSON body. Read the stream and return the
	// JSON-RPC response whose id matches the request; true streaming
	// passthrough of events to the client is not supported yet.
	ct := resp.Header.Get("Content-Type")
	if isSSEContentType(ct) {
		return parseSSEResponse(bytes.NewReader(respBody), req.ID, b.name)
	}
	if !isJSONContentType(ct) {
		return nil, fmt.Errorf("http backend %s: unsupported content type %q", b.name, ct)
	}
	var rpcResp Response
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("http backend %s: parse response: %w", b.name, err)
	}
	if rpcResp.JSONRPC != "2.0" {
		return nil, fmt.Errorf("http backend %s: invalid jsonrpc version %q", b.name, rpcResp.JSONRPC)
	}
	return &rpcResp, nil
}

func (b *httpBackend) Close() error { return nil }

// parseSSEResponse reads an SSE (text/event-stream) response body and
// returns the JSON-RPC response whose id matches wantID.
//
// Per the MCP Streamable HTTP spec, a POST response stream carries one
// JSON-RPC message per event's data field; other messages in the stream
// (progress notifications, server pings) are skipped. Multi-line data
// fields are joined with newlines. Events whose data is not a JSON-RPC 2.0
// message are skipped rather than failing the whole call: the id match is
// the authority on which message answers this request.
func parseSSEResponse(r io.Reader, wantID any, backendName string) (*Response, error) {
	want, err := json.Marshal(wantID)
	if err != nil {
		return nil, fmt.Errorf("http backend %s: marshal request id: %w", backendName, err)
	}
	var data []string
	sawData := false
	// flushEvent checks the data accumulated for one event against the
	// request id. It returns the matching response, or nil to keep
	// scanning the stream.
	flushEvent := func() *Response {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var msg Response
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil // not a JSON-RPC message; skip
		}
		if msg.JSONRPC != "2.0" {
			return nil
		}
		id, err := json.Marshal(msg.ID)
		if err != nil || string(id) != string(want) {
			return nil
		}
		return &msg
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20) // data lines up to 1MB
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			// Blank line: event boundary.
			if m := flushEvent(); m != nil {
				return m, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / heartbeat
		}
		name, value := splitSSEField(line)
		if name != "data" {
			continue // event:, id:, retry: fields carry no payload
		}
		sawData = true
		data = append(data, value)
	}
	// The stream may end without a trailing blank line.
	if m := flushEvent(); m != nil {
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("http backend %s: read SSE stream: %w", backendName, err)
	}
	if !sawData {
		return nil, fmt.Errorf("http backend %s: SSE stream contained no data events", backendName)
	}
	return nil, fmt.Errorf("http backend %s: SSE stream had no JSON-RPC response matching the request id", backendName)
}

// splitSSEField splits an SSE field line at the first colon. A single
// leading space after the colon is stripped per the SSE spec.
func splitSSEField(line string) (name, value string) {
	if i := strings.IndexByte(line, ':'); i >= 0 {
		name, value = line[:i], line[i+1:]
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		return name, value
	}
	return line, ""
}

func isSSEContentType(ct string) bool {
	// Tolerate parameters such as "; charset=utf-8".
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	return strings.TrimSpace(ct) == "text/event-stream"
}

func isJSONContentType(ct string) bool {
	// Tolerate parameters such as "; charset=utf-8".
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	return ct == "application/json" || ct == "text/json"
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
