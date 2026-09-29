package responsesapi

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// inputImageParts returns every input_image content part found anywhere in a
// decoded request body, so the checks cover all places that can carry images.
func inputImageParts(value any) []map[string]any {
	var parts []map[string]any
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == "input_image" {
			parts = append(parts, value)
		}
		for _, field := range value {
			parts = append(parts, inputImageParts(field)...)
		}
	case []any:
		for _, element := range value {
			parts = append(parts, inputImageParts(element)...)
		}
	}
	return parts
}

// missingImageDetail mirrors servers that validate input_image parts against the
// published schema, where detail is a required field.
func missingImageDetail(body []byte) (bool, error) {
	var request any
	if err := json.Unmarshal(body, &request); err != nil {
		return false, err
	}
	for _, part := range inputImageParts(request) {
		switch part["detail"] {
		case "low", "high", "auto":
		default:
			return true, nil
		}
	}
	return false, nil
}

func imageRequest() llm.Request {
	return llm.Request{
		Model: llm.Model{ID: "vision-test"},
		Input: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Take a screenshot."}},
			{ProviderID: "fc-1", Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: "screenshot", Arguments: `{}`}},
			{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{
				{Kind: llm.ToolResultText, Value: "Captured 1280x720"},
				{Kind: llm.ToolResultImage, Value: "data:image/png;base64,aGVsbG8="},
			}}},
			{ProviderID: "fc-2", Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-2", Name: "view_image", Arguments: `{"path":"a.png"}`}},
			{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call-2", Output: []llm.ToolResultOutput{
				{Kind: llm.ToolResultImage, Value: "https://example.com/first.png"},
				{Kind: llm.ToolResultImage, Value: "https://example.com/second.png"},
			}}},
		},
	}
}

func TestRequestBodySetsDetailOnEveryInputImage(t *testing.T) {
	body, err := requestBody(imageRequest(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var request any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	parts := inputImageParts(request)
	if len(parts) != 3 {
		t.Fatalf("found %d input_image parts, want 3: %s", len(parts), body)
	}
	for _, part := range parts {
		if part["detail"] != "auto" {
			t.Errorf("input_image part %v has detail %v, want \"auto\"", part["image_url"], part["detail"])
		}
	}
}

func TestAdapterSendsImagesThatStrictServersAccept(t *testing.T) {
	var accepted, rejected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		missing, err := missingImageDetail(body)
		if err != nil || missing {
			rejected.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(writer, `{"error":{"message":"1 validation error for ResponseInputImageParam\ndetail\n  Field required","type":"BadRequestError","code":"invalid_request"}}`)
			return
		}
		accepted.Add(1)
		writeStreamResponse(t, writer, `{"id":"resp-1","status":"completed","output":[],"usage":{}}`)
	}))
	defer server.Close()

	// The fake server must reject an image part without detail, or the adapter
	// check below would pass for the wrong reason.
	withoutDetail := `{"model":"vision-test","input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_image","image_url":"https://example.com/first.png"}]}]}`
	response, err := http.Post(server.URL, "application/json", bytes.NewBufferString(withoutDetail))
	if err != nil {
		t.Fatal(err)
	}
	message, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(message), "Field required") {
		t.Fatalf("fake server answered %d %s to an image without detail, want 400", response.StatusCode, message)
	}

	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL})
	got, err := adapter.Respond(t.Context(), imageRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if got.ID != "resp-1" || got.Stop != llm.StopComplete {
		t.Fatalf("response = %#v", got)
	}
	if accepted.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf("accepted = %d, rejected = %d, want 1 and 1", accepted.Load(), rejected.Load())
	}
}
