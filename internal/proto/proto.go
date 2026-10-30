// Package proto 是设备↔Mac 的 JSON 行协议（字段名与固件 1:1）。
//
// 下行（Mac→设备，CTRL 特征，一行 JSON ≤2048B，末尾不加 '\n'）：
//
//	transcript              text(≤127B/条)、final
//	agent.status            state(ready/thinking/running/error/done)、message
//	agent.approval_request  taskId、title、target、diffSummary、riskLevel(low/medium/high)
//	time.set                epoch（UTC 秒）
//
// 上行（设备→Mac，EVENT 特征，一行 JSON ≤512B，末尾带 '\n'）：
//
//	device.hello  proto
//	voice.start   audio（ima_adpcm / pcm）
//	voice.end     —
//	key.action    action（enter / clear）
//	agent.action  taskId、action（approve / reject / details）
//	status        drop（会话后补发的设备侧掉帧数）
//
// 契约来源：main/app_protocol.c、main/ble_audio.h。
package proto

import (
	"bytes"
	"encoding/json"
	"errors"
)

// 协议上限（固件常量，不可放宽）。
const (
	// MaxCtrlLine CTRL 写载荷上限（固件 APP_PROTO_RX_CAP）。
	MaxCtrlLine = 2048
	// MaxEventLine 事件行上限（固件 EVENT_LINE_MAX / APP_PROTO_TX_CAP）。
	MaxEventLine = 512
	// TranscriptTextMax transcript.text 单条字节上限（按 UTF-8 边界切）。
	TranscriptTextMax = 127
	// MaxReassembleLine 事件行重组缓冲上限，超过视为失步。
	MaxReassembleLine = 4 * 1024
)

// 上行事件种类。
const (
	EventDeviceHello = "device.hello"
	EventVoiceStart  = "voice.start"
	EventVoiceEnd    = "voice.end"
	EventKeyAction   = "key.action"
	EventAgentAction = "agent.action"
	EventStatus      = "status"
)

// 下行消息种类。
const (
	TypeTranscript      = "transcript"
	TypeAgentStatus     = "agent.status"
	TypeApprovalRequest = "agent.approval_request"
	TypeTimeSet         = "time.set"
)

// Agent 状态。会话结束必须发 done，否则设备停在 TRANSCRIBING 直到 STT 超时。
const (
	AgentReady    = "ready"
	AgentThinking = "thinking"
	AgentRunning  = "running"
	AgentError    = "error"
	AgentDone     = "done"
)

// 音频格式。
const (
	AudioADPCM = "ima_adpcm"
	AudioPCM   = "pcm"
)

// 审批风险等级。
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// 按键盘动作。
const (
	KeyEnter = "enter"
	KeyClear = "clear"
)

// 审批裁决。
const (
	ActionApprove = "approve"
	ActionReject  = "reject"
	ActionDetails = "details"
)

// ErrLineTooLong 单条下行超过 MaxCtrlLine。
var ErrLineTooLong = errors.New("proto: ctrl line exceeds 2048 bytes")

// Event 是解析后的上行事件。只有该事件类型相关的字段有意义。
type Event struct {
	Kind   string // 事件名（原样保留，未知类型也返回，由调用方记日志）
	Proto  int    // device.hello
	Audio  string // voice.start；未知值由调用方按 pcm 兜底
	Action string // key.action / agent.action
	TaskID string // agent.action
	Drop   int    // status
	Raw    string // 原始行（诊断用）
}

type wireEvent struct {
	Event  string `json:"event"`
	Proto  int    `json:"proto"`
	Audio  string `json:"audio"`
	Action string `json:"action"`
	TaskID string `json:"taskId"`
	Drop   int    `json:"drop"`
}

// ParseEvent 解析一条上行事件行（不含结尾 '\n'）。
func ParseEvent(line []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(line, &w); err != nil {
		return Event{}, err
	}
	if w.Event == "" {
		return Event{}, errors.New("proto: event line without \"event\" field")
	}
	ev := Event{
		Kind:   w.Event,
		Proto:  w.Proto,
		Audio:  w.Audio,
		Action: w.Action,
		TaskID: w.TaskID,
		Drop:   w.Drop,
		Raw:    string(line),
	}
	switch w.Event {
	case EventVoiceStart:
		if ev.Audio == "" {
			ev.Audio = AudioPCM // 固件约定：未知/缺省按 pcm 兜底
		}
	}
	return ev, nil
}

// AudioFormat 返回会话音频格式，未知值按 pcm 兜底。
func (e Event) AudioFormat() string {
	switch e.Audio {
	case AudioADPCM, AudioPCM:
		return e.Audio
	}
	return AudioPCM
}

type wireTranscript struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

type wireAgentStatus struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Message string `json:"message"`
}

type wireApproval struct {
	Type        string `json:"type"`
	TaskID      string `json:"taskId"`
	Title       string `json:"title"`
	Target      string `json:"target"`
	DiffSummary string `json:"diffSummary"`
	RiskLevel   string `json:"riskLevel"`
}

type wireTimeSet struct {
	Type  string `json:"type"`
	Epoch int64  `json:"epoch"`
}

// marshal 序列化并校验长度；不转义 HTML 字符（中文转写会因此膨胀）。
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if len(out) > MaxCtrlLine {
		return nil, ErrLineTooLong
	}
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp, nil
}

// Transcript 构造一条 transcript 下行（text 应已按 SplitTranscript 切好）。
func Transcript(text string, final bool) ([]byte, error) {
	return marshal(wireTranscript{Type: TypeTranscript, Text: text, Final: final})
}

// AgentStatus 构造 agent.status 下行。会话结束必须发 AgentDone。
func AgentStatus(state, message string) ([]byte, error) {
	return marshal(wireAgentStatus{Type: TypeAgentStatus, State: state, Message: message})
}

// ApprovalRequest 构造审批请求下行。riskLevel 非法时按 medium 兜底。
func ApprovalRequest(taskID, title, target, diffSummary, riskLevel string) ([]byte, error) {
	switch riskLevel {
	case RiskLow, RiskMedium, RiskHigh:
	default:
		riskLevel = RiskMedium
	}
	return marshal(wireApproval{
		Type:        TypeApprovalRequest,
		TaskID:      taskID,
		Title:       title,
		Target:      target,
		DiffSummary: diffSummary,
		RiskLevel:   riskLevel,
	})
}

// TimeSet 构造校时下行（UTC 秒）。
func TimeSet(epoch int64) ([]byte, error) {
	return marshal(wireTimeSet{Type: TypeTimeSet, Epoch: epoch})
}

// SplitTranscript 按字节上限切分转写文本，不切断 UTF-8 多字节字符。
// 单段超限拆两条；恰在上限内原样单条。空文本返回 nil。
func SplitTranscript(text string, maxBytes int) []string {
	if text == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = TranscriptTextMax
	}
	if len(text) <= maxBytes {
		return []string{text}
	}
	var (
		out      []string
		cur      []rune
		curBytes int
	)
	for _, r := range text {
		n := utf8Len(r)
		if curBytes+n > maxBytes && len(cur) > 0 {
			out = append(out, string(cur))
			cur, curBytes = cur[:0], 0
		}
		cur = append(cur, r)
		curBytes += n
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// TranscriptLines 把文本切成若干条 transcript 下行。
func TranscriptLines(text string, final bool) ([][]byte, error) {
	segs := SplitTranscript(text, TranscriptTextMax)
	lines := make([][]byte, 0, len(segs))
	for _, seg := range segs {
		line, err := Transcript(seg, final)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func utf8Len(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case r < 0x800:
		return 2
	case r < 0x10000:
		return 3
	default:
		return 4
	}
}
