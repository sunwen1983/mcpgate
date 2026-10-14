// Command echo-backend is a minimal MCP server that speaks JSON-RPC 2.0
// over stdio. It exists so mcpgate has something real to proxy in the demo
// and tests. It implements the three methods the gateway exercises:
//
//   - initialize: returns the server's protocol version and capabilities.
//   - tools/list: returns the echo and add tools.
//   - tools/call: echo returns its input unchanged; add sums two numbers.
//
// It reads one JSON object per line from stdin and writes one JSON object
// per line to stdout. Malformed lines get a JSON-RPC -32700 parse error.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      any     `json:"id,omitempty"`
	Result  any     `json:"result,omitempty"`
	Error   *rpcErr `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func respond(w *bufio.Writer, id any, result any, err *rpcErr) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: err}
	line, _ := json.Marshal(resp)
	fmt.Fprintln(w, string(line))
	_ = w.Flush()
}

func handle(w *bufio.Writer, req rpcRequest) {
	switch req.Method {
	case "initialize":
		respond(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "echo-backend", "version": "0.1.0"},
		}, nil)
	case "tools/list":
		respond(w, req.ID, map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Return the input text unchanged.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"},
					},
				},
				map[string]any{
					"name":        "add",
					"description": "Add two numbers.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}},
						"required":   []string{"a", "b"},
					},
				},
			},
		}, nil)
	case "tools/call":
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		switch name {
		case "echo":
			text, _ := args["text"].(string)
			respond(w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
			}, nil)
		case "add":
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			respond(w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("%v", a+b)}},
			}, nil)
		default:
			respond(w, req.ID, nil, &rpcErr{Code: -32602, Message: "unknown tool: " + name})
		}
	case "notifications/initialized", "notifications/cancelled":
		// Notifications carry no id and get no response.
	default:
		respond(w, req.ID, nil, &rpcErr{Code: -32601, Message: "method not found: " + req.Method})
	}
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			respond(writer, nil, nil, &rpcErr{Code: -32700, Message: "parse error"})
			continue
		}
		handle(writer, req)
	}
}
