// Package transform rewrites MCP tool schemas, call arguments, and
// result payloads at the gateway boundary.
//
// An operator configures per-backend schema rewrites (see
// config.SchemaRewrite) to present a cleaner tool surface than the
// backend exposes: rename an argument (a backend's `snake_case` name
// becomes the client's `camelCase` name, say), or hide an argument
// clients should never set. Tool names are never rewritten — the tool
// filter, per-tool authorization, and cache keys all keep using the
// backend's tool name.
//
// The two directions are kept separate on purpose:
//
//   - RewriteTool / RewriteToolsList reshape what tools/list shows:
//     renamed arguments appear under their client-facing name in
//     properties and required, hidden arguments disappear.
//   - TranslateArguments maps a tools/call's client-facing arguments
//     back to the backend's names before the call is forwarded, and
//     strips hidden arguments a client sent anyway.
//
// A rewrite naming an argument the backend does not expose is a
// no-op for that argument: backends may serve slightly different
// schemas, and a rewrite must not invent properties. Unknown client
// arguments pass through translation unchanged.
//
// Response rewriting is the mirror image, applied to tools/call
// results instead of requests: RewriteResult renames and hides
// fields in the result payload (structuredContent, and text content
// items that parse as JSON) per config.ResponseRewrite, so a backend
// that returns internal field names or fields clients should never
// see can be cleaned up at the boundary. Like the request side, a
// rewrite naming a field the backend does not return is a no-op for
// that field.
package transform

import (
	"encoding/json"
	"reflect"

	"github.com/sunwen1983/mcpgate/internal/config"
)

// Find returns the rewrite configured for tool, if one exists.
func Find(rewrites []config.SchemaRewrite, tool string) (config.SchemaRewrite, bool) {
	for _, rw := range rewrites {
		if rw.Tool == tool {
			return rw, true
		}
	}
	return config.SchemaRewrite{}, false
}

// RewriteToolsList applies each tool's rewrite, by name, to a
// tools/list result. Entries without a recognizable name, or with no
// configured rewrite, pass through untouched. The input slice is
// never modified; rewritten tools are fresh maps.
func RewriteToolsList(tools []any, rewrites []config.SchemaRewrite) []any {
	if len(rewrites) == 0 {
		return tools
	}
	out := make([]any, len(tools))
	for i, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			out[i] = item
			continue
		}
		name, _ := tool["name"].(string)
		rw, ok := Find(rewrites, name)
		if !ok {
			out[i] = item
			continue
		}
		out[i] = RewriteTool(tool, rw)
	}
	return out
}

// RewriteTool returns a copy of tool with rw applied to its
// inputSchema: renamed arguments appear under their client-facing
// names (in properties and in required), hidden arguments are
// removed. A tool without an object inputSchema is returned
// unchanged. The input map is never modified.
func RewriteTool(tool map[string]any, rw config.SchemaRewrite) map[string]any {
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok {
		return tool
	}
	out := make(map[string]any, len(tool))
	for k, v := range tool {
		out[k] = v
	}
	// Deep-copy the schema subtree we are about to change, so the
	// caller's map (which may be a cached backend payload) is never
	// mutated.
	newSchema := make(map[string]any, len(schema))
	for k, v := range schema {
		newSchema[k] = v
	}
	out["inputSchema"] = newSchema

	if props, ok := schema["properties"].(map[string]any); ok {
		newProps := make(map[string]any, len(props))
		for name, prop := range props {
			if hidden(rw, name) {
				continue
			}
			if renamed, ok := rw.RenameArguments[name]; ok {
				newProps[renamed] = prop
				continue
			}
			newProps[name] = prop
		}
		newSchema["properties"] = newProps
	}
	if req, ok := schema["required"].([]any); ok {
		newReq := make([]any, 0, len(req))
		for _, item := range req {
			name, ok := item.(string)
			if !ok {
				newReq = append(newReq, item)
				continue
			}
			if hidden(rw, name) {
				continue
			}
			if renamed, ok := rw.RenameArguments[name]; ok {
				newReq = append(newReq, renamed)
				continue
			}
			newReq = append(newReq, item)
		}
		newSchema["required"] = newReq
	}
	return out
}

// TranslateArguments maps client-facing argument names back to the
// backend's names, dropping hidden arguments. Unknown arguments pass
// through unchanged. A nil map stays nil; the input is never
// modified.
func TranslateArguments(args map[string]any, rw config.SchemaRewrite) map[string]any {
	if args == nil {
		return nil
	}
	// Invert the rename map once: client name -> backend name.
	inverse := make(map[string]string, len(rw.RenameArguments))
	for backend, client := range rw.RenameArguments {
		inverse[client] = backend
	}
	out := make(map[string]any, len(args))
	for name, value := range args {
		if _, isClientName := inverse[name]; isClientName {
			// Renamed arguments are applied in the second pass
			// below, so the client-facing name wins if a client
			// sends both it and the backend name.
			continue
		}
		if hidden(rw, name) {
			// A client sent an argument the exposed schema hides:
			// drop it rather than forwarding something the
			// backend was never meant to receive from clients.
			continue
		}
		out[name] = value
	}
	for client, backend := range inverse {
		if value, ok := args[client]; ok {
			out[backend] = value
		}
	}
	return out
}

// hidden reports whether name is in rw's hide list.
func hidden(rw config.SchemaRewrite, name string) bool {
	for _, h := range rw.HideArguments {
		if h == name {
			return true
		}
	}
	return false
}

// FindResponseRewrite returns the response rewrite configured for
// tool, if one exists.
func FindResponseRewrite(rewrites []config.ResponseRewrite, tool string) (config.ResponseRewrite, bool) {
	for _, rw := range rewrites {
		if rw.Tool == tool {
			return rw, true
		}
	}
	return config.ResponseRewrite{}, false
}

// RewriteResult applies rw to a tools/call result: every JSON object
// in the result has its fields renamed and hidden per rw, at any
// depth. That covers structuredContent (when it is an object) and
// any text content item whose text parses as JSON; anything else —
// non-JSON text, image or resource content, a result without those
// keys — passes through untouched. The input map is never modified;
// rewritten subtrees are fresh maps. A result that parses but needs
// no change keeps its original text bytes, so a rewrite never
// reformats a payload it does not touch.
func RewriteResult(result map[string]any, rw config.ResponseRewrite) map[string]any {
	out := make(map[string]any, len(result))
	for k, v := range result {
		out[k] = v
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		out["structuredContent"] = rewriteValue(sc, rw)
	} else if sc, ok := result["structuredContent"].([]any); ok {
		out["structuredContent"] = rewriteValue(sc, rw)
	}
	if content, ok := result["content"].([]any); ok {
		newContent := make([]any, len(content))
		for i, item := range content {
			newContent[i] = rewriteContentItem(item, rw)
		}
		out["content"] = newContent
	}
	return out
}

// rewriteContentItem rewrites one result content item: a text item
// whose text parses as JSON gets its fields rewritten (and is
// re-encoded only when the rewrite changed something); every other
// item passes through untouched.
func rewriteContentItem(item any, rw config.ResponseRewrite) any {
	m, ok := item.(map[string]any)
	if !ok {
		return item
	}
	text, ok := m["text"].(string)
	if m["type"] != "text" || !ok {
		return item
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return item // not JSON: leave the text alone
	}
	rewritten := rewriteValue(parsed, rw)
	if reflect.DeepEqual(rewritten, parsed) {
		return item
	}
	encoded, err := json.Marshal(rewritten)
	if err != nil {
		return item // unreachable for JSON-decoded values; stay safe
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out["text"] = string(encoded)
	return out
}

// rewriteValue renames and hides fields in v, recursing into nested
// objects and arrays. Scalars pass through unchanged. The input is
// never modified: every map or slice on a rewritten path is a fresh
// copy. A renamed field wins over a pre-existing field with the
// target name, so the result does not depend on map iteration order.
func rewriteValue(v any, rw config.ResponseRewrite) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for name, value := range t {
			if hideField(rw, name) {
				continue
			}
			if _, isSource := rw.RenameFields[name]; isSource {
				continue // applied in the second pass below
			}
			out[name] = rewriteValue(value, rw)
		}
		for old, new := range rw.RenameFields {
			if value, ok := t[old]; ok {
				out[new] = rewriteValue(value, rw)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = rewriteValue(value, rw)
		}
		return out
	default:
		return v
	}
}

// hideField reports whether name is in rw's hide list.
func hideField(rw config.ResponseRewrite, name string) bool {
	for _, h := range rw.HideFields {
		if h == name {
			return true
		}
	}
	return false
}
