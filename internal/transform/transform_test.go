package transform_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sunwen1983/mcpgate/internal/config"
	"github.com/sunwen1983/mcpgate/internal/transform"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func props(t *testing.T, tool map[string]any) map[string]any {
	t.Helper()
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema = %#v, want object", tool["inputSchema"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v, want object", schema["properties"])
	}
	return props
}

func required(t *testing.T, tool map[string]any) []string {
	t.Helper()
	schema, _ := tool["inputSchema"].(map[string]any)
	raw, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("required entry = %#v, want string", v)
		}
		out = append(out, s)
	}
	return out
}

func TestRewriteToolRenamesArguments(t *testing.T) {
	tool := mustJSON(t, `{"name":"add","inputSchema":{
		"type":"object",
		"properties":{"a":{"type":"number"},"b":{"type":"number"}},
		"required":["a","b"]}}`)
	rw := config.SchemaRewrite{Tool: "add", RenameArguments: map[string]string{"a": "first", "b": "second"}}

	got := transform.RewriteTool(tool, rw)
	p := props(t, got)
	if _, ok := p["first"]; !ok {
		t.Errorf("properties missing renamed %q: %v", "first", p)
	}
	if _, ok := p["second"]; !ok {
		t.Errorf("properties missing renamed %q: %v", "second", p)
	}
	if _, ok := p["a"]; ok {
		t.Errorf("properties still has backend name %q", "a")
	}
	if _, ok := p["b"]; ok {
		t.Errorf("properties still has backend name %q", "b")
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(required(t, got), want) {
		t.Errorf("required = %v, want %v", required(t, got), want)
	}
	// The property's own schema survives the rename.
	if p["first"].(map[string]any)["type"] != "number" {
		t.Errorf("renamed property lost its schema: %#v", p["first"])
	}
	// The input is never modified: the backend's own view is untouched.
	if _, ok := props(t, tool)["first"]; ok {
		t.Errorf("RewriteTool modified its input")
	}
}

func TestRewriteToolHidesArguments(t *testing.T) {
	tool := mustJSON(t, `{"name":"add","inputSchema":{
		"type":"object",
		"properties":{"a":{"type":"number"},"debug":{"type":"boolean"}},
		"required":["a","debug"]}}`)
	rw := config.SchemaRewrite{Tool: "add", HideArguments: []string{"debug"}}

	got := transform.RewriteTool(tool, rw)
	p := props(t, got)
	if _, ok := p["debug"]; ok {
		t.Errorf("properties still has hidden %q", "debug")
	}
	if _, ok := p["a"]; !ok {
		t.Errorf("properties lost unhidden %q", "a")
	}
	if want := []string{"a"}; !reflect.DeepEqual(required(t, got), want) {
		t.Errorf("required = %v, want %v", required(t, got), want)
	}
}

func TestRewriteToolRenameAndHideTogether(t *testing.T) {
	tool := mustJSON(t, `{"name":"search","inputSchema":{
		"type":"object",
		"properties":{"q":{"type":"string"},"limit":{"type":"number"},"internal":{"type":"string"}},
		"required":["q"]}}`)
	rw := config.SchemaRewrite{
		Tool:            "search",
		RenameArguments: map[string]string{"q": "query"},
		HideArguments:   []string{"internal"},
	}
	got := transform.RewriteTool(tool, rw)
	p := props(t, got)
	if len(p) != 2 {
		t.Fatalf("properties = %v, want query+limit", p)
	}
	if _, ok := p["query"]; !ok {
		t.Errorf("properties missing renamed %q", "query")
	}
	if _, ok := p["limit"]; !ok {
		t.Errorf("properties missing untouched %q", "limit")
	}
}

func TestRewriteToolIgnoresUnknownArguments(t *testing.T) {
	// A rewrite may name an argument the backend does not expose
	// (schemas can vary); it is a no-op for that argument, not an error.
	tool := mustJSON(t, `{"name":"add","inputSchema":{
		"type":"object",
		"properties":{"a":{"type":"number"}},
		"required":["a"]}}`)
	rw := config.SchemaRewrite{
		Tool:            "add",
		RenameArguments: map[string]string{"missing": "renamed"},
		HideArguments:   []string{"also_missing"},
	}
	got := transform.RewriteTool(tool, rw)
	if want := []string{"a"}; !reflect.DeepEqual(required(t, got), want) {
		t.Errorf("required = %v, want %v", required(t, got), want)
	}
	if _, ok := props(t, got)["renamed"]; ok {
		t.Errorf("rename of an absent argument created a property")
	}
}

func TestRewriteToolWithoutSchemaIsUntouched(t *testing.T) {
	tool := map[string]any{"name": "ping"}
	rw := config.SchemaRewrite{Tool: "ping", RenameArguments: map[string]string{"a": "b"}}
	got := transform.RewriteTool(tool, rw)
	if !reflect.DeepEqual(got, tool) {
		t.Errorf("got = %v, want unchanged %v", got, tool)
	}

	// A non-object inputSchema is likewise left alone, not crashed on.
	tool = map[string]any{"name": "ping", "inputSchema": "not-an-object"}
	got = transform.RewriteTool(tool, rw)
	if !reflect.DeepEqual(got, tool) {
		t.Errorf("got = %v, want unchanged %v", got, tool)
	}
}

func TestRewriteToolsListAppliesPerTool(t *testing.T) {
	tools := []any{
		mustJSON(t, `{"name":"add","inputSchema":{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}}`),
		mustJSON(t, `{"name":"echo","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}`),
		"not-a-tool",
		map[string]any{"description": "no name"},
	}
	rewrites := []config.SchemaRewrite{
		{Tool: "add", RenameArguments: map[string]string{"a": "first"}},
	}
	got := transform.RewriteToolsList(tools, rewrites)
	if len(got) != len(tools) {
		t.Fatalf("len = %d, want %d", len(got), len(tools))
	}
	if _, ok := props(t, got[0].(map[string]any))["first"]; !ok {
		t.Errorf("add was not rewritten: %#v", got[0])
	}
	if _, ok := props(t, got[1].(map[string]any))["text"]; !ok {
		t.Errorf("echo must be untouched: %#v", got[1])
	}
	if got[2] != "not-a-tool" {
		t.Errorf("non-object entry changed: %#v", got[2])
	}
	if !reflect.DeepEqual(got[3], tools[3]) {
		t.Errorf("unnamed entry changed: %#v", got[3])
	}
}

func TestTranslateArguments(t *testing.T) {
	rw := config.SchemaRewrite{
		Tool:            "add",
		RenameArguments: map[string]string{"a": "first", "b": "second"},
		HideArguments:   []string{"debug"},
	}
	// Client-facing names map back to backend names; hidden arguments
	// are stripped; unknown arguments pass through unchanged.
	got := transform.TranslateArguments(map[string]any{
		"first": 2, "second": 3, "debug": true, "extra": "kept",
	}, rw)
	want := map[string]any{"a": 2, "b": 3, "extra": "kept"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestTranslateArgumentsNilStaysNil(t *testing.T) {
	rw := config.SchemaRewrite{Tool: "add", RenameArguments: map[string]string{"a": "first"}}
	if got := transform.TranslateArguments(nil, rw); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestTranslateArgumentsClientNameWinsOverBackendName(t *testing.T) {
	// A client that sends both the client-facing name and the
	// backend name for one argument gets a deterministic result:
	// the documented client-facing name wins.
	rw := config.SchemaRewrite{Tool: "add", RenameArguments: map[string]string{"a": "first"}}
	got := transform.TranslateArguments(map[string]any{"first": 2, "a": 99}, rw)
	if got["a"] != 2 {
		t.Errorf("got = %v, want a=2 (client-facing name wins)", got)
	}
}

func TestFind(t *testing.T) {
	rewrites := []config.SchemaRewrite{{Tool: "add"}, {Tool: "echo"}}
	if _, ok := transform.Find(rewrites, "missing"); ok {
		t.Errorf("Find(missing) = true")
	}
	rw, ok := transform.Find(rewrites, "echo")
	if !ok || rw.Tool != "echo" {
		t.Errorf("Find(echo) = %+v, %v", rw, ok)
	}
}

func TestRewriteResultStructuredContent(t *testing.T) {
	result := mustJSON(t, `{"structuredContent":{
		"user_id": 7, "user_name": "ada", "internal_id": "x-1",
		"profile": {"user_id": 8, "internal_id": "x-2", "city": "Austin"},
		"tags": [{"user_id": 9, "label": "a"}],
		"count": 3}}`)
	rw := config.ResponseRewrite{
		Tool:         "lookup",
		RenameFields: map[string]string{"user_id": "userId", "user_name": "userName"},
		HideFields:   []string{"internal_id"},
	}
	got := transform.RewriteResult(result, rw)
	sc, ok := got["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %#v, want object", got["structuredContent"])
	}
	if sc["userId"] != float64(7) || sc["userName"] != "ada" {
		t.Errorf("top-level renames missing: %v", sc)
	}
	if _, ok := sc["user_id"]; ok {
		t.Errorf("backend field user_id survived: %v", sc)
	}
	if _, ok := sc["internal_id"]; ok {
		t.Errorf("hidden field internal_id survived: %v", sc)
	}
	// Renames and hides recurse into nested objects and arrays.
	profile, _ := sc["profile"].(map[string]any)
	if profile["userId"] != float64(8) || profile["city"] != "Austin" {
		t.Errorf("nested object not rewritten: %v", profile)
	}
	if _, ok := profile["internal_id"]; ok {
		t.Errorf("nested hidden field survived: %v", profile)
	}
	tags, _ := sc["tags"].([]any)
	tag, _ := tags[0].(map[string]any)
	if tag["userId"] != float64(9) || tag["label"] != "a" {
		t.Errorf("object inside array not rewritten: %v", tag)
	}
	if sc["count"] != float64(3) {
		t.Errorf("untouched scalar changed: %v", sc)
	}
	// The input is never modified.
	orig, _ := result["structuredContent"].(map[string]any)
	if _, ok := orig["userId"]; ok {
		t.Errorf("RewriteResult modified its input")
	}
}

func TestRewriteResultTextContentJSON(t *testing.T) {
	result := mustJSON(t, `{"content":[
		{"type":"text","text":"{\"user_id\": 7, \"internal_id\": \"x\"}"},
		{"type":"text","text":"plain words, not JSON"},
		{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}
	]}`)
	rw := config.ResponseRewrite{
		Tool:         "lookup",
		RenameFields: map[string]string{"user_id": "userId"},
		HideFields:   []string{"internal_id"},
	}
	got := transform.RewriteResult(result, rw)
	content, _ := got["content"].([]any)
	jsonItem, _ := content[0].(map[string]any)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonItem["text"].(string)), &decoded); err != nil {
		t.Fatalf("rewritten text is not JSON: %v", jsonItem["text"])
	}
	if decoded["userId"] != float64(7) {
		t.Errorf("JSON text not renamed: %v", decoded)
	}
	if _, ok := decoded["internal_id"]; ok {
		t.Errorf("hidden field survived in JSON text: %v", decoded)
	}
	// Non-JSON text and non-text items pass through untouched.
	if content[1].(map[string]any)["text"] != "plain words, not JSON" {
		t.Errorf("non-JSON text changed: %v", content[1])
	}
	if !reflect.DeepEqual(content[2], result["content"].([]any)[2]) {
		t.Errorf("image content changed: %v", content[2])
	}
}

func TestRewriteResultLeavesUnchangedTextBytesAlone(t *testing.T) {
	// A rewrite that changes nothing must not reformat the payload.
	result := mustJSON(t, `{"content":[
		{"type":"text","text":"{ \"a\" : 1 }"}
	]}`)
	rw := config.ResponseRewrite{Tool: "lookup", RenameFields: map[string]string{"missing": "renamed"}}
	got := transform.RewriteResult(result, rw)
	content, _ := got["content"].([]any)
	if content[0].(map[string]any)["text"] != `{ "a" : 1 }` {
		t.Errorf("unchanged JSON text was reformatted: %v", content[0])
	}
}

func TestRewriteResultRenamedValueWinsOverExistingTarget(t *testing.T) {
	// Payload already has the target name: the renamed value wins,
	// deterministically, instead of depending on map order.
	result := mustJSON(t, `{"structuredContent":{"user_id": 7, "userId": 99}}`)
	rw := config.ResponseRewrite{Tool: "lookup", RenameFields: map[string]string{"user_id": "userId"}}
	got := transform.RewriteResult(result, rw)
	sc, _ := got["structuredContent"].(map[string]any)
	if sc["userId"] != float64(7) {
		t.Errorf("userId = %v, want 7 (renamed value wins)", sc["userId"])
	}
}

func TestRewriteResultWithoutPayloadKeysIsUntouched(t *testing.T) {
	result := map[string]any{"isError": true, "note": "nothing to rewrite"}
	rw := config.ResponseRewrite{Tool: "lookup", RenameFields: map[string]string{"a": "b"}}
	got := transform.RewriteResult(result, rw)
	if !reflect.DeepEqual(got, result) {
		t.Errorf("got = %v, want unchanged %v", got, result)
	}
}

func TestFindResponseRewrite(t *testing.T) {
	rewrites := []config.ResponseRewrite{{Tool: "lookup"}, {Tool: "echo"}}
	if _, ok := transform.FindResponseRewrite(rewrites, "missing"); ok {
		t.Errorf("FindResponseRewrite(missing) = true")
	}
	rw, ok := transform.FindResponseRewrite(rewrites, "echo")
	if !ok || rw.Tool != "echo" {
		t.Errorf("FindResponseRewrite(echo) = %+v, %v", rw, ok)
	}
}
