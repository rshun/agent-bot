package antigravity

import (
	"strings"
	"testing"
)

func TestParseStreamSuccessToolsAndUsage(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"event":"init","conversation_id":"conv-123","init":{"cwd":"/tmp/ws","tools":["view_file","run_command"],"permission_mode":"request-review"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":0,"state":"DONE","step_type":"user_input"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":1,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"echo hi"}}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"echo hi"},"output":"hi\n"}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":2,"state":"DONE","step_type":"tool","tool_name":"call_mcp_tool","tool_info":{"name":"call_mcp_tool","parameters":{"ServerName":"agentbot","ToolName":"pronounce","Arguments":{"text":"hello"}}}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":3,"state":"DONE","step_type":"tool","tool_name":"generate_image","tool_info":{"name":"generate_image","parameters":{"Prompt":"cat"},"output":"{\"path\":\"/home/agy/.gemini/antigravity-cli/brain/conv-123/cat.png\"}"}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":4,"state":"ACTIVE","step_type":"agent_response","text_delta":"Hel"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-123","step_index":4,"state":"DONE","step_type":"agent_response","text_delta":"lo"}}`,
		`{"event":"result","result":{"conversation_id":"conv-123","status":"SUCCESS","response":"Hello","usage":{"input_tokens":100,"cache_read_tokens":40,"output_tokens":20,"thinking_tokens":5}}}`,
	}, "\n")

	var tools []ToolUse
	res, err := ParseStream(strings.NewReader(ndjson), func(tu ToolUse) {
		tools = append(tools, tu)
	})
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if res.SessionID != "conv-123" || res.Text != "Hello" {
		t.Fatalf("unexpected result: %#v", res)
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 deduplicated tool calls, got %d: %#v", len(tools), tools)
	}
	if tools[0].Name != "run_command" || tools[1].Name != "agentbot__pronounce" || tools[2].Name != "generate_image" {
		t.Fatalf("unexpected tool names: %#v", tools)
	}
	if len(res.GeneratedImages) != 1 || res.GeneratedImages[0] != "/home/agy/.gemini/antigravity-cli/brain/conv-123/cat.png" {
		t.Fatalf("unexpected GeneratedImages: %#v", res.GeneratedImages)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 100 || *res.Usage.CachedInputTokens != 40 || *res.Usage.OutputTokens != 20 || *res.Usage.ReasoningOutputTokens != 5 {
		t.Fatalf("unexpected usage: %#v", res.Usage)
	}
}

func TestParseStreamErrorStatus(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"event":"init","conversation_id":"conv-err"}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-err","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"partial"}}`,
		`{"event":"result","result":{"conversation_id":"conv-err","status":"ERROR","error":"authentication required"}}`,
	}, "\n")

	_, err := ParseStream(strings.NewReader(ndjson), nil)
	if err == nil {
		t.Fatal("expected error for ERROR status")
	}
	re, ok := err.(*RunError)
	if !ok || re.SessionID != "conv-err" || re.Message != "authentication required" || re.PartialText != "partial" {
		t.Fatalf("unexpected RunError: %#v", err)
	}
}
