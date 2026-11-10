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
	"errors"
	"fmt"
	"io"
	"mime"
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

// Streamer is implemented by backends that can relay a live event
// stream for a request. The HTTP backend implements it: an SSE
// (text/event-stream) answer is yielded event by event as it arrives,
// and a plain JSON answer is yielded as a single event, so callers
// drive one code path either way. The stdio backend does not
// implement it — a child process speaks newline-delimited JSON-RPC,
// not SSE — so callers fall back to Call for stdio backends.
type Streamer interface {
	// Stream issues the request and returns the backend's event
	// stream. Stream returns an error before any event is yielded
	// when the backend cannot be reached or answers with an
	// unexpected status, so the caller can still answer with a
	// normal error response. The caller must Close the stream when
	// done (or when it stops reading early) so the backend request
	// is cancelled and the connection released.
	Stream(ctx context.Context, req *Request) (EventStream, error)
}

// EventStream yields one SSE event per Next call: the event's data
// payload, with multi-line data fields joined with "\n". Events
// arrive in backend order. Next returns io.EOF when the backend
// closes the stream. Whether the stream carried a JSON-RPC response
// matching the request is the caller's decision, not the stream's:
// a stream of pure heartbeats simply ends with io.EOF.
type EventStream interface {
	Next() ([]byte, error)
	Close() error
}

// New builds the Backend for a backend config entry.
func New(cfg config.Backend) (Backend, error) {
	switch cfg.Transport {
	case "stdio":
		return newStdioBackend(cfg)
	case "http":
		if len(cfg.Endpoints) > 0 {
			return newPoolBackend(cfg, cfg.Endpoints, poolFailureThreshold, poolCooldown)
		}
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

// answerServerRequest replies to a server-initiated request with a
// JSON-RPC -32601 error: mcpgate never handles server requests (such as
// sampling/createMessage), but the server is blocked waiting for an
// answer, so silence would hang the in-flight call. The caller holds
// b.mu, so this write cannot interleave with another request.
func (b *stdioBackend) answerServerRequest(rawID json.RawMessage) error {
	var id any
	if err := json.Unmarshal(rawID, &id); err != nil {
		return fmt.Errorf("stdio backend %s: parse server request id: %w", b.name, err)
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32601, "message": "method not supported"},
	})
	if err != nil {
		return fmt.Errorf("stdio backend %s: marshal server request error: %w", b.name, err)
	}
	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("stdio backend %s: write server request error: %w", b.name, err)
	}
	return nil
}

// Call writes one JSON-RPC request line and reads lines until the
// response matching the request id arrives.
//
// A stdio server may send notifications (logging, progress) before the
// response — MCP servers do this whenever a request carries a progress
// token or the server just wants to talk. Those lines are not responses:
// returning the first line verbatim would hand the caller an empty
// "response" and leave the real response to corrupt the next call. So
// notification lines (a method, no id) are skipped, as are responses
// carrying a different id, mirroring the HTTP SSE path's id-match rule.
//
// A line carrying a method is a server-initiated message, never our
// response — even when its id happens to equal the request id (servers
// that number their own requests from 1 collide with ours constantly).
// Returning such a line as the response would hand the caller the
// server's request as if it were the answer. Server requests (a method
// and an id) are answered with a -32601 error instead of being dropped:
// the server is waiting for that answer before it sends our response,
// so dropping it would hang the call until the context timeout kills
// the child. A server that never answers still terminates the loop:
// the context timeout kills the child, which unblocks the read below.
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
	wantID, err := json.Marshal(req.ID)
	if err != nil {
		return nil, fmt.Errorf("stdio backend %s: marshal request id: %w", b.name, err)
	}
	for {
		respLine, err := readLine(b.stdout, maxStdioLineBytes)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				// The rest of the line is still in the pipe and
				// would corrupt the next call, so drop the child;
				// the next call restarts it via ensureRunning.
				_ = b.Close()
				return nil, fmt.Errorf("stdio backend %s: response line exceeds %d bytes", b.name, maxStdioLineBytes)
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("stdio backend %s: request timed out: %w", b.name, ctx.Err())
			}
			return nil, fmt.Errorf("stdio backend %s: read response: %w", b.name, err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(respLine, &raw); err != nil {
			return nil, fmt.Errorf("stdio backend %s: parse response: %w", b.name, err)
		}
		_, hasID := raw["id"]
		_, hasMethod := raw["method"]
		if hasMethod {
			// Server-initiated message, never our response.
			if !hasID {
				continue // server notification, not the response
			}
			// Server request: answer -32601 (mcpgate does not
			// implement server-request handling such as sampling)
			// so the server can proceed to our real response.
			if err := b.answerServerRequest(raw["id"]); err != nil {
				return nil, err
			}
			continue
		}
		if !hasID {
			return nil, fmt.Errorf("stdio backend %s: response has no id", b.name)
		}
		var resp Response
		if err := json.Unmarshal(respLine, &resp); err != nil {
			return nil, fmt.Errorf("stdio backend %s: parse response: %w", b.name, err)
		}
		if resp.JSONRPC != "2.0" {
			return nil, fmt.Errorf("stdio backend %s: invalid jsonrpc version %q", b.name, resp.JSONRPC)
		}
		gotID, err := json.Marshal(resp.ID)
		if err != nil || string(gotID) != string(wantID) {
			continue // a response to some other request; keep waiting
		}
		return &resp, nil
	}
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

// newRequest builds the POST for one JSON-RPC message, with the
// headers and backend auth Call, Notify, and Stream all share.
// Static headers are applied first, so backend auth always wins on
// a name clash.
func (b *httpBackend) newRequest(ctx context.Context, req *Request) (*http.Request, error) {
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
	b.setHeaders(httpReq)
	switch b.cfg.Auth.Type {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+b.cfg.Auth.Token)
	case "basic":
		creds := b.cfg.Auth.Username + ":" + b.cfg.Auth.Password
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	return httpReq, nil
}

// setHeaders injects the configured static headers into an outgoing
// backend request. It runs before backend auth is applied, so auth
// always wins over a configured header with the same name.
func (b *httpBackend) setHeaders(httpReq *http.Request) {
	for k, v := range b.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
}

// Notify POSTs a JSON-RPC notification (no id) to the Streamable HTTP
// endpoint. Per the MCP spec, servers answer notifications with 202
// Accepted and no body; 200 is accepted too for lenient servers. The
// response body, if any, is drained and ignored.
func (b *httpBackend) Notify(ctx context.Context, method string, params map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	httpReq, err := b.newRequest(ctx, &Request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
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

	httpReq, err := b.newRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http backend %s: request failed: %w", b.name, err)
	}
	defer resp.Body.Close()
	// An MCP server may answer a Streamable HTTP POST with an SSE stream
	// instead of a plain JSON body. Parse the stream incrementally and
	// return as soon as the JSON-RPC response matching the request id
	// arrives: reading the whole body first would hang until the server
	// closes a stream it intends to keep open (long-lived SSE
	// connections), turning every call into a timeout. True streaming
	// passthrough of events to the client is not supported yet.
	ct := resp.Header.Get("Content-Type")
	if isSSEContentType(ct) {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return nil, fmt.Errorf("http backend %s: unexpected status %d: %s", b.name, resp.StatusCode, truncate(respBody, 200))
		}
		return parseSSEResponse(io.LimitReader(resp.Body, 10<<20), req.ID, b.name)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("http backend %s: read response: %w", b.name, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("http backend %s: unexpected status %d: %s", b.name, resp.StatusCode, truncate(respBody, 200))
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
	// A body carrying a method is a server-initiated message, not a
	// response — never return it as the answer to our request, even
	// when its id collides with the request id.
	var probe struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(respBody, &probe); err == nil && probe.Method != "" {
		return nil, fmt.Errorf("http backend %s: response body is a server request, not a response", b.name)
	}
	// The response must answer this request: the SSE and stdio paths
	// both enforce the id match, and the plain-JSON path must too.
	// Without it, a stale or misrouted response would be silently
	// relabeled with the client's id by the gateway and served as
	// the answer.
	wantID, err := json.Marshal(req.ID)
	if err != nil {
		return nil, fmt.Errorf("http backend %s: marshal request id: %w", b.name, err)
	}
	gotID, err := json.Marshal(rpcResp.ID)
	if err != nil || string(gotID) != string(wantID) {
		return nil, fmt.Errorf("http backend %s: response id %s does not match request id %s", b.name, gotID, wantID)
	}
	return &rpcResp, nil
}

// Stream POSTs the request and returns the backend's event stream.
//
// An SSE (text/event-stream) answer is yielded event by event as it
// arrives, so a caller can relay progress notifications live instead
// of waiting for the response event. A plain JSON answer is yielded
// as a single event carrying the raw body. The caller classifies
// events (which one answers the request) and decides when to stop
// reading; Close cancels the backend request and releases the
// connection. The configured timeout bounds the whole stream, from
// the first byte to the last event read.
//
// An unexpected status or content type fails before any event is
// yielded, so the caller can still answer with a normal error
// response. No total-size cap applies to a live stream: events are
// relayed, not retained, so a long stream cannot grow gateway
// memory; individual data lines are still capped at 1MB by the SSE
// scanner, and the request timeout bounds the stream's lifetime.
func (b *httpBackend) Stream(ctx context.Context, req *Request) (EventStream, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	httpReq, err := b.newRequest(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("http backend %s: request failed: %w", b.name, err)
	}
	ct := resp.Header.Get("Content-Type")
	if isSSEContentType(ct) {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("http backend %s: unexpected status %d: %s", b.name, resp.StatusCode, truncate(respBody, 200))
		}
		return &httpEventStream{name: b.name, cancel: cancel, body: resp.Body, sc: newSSEScanner(resp.Body)}, nil
	}
	// Plain JSON: read the whole body (bounded like Call) and yield it
	// as one event. The request is complete, so its timeout can go.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	resp.Body.Close()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("http backend %s: read response: %w", b.name, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("http backend %s: unexpected status %d: %s", b.name, resp.StatusCode, truncate(body, 200))
	}
	if !isJSONContentType(ct) {
		return nil, fmt.Errorf("http backend %s: unsupported content type %q", b.name, ct)
	}
	return &singleEventStream{data: body}, nil
}

// httpEventStream yields SSE events from a live backend response.
type httpEventStream struct {
	name   string
	cancel context.CancelFunc
	body   io.ReadCloser
	sc     *sseScanner
	closed bool
}

func (s *httpEventStream) Next() ([]byte, error) {
	payload, err := s.sc.Next()
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("http backend %s: read SSE stream: %w", s.name, err)
	}
	return []byte(payload), nil
}

// Close cancels the backend request and releases the connection.
// Safe to call more than once; a stream drained to EOF still needs
// Close so the in-flight request is cancelled instead of held open
// until the timeout.
func (s *httpEventStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	return s.body.Close()
}

// singleEventStream yields one pre-read payload, then EOF.
type singleEventStream struct {
	data []byte
	done bool
}

func (s *singleEventStream) Next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return s.data, nil
}

func (s *singleEventStream) Close() error { return nil }

// maxStdioLineBytes caps one stdio response line — the stdio analogue
// of the HTTP path's 10MB body cap. Without it a backend emitting an
// endless line would grow the gateway's memory without bound:
// ReadBytes keeps appending until a newline arrives, and a stuck read
// only ends when the call timeout kills the child.
const maxStdioLineBytes = 10 << 20

var errLineTooLong = errors.New("line exceeds maximum length")

// readLine reads one newline-terminated line from r, refusing lines
// longer than limit. Fragments are accumulated with ReadSlice so a line
// longer than the reader's buffer still works; the accumulated length is
// checked on every fragment so an endless line is cut off at the limit
// instead of growing memory.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > limit {
			return nil, errLineTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func (b *httpBackend) Close() error { return nil }

// sseScanner iterates the data payloads of an SSE (text/event-stream)
// body, one event per Next call. Comment lines, non-data fields
// (event:, id:, retry:), and events with no data are skipped — they
// carry no payload. Multi-line data fields are joined with newlines.
// Data lines are capped at 1MB by the scanner. A stream that ends
// without a trailing blank line still yields its last event.
//
// Next returns io.EOF when the stream is exhausted. A scan error
// (e.g. a data line past the cap) is reported on the call after the
// last buffered event, so a pending event is never lost to the error.
type sseScanner struct {
	sc   *bufio.Scanner
	data []string
}

func newSSEScanner(r io.Reader) *sseScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20) // data lines up to 1MB
	return &sseScanner{sc: sc}
}

func (s *sseScanner) Next() (string, error) {
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			// Blank line: event boundary.
			if len(s.data) > 0 {
				payload := strings.Join(s.data, "\n")
				s.data = s.data[:0]
				return payload, nil
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
		s.data = append(s.data, value)
	}
	// The stream may end without a trailing blank line.
	if len(s.data) > 0 {
		payload := strings.Join(s.data, "\n")
		s.data = s.data[:0]
		return payload, nil
	}
	if err := s.sc.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

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
	sc := newSSEScanner(r)
	sawData := false
	for {
		payload, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("http backend %s: read SSE stream: %w", backendName, err)
		}
		sawData = true
		var msg Response
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			continue // not a JSON-RPC message; skip
		}
		if msg.JSONRPC != "2.0" {
			continue
		}
		// A server-initiated message (notification or request) is never
		// our response, even when its id collides with the request id.
		var probe struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err == nil && probe.Method != "" {
			continue
		}
		id, err := json.Marshal(msg.ID)
		if err != nil || string(id) != string(want) {
			continue
		}
		return &msg, nil
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

// Content-Type checks parse the header the way RFC 9110 requires: the
// type itself is case-insensitive and parameters (and whitespace around
// them) are not part of it. The previous hand-rolled split rejected
// "Application/JSON" outright and rejected "application/json ; …"
// (space before the semicolon) on the JSON path while accepting it on
// the SSE path, so the two checks disagreed on the same header.
func isSSEContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "text/event-stream"
}

func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "application/json" || mt == "text/json")
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
