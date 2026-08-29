package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

const testImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateMessageImageMatrix(t *testing.T) {
	validInline := &ImageContent{MediaType: "image/png", Data: []byte{1, 2, 3}}
	validRef := &ImageContent{MediaType: "image/webp", Ref: "opaque/ref", Digest: testImageDigest, SizeBytes: 3}
	tests := []struct {
		name    string
		role    Role
		part    ContentPart
		wantErr bool
	}{
		{name: "inline", role: RoleUser, part: ContentPart{Type: PartImage, Image: validInline}},
		{name: "reference", role: RoleUser, part: ContentPart{Type: PartImage, Image: validRef}},
		{name: "assistant image", role: RoleAssistant, part: ContentPart{Type: PartImage, Image: validInline}, wantErr: true},
		{name: "missing image", role: RoleUser, part: ContentPart{Type: PartImage}, wantErr: true},
		{name: "text payload", role: RoleUser, part: ContentPart{Type: PartImage, Text: "x", Image: validInline}, wantErr: true},
		{name: "tool call payload", role: RoleUser, part: ContentPart{Type: PartImage, Image: validInline, ToolCall: &ToolCall{ID: "c", Name: "x"}}, wantErr: true},
		{name: "tool result payload", role: RoleUser, part: ContentPart{Type: PartImage, Image: validInline, ToolResult: &ToolResult{ToolCallID: "c", Name: "x"}}, wantErr: true},
		{name: "empty source", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/png"}}, wantErr: true},
		{name: "both sources", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{1}, Ref: "ref"}}, wantErr: true},
		{name: "blank reference", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Ref: " \t ", Digest: testImageDigest, SizeBytes: 1}}, wantErr: true},
		{name: "inline digest", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{1}, Digest: testImageDigest}}, wantErr: true},
		{name: "inline size", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{1}, SizeBytes: 1}}, wantErr: true},
		{name: "missing MIME", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{Data: []byte{1}}}, wantErr: true},
		{name: "noncanonical MIME", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "IMAGE/PNG", Data: []byte{1}}}, wantErr: true},
		{name: "gif", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/gif", Data: []byte{1}}}, wantErr: true},
		{name: "reference missing digest", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/jpeg", Ref: "ref", SizeBytes: 1}}, wantErr: true},
		{name: "reference uppercase digest", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/jpeg", Ref: "ref", Digest: "sha256:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789", SizeBytes: 1}}, wantErr: true},
		{name: "reference bare digest", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/jpeg", Ref: "ref", Digest: testImageDigest[7:], SizeBytes: 1}}, wantErr: true},
		{name: "reference zero size", role: RoleUser, part: ContentPart{Type: PartImage, Image: &ImageContent{MediaType: "image/jpeg", Ref: "ref", Digest: testImageDigest}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMessage(Message{Role: tt.role, Parts: []ContentPart{tt.part}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestImageTextOrderJSONRoundTripAndDigest(t *testing.T) {
	message := Message{Role: RoleUser, Parts: []ContentPart{
		{Type: PartText, Text: "before"},
		{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{0, 1, 2, 255}}},
		{Type: PartText, Text: "after"},
	}}
	if err := ValidateMessage(message); err != nil {
		t.Fatalf("ValidateMessage() error = %v", err)
	}
	refMessage := Message{Role: RoleUser, Parts: []ContentPart{{Type: PartImage, Image: &ImageContent{
		MediaType: "image/webp", Ref: "opaque/ref", Digest: testImageDigest, SizeBytes: 4,
	}}}}
	for _, original := range []Message{message, refMessage} {
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Message
		if err := unmarshalJSONStrict(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, original) {
			t.Fatalf("round trip = %#v, want %#v", decoded, original)
		}
	}

	variants := []Message{
		message,
		{Role: RoleUser, Parts: []ContentPart{{Type: PartText, Text: "before"}, {Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{0, 1, 3, 255}}}, {Type: PartText, Text: "after"}}},
		{Role: RoleUser, Parts: []ContentPart{{Type: PartText, Text: "before"}, {Type: PartImage, Image: &ImageContent{MediaType: "image/png", Ref: "opaque/ref", Digest: testImageDigest, SizeBytes: 4}}, {Type: PartText, Text: "after"}}},
		{Role: RoleUser, Parts: []ContentPart{{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{0, 1, 2, 255}}}, {Type: PartText, Text: "before"}, {Type: PartText, Text: "after"}}},
	}
	seen := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		digest, err := CanonicalDigest(variant)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := seen[digest]; exists {
			t.Fatalf("digest did not change for %#v", variant)
		}
		seen[digest] = struct{}{}
	}
}

// TestCloneMessagesDeepCopiesImageBytes covers the property the deleted
// MemoryStore used to demonstrate: image bytes crossing the runtime boundary are
// copied, so a caller that mutates what it received cannot reach back into
// state the runtime still holds.
func TestCloneMessagesDeepCopiesImageBytes(t *testing.T) {
	data := []byte{1, 2, 3}
	original := []Message{{Role: RoleUser, Parts: []ContentPart{
		{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: data}},
	}}}

	cloned := cloneMessages(original)
	data[0] = 9
	if got := cloned[0].Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("clone aliased the caller's bytes: %v", got)
	}
	cloned[0].Parts[0].Image.Data[1] = 9
	if got := original[0].Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{9, 2, 3}) {
		t.Fatalf("mutating a clone reached the original: %v", got)
	}
}

func TestRunSnapshotImageRoundTripAndCloneDoNotAlias(t *testing.T) {
	data := []byte{1, 2, 3}
	snapshot := testRunSnapshot()
	snapshot.Checkpoint.History = []Message{{Role: RoleUser, Parts: []ContentPart{{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: data}}}}}
	encoded, err := MarshalRunSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRunSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Checkpoint.History, snapshot.Checkpoint.History) {
		t.Fatalf("snapshot image round trip = %#v", decoded.Checkpoint.History)
	}
	cloned := snapshot.Clone()
	data[0] = 9
	cloned.Checkpoint.History[0].Parts[0].Image.Data[1] = 9
	if got := snapshot.Checkpoint.History[0].Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{9, 2, 3}) {
		t.Fatalf("snapshot clone image aliased: %v", got)
	}
}

type imageCapabilityModel struct {
	caps  Capabilities
	calls int
	req   *GenerateRequest
}

func (m *imageCapabilityModel) Name() string               { return "image-model" }
func (m *imageCapabilityModel) Capabilities() Capabilities { return m.caps }
func (m *imageCapabilityModel) Stream(_ context.Context, req *GenerateRequest) (<-chan StreamChunk, error) {
	m.calls++
	m.req = req
	response := NewAssistantMessage("ok")
	channel := make(chan StreamChunk, 2)
	channel <- StreamChunk{Type: ChunkText, TextDelta: "ok"}
	channel <- StreamChunk{Type: ChunkFinish, Response: &Response{Message: response, FinishReason: FinishStop}}
	close(channel)
	return channel, nil
}

func TestAgentRejectsUnsupportedImageBeforeModelEffect(t *testing.T) {
	message := Message{Role: RoleUser, Parts: []ContentPart{{Type: PartImage, Image: &ImageContent{MediaType: "image/png", Data: []byte{1}}}}}
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "supported"}[supported], func(t *testing.T) {
			model := &imageCapabilityModel{caps: Capabilities{ImageInput: supported}}
			runner := newTestAgent(t, Config{Key: "image", ModelName: "image-model", MaxSteps: 4}, model)
			_, err := runner.Run(context.Background(), RunRequest{Messages: []Message{message}})
			if supported && err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !supported && (err == nil || model.calls != 0) {
				t.Fatalf("unsupported Run() error = %v, model calls = %d", err, model.calls)
			}
			if supported && model.calls != 1 {
				t.Fatalf("supported model calls = %d", model.calls)
			}
		})
	}
}
