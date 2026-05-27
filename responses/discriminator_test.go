package responses_test

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

// Reproduces the round-trip from Issue #483: marshal a ResponseOutputMessageParam
// through ResponseInputItemUnionParam, then unmarshal. Prior to the fix, the
// "type":"message" discriminator collision caused the item to land in OfMessage
// (EasyInputMessageParam), losing id/status and dropping output_text content.
func TestResponseInputItemUnionParam_OutputMessageRoundTrip(t *testing.T) {
	original := responses.ResponseInputItemParamOfOutputMessage(
		[]responses.ResponseOutputMessageContentUnionParam{
			{OfOutputText: &responses.ResponseOutputTextParam{Text: "Hi! How can I help you today?"}},
		},
		"msg_123",
		responses.ResponseOutputMessageStatusCompleted,
	)

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored responses.ResponseInputItemUnionParam
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.OfOutputMessage == nil {
		t.Fatalf("expected OfOutputMessage to be populated, got OfMessage=%+v OfInputMessage=%+v",
			restored.OfMessage, restored.OfInputMessage)
	}
	if restored.OfMessage != nil {
		t.Errorf("OfMessage should be nil after round-trip, got %+v", restored.OfMessage)
	}
	if got := restored.OfOutputMessage.ID; got != "msg_123" {
		t.Errorf("ID: got %q, want %q", got, "msg_123")
	}
	if got := restored.OfOutputMessage.Status; got != responses.ResponseOutputMessageStatusCompleted {
		t.Errorf("Status: got %q, want %q", got, responses.ResponseOutputMessageStatusCompleted)
	}
	if n := len(restored.OfOutputMessage.Content); n != 1 {
		t.Fatalf("Content length: got %d, want 1", n)
	}
	if got := restored.OfOutputMessage.Content[0].OfOutputText; got == nil || got.Text != "Hi! How can I help you today?" {
		t.Errorf("Content[0].OfOutputText: got %+v", got)
	}
}
