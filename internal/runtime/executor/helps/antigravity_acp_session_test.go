package helps

import (
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func sampleACPSessionDoc(convID string, extraContents ...string) *ojson.Doc {
	sysPrompt := "<user_information>\nThe USER's OS version is linux.\nApp Data Directory: /home/user/.gemini/antigravity-acp\nConversation ID: " + convID + "\nArtifact Directory Path: /home/user/.gemini/antigravity-acp/brain/" + convID + "\n</user_information>"
	doc := ojson.NewDocString(`{"request":{"contents":[{"role":"user","parts":[{"text":"<USER_REQUEST>Hello</USER_REQUEST>"}]}],"systemInstruction":{"parts":[{}]}}}`)
	_ = doc.Set("request.systemInstruction.parts.0.text", sysPrompt)
	for i, raw := range extraContents {
		_ = doc.SetRaw("request.contents."+strconv.Itoa(1+i), raw)
	}
	return doc
}

func TestACPSessionCorrelator_NextCallUsesStepFloorAndStaysMonotonic(t *testing.T) {
	c := newACPSessionCorrelator()

	// Fresh correlator starting mid-conversation at step 4 (minSeq = 5).
	_, seq1 := c.nextCall("conv-a", 5)
	if seq1 != 5 {
		t.Fatalf("first seq after cold start = %d, want 5", seq1)
	}

	// Retry at the same step still increments monotonically.
	_, seq2 := c.nextCall("conv-a", 5)
	if seq2 != 6 {
		t.Fatalf("retry seq = %d, want 6", seq2)
	}

	// Jumping ahead to step 9 (minSeq = 10) tracks last_step_index + 1.
	_, seq3 := c.nextCall("conv-a", 10)
	if seq3 != 10 {
		t.Fatalf("advanced seq = %d, want 10", seq3)
	}

	// Lower minSeq never moves backward.
	_, seq4 := c.nextCall("conv-a", 2)
	if seq4 != 11 {
		t.Fatalf("lower floor seq = %d, want 11", seq4)
	}
}

// TestApplyAntigravityACPRequestDoc_RequestIDStepFloorAcrossRestart verifies
// that the step-derived floor (last_step_index + 1) prevents requestId sequences
// from resetting to /1 after a restart, while in-memory retries within a process
// still advance monotonically.
func TestApplyAntigravityACPRequestDoc_RequestIDStepFloorAcrossRestart(t *testing.T) {
	origCorrelator := defaultACPSessionCorrelator
	defaultACPSessionCorrelator = newACPSessionCorrelator()
	t.Cleanup(func() {
		defaultACPSessionCorrelator = origCorrelator
	})

	convID := "12121212-1212-4121-8121-121212121212"
	account := &auth.Auth{
		ID:       "auth-restart-seq",
		Metadata: map[string]any{"email": "restart@example.com"},
	}
	opts := executor.Options{
		SourceFormat: sdktranslator.FormatGemini,
		Metadata:     map[string]any{executor.CallerScopeMetadataKey: "worker-restart-seq"},
	}

	// Turn 1: last_step_index=0 -> .../1
	doc1 := sampleACPSessionDoc(convID)
	meta1, err1 := ApplyAntigravityACPRequestDoc(doc1, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99")
	if err1 != nil || !meta1.Matched {
		t.Fatalf("turn 1 ApplyAntigravityACPRequestDoc = (%+v, %v), want matched nil error", meta1, err1)
	}
	trajID := doc1.Get("request.labels.trajectory_id").String()
	if got := doc1.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/1") {
		t.Fatalf("turn 1 requestId = %q, want suffix /%s/1", got, trajID)
	}

	// Turn 2: 1 model turn + 1 tool result => last_step_index=2 -> .../3
	modelCall1 := `{"role":"model","parts":[{"functionCall":{"id":"c1","name":"read_file","args":{}}}]}`
	toolResp1 := `{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"ok"}}}]}`
	doc2 := sampleACPSessionDoc(convID, modelCall1, toolResp1)
	if _, err2 := ApplyAntigravityACPRequestDoc(doc2, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); err2 != nil {
		t.Fatalf("turn 2 error: %v", err2)
	}
	if got := doc2.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/3") {
		t.Fatalf("turn 2 requestId = %q, want suffix /%s/3", got, trajID)
	}

	// Simulate proxy restart by replacing the in-memory correlator.
	defaultACPSessionCorrelator = newACPSessionCorrelator()

	// Turn 3 after restart: 2 model turns + 2 tool results => last_step_index=4 -> .../5 (not /1)
	modelCall2 := `{"role":"model","parts":[{"functionCall":{"id":"c2","name":"write_file","args":{}}}]}`
	toolResp2 := `{"role":"user","parts":[{"functionResponse":{"id":"c2","name":"write_file","response":{"output":"ok"}}}]}`
	doc3 := sampleACPSessionDoc(convID, modelCall1, toolResp1, modelCall2, toolResp2)
	if _, err3 := ApplyAntigravityACPRequestDoc(doc3, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); err3 != nil {
		t.Fatalf("turn 3 error: %v", err3)
	}
	if got := doc3.Get("request.labels.last_step_index").String(); got != "4" {
		t.Fatalf("turn 3 last_step_index = %q, want 4", got)
	}
	if got := doc3.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/5") {
		t.Fatalf("turn 3 requestId after restart = %q, want suffix /%s/5", got, trajID)
	}

	// Retry of Turn 3 at the same step still advances to .../6.
	doc3Retry := sampleACPSessionDoc(convID, modelCall1, toolResp1, modelCall2, toolResp2)
	if _, errRetry := ApplyAntigravityACPRequestDoc(doc3Retry, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); errRetry != nil {
		t.Fatalf("turn 3 retry error: %v", errRetry)
	}
	if got := doc3Retry.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/6") {
		t.Fatalf("turn 3 retry requestId = %q, want suffix /%s/6", got, trajID)
	}

	// Caller-supplied last_step_index higher than history is respected as the floor.
	docCustomStep := sampleACPSessionDoc(convID)
	_ = docCustomStep.Set("request.labels.last_step_index", "10")
	if _, errCustom := ApplyAntigravityACPRequestDoc(docCustomStep, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); errCustom != nil {
		t.Fatalf("custom step error: %v", errCustom)
	}
	if got := docCustomStep.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/11") {
		t.Fatalf("custom last_step_index requestId = %q, want suffix /%s/11", got, trajID)
	}

	// Caller-supplied requestId is preserved untouched and does not advance callSeq.
	docPresetID := sampleACPSessionDoc(convID)
	_ = docPresetID.Set("requestId", "agent/preset-id")
	if _, errPreset := ApplyAntigravityACPRequestDoc(docPresetID, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); errPreset != nil {
		t.Fatalf("preset requestId error: %v", errPreset)
	}
	if got := docPresetID.Get("requestId").String(); got != "agent/preset-id" {
		t.Fatalf("preset requestId = %q, want agent/preset-id", got)
	}
	docNext := sampleACPSessionDoc(convID)
	if _, errNext := ApplyAntigravityACPRequestDoc(docNext, opts, account, "proj-1", "gemini-4-hydrogen-high", "MODEL_PLACEHOLDER_M99"); errNext != nil {
		t.Fatalf("post-preset request error: %v", errNext)
	}
	if got := docNext.Get("requestId").String(); !strings.HasSuffix(got, "/"+trajID+"/12") {
		t.Fatalf("post-preset requestId = %q, want suffix /%s/12", got, trajID)
	}
}
