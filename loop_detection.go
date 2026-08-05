package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

// 死循环检测默认参数。服务端单轮比 CLI 会话短一个量级，
// 故不照抄 crush 的 10/5（那套在 max_steps=8 时窗口永远攒不满、检测直接失效）。
const (
	// DefaultLoopDetectWindow 滑动窗口大小。
	DefaultLoopDetectWindow = 4
	// DefaultLoopDetectThreshold 同签名重复阈值：出现次数超过此值即判定卡死（即第 3 次命中）。
	DefaultLoopDetectThreshold = 2
)

// LoopDetector 用「工具交互签名 + 滑动窗口计数」检测模型反复用同样参数调同一个工具。
// 光靠 StepCountIs 不够：模型可能把步数烧完却毫无进展。
type LoopDetector struct {
	window    int
	threshold int
}

// NewLoopDetector 构造检测器。window/threshold <= 0 时取默认值。
func NewLoopDetector(window, threshold int) *LoopDetector {
	if window <= 0 {
		window = DefaultLoopDetectWindow
	}
	if threshold <= 0 {
		threshold = DefaultLoopDetectThreshold
	}
	return &LoopDetector{window: window, threshold: threshold}
}

// Window 返回窗口大小。
func (d *LoopDetector) Window() int { return d.window }

// Threshold 返回重复阈值。
func (d *LoopDetector) Threshold() int { return d.threshold }

// Validate 校验窗口不超过 max_steps。窗口比 max_steps 大意味着检测永远不会生效，
// 这种静默失效必须在装配期暴露，不允许带到线上。
func (d *LoopDetector) Validate(maxSteps int) error {
	if d.window > maxSteps {
		return fmt.Errorf("%w: loop_detect_window(%d) 不能大于 max_steps(%d)",
			ErrAgentConfigInvalid, d.window, maxSteps)
	}
	if d.threshold >= d.window {
		return fmt.Errorf("%w: loop_detect_threshold(%d) 必须小于 loop_detect_window(%d)，否则窗口内凑不出足够重复次数",
			ErrAgentConfigInvalid, d.threshold, d.window)
	}
	return nil
}

// Detect 判定最近的步骤里是否出现死循环，命中则返回签名与重复次数。
// 与 crush 不同：不做 len(steps) < window 的提前返回 —— 我们的 max_steps 只有 8，
// 窗口没攒满就返回 false 会让检测在实际步数范围内完全失效。
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

// stepSignature 计算一步内全部工具交互的有序聚合签名。
// 签名 = SHA256((toolName ‖ input ‖ output)... )，调用与结果按 tool_call_id 配对 ——
// 只看调用不看结果会把「同样参数但结果在变」的正常轮询误判成死循环。
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
	if err := jsoncodec.Unmarshal([]byte(input), &value); err != nil {
		return input
	}
	data, err := jsoncodec.Marshal(value)
	if err != nil {
		return input
	}
	return string(data)
}

func appendSignatureField(payload []byte, value string) []byte {
	payload = append(payload, value...)
	return append(payload, 0)
}
