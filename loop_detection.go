package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Default loop detection parameters. A server-side run is an order of
// magnitude shorter than a CLI session, so larger window/threshold pairs would
// never fill the window at small max_steps and detection would silently fail.
const (
	// DefaultLoopDetectWindow is the sliding window size.
	DefaultLoopDetectWindow = 4
	// DefaultLoopDetectThreshold is the repeat threshold for one signature:
	// exceeding it (the third hit) marks the run as stuck.
	DefaultLoopDetectThreshold = 2
)

// LoopDetector detects the model repeatedly calling the same tool with the
// same input, using tool interaction signatures counted over a sliding window.
// StepCountIs alone is not enough: the model can burn all steps without
// making progress.
type LoopDetector struct {
	window    int
	threshold int
}

// NewLoopDetector builds a detector. window/threshold <= 0 use the defaults.
func NewLoopDetector(window, threshold int) *LoopDetector {
	if window <= 0 {
		window = DefaultLoopDetectWindow
	}
	if threshold <= 0 {
		threshold = DefaultLoopDetectThreshold
	}
	return &LoopDetector{window: window, threshold: threshold}
}

// Window returns the window size.
func (d *LoopDetector) Window() int { return d.window }

// Threshold returns the repeat threshold.
func (d *LoopDetector) Threshold() int { return d.threshold }

// Validate ensures the window does not exceed max_steps. A window larger than
// max_steps means detection can never trigger; that silent failure must be
// exposed at assembly time rather than shipped to production.
func (d *LoopDetector) Validate(maxSteps int) error {
	if d.window > maxSteps {
		return fmt.Errorf("%w: loop_detect_window(%d) cannot exceed max_steps(%d)",
			ErrAgentConfigInvalid, d.window, maxSteps)
	}
	if d.threshold >= d.window {
		return fmt.Errorf("%w: loop_detect_threshold(%d) must be less than loop_detect_window(%d); otherwise the window can never accumulate enough repeats",
			ErrAgentConfigInvalid, d.threshold, d.window)
	}
	return nil
}

// Detect reports whether recent steps form a loop, returning the signature and
// repeat count on a hit. There is deliberately no early return for
// len(steps) < window: with a small max_steps, returning false before the
// window fills would disable detection across the practical step range.
func (d *LoopDetector) Detect(steps []StepResult) (sig string, count int, detected bool) {
	start := 0
	if len(steps) > d.window {
		start = len(steps) - d.window
	}
	counts := make(map[string]int)
	for _, s := range steps[start:] {
		sig := stepSignature(s)
		if sig == "" {
			continue
		}
		counts[sig]++
		if counts[sig] > d.threshold {
			return sig, counts[sig], true
		}
	}
	return "", 0, false
}

// stepSignature computes the ordered aggregate signature of all tool
// interactions in one step. Signature = SHA256((toolName ‖ input ‖ output)...),
// pairing calls with results by tool_call_id. Looking at calls without results
// would misclassify legitimate polling (same input, changing output) as a loop.
func stepSignature(s StepResult) string {
	if len(s.ToolCalls) == 0 {
		return ""
	}
	outputs := make(map[string]string, len(s.ToolResults))
	for _, r := range s.ToolResults {
		outputs[r.ToolCallID] = r.Content
	}
	var payload []byte
	for _, c := range s.ToolCalls {
		payload = appendSignatureField(payload, c.Name)
		payload = appendSignatureField(payload, canonicalSignatureInput(c.Input))
		payload = appendSignatureField(payload, outputs[c.ID])
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func canonicalSignatureInput(input string) string {
	var value any
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		return input
	}
	data, err := json.Marshal(value)
	if err != nil {
		return input
	}
	return string(data)
}

func appendSignatureField(payload []byte, value string) []byte {
	payload = append(payload, value...)
	return append(payload, 0)
}
