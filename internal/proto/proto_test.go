package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseEvent(t *testing.T) {
	cases := []struct {
		line string
		want Event
	}{
		{`{"event":"device.hello","proto":1}`, Event{Kind: EventDeviceHello, Proto: 1}},
		{`{"event":"voice.start","audio":"ima_adpcm"}`, Event{Kind: EventVoiceStart, Audio: AudioADPCM}},
		{`{"event":"voice.end"}`, Event{Kind: EventVoiceEnd}},
		{`{"event":"key.action","action":"clear"}`, Event{Kind: EventKeyAction, Action: KeyClear}},
		{`{"event":"agent.action","taskId":"t-1","action":"approve"}`, Event{Kind: EventAgentAction, TaskID: "t-1", Action: ActionApprove}},
		{`{"event":"status","drop":3}`, Event{Kind: EventStatus, Drop: 3}},
	}
	for _, c := range cases {
		got, err := ParseEvent([]byte(c.line))
		if err != nil {
			t.Fatalf("%s: %v", c.line, err)
		}
		if got.Kind != c.want.Kind || got.Proto != c.want.Proto || got.Audio != c.want.Audio ||
			got.Action != c.want.Action || got.TaskID != c.want.TaskID || got.Drop != c.want.Drop {
			t.Fatalf("%s → %+v, want %+v", c.line, got, c.want)
		}
	}
}

func TestParseEventBadInput(t *testing.T) {
	for _, line := range []string{`not json`, `{}`, `{"proto":1}`} {
		if _, err := ParseEvent([]byte(line)); err == nil {
			t.Fatalf("%q 应当报错", line)
		}
	}
}

func TestAudioFormatFallback(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"event":"voice.start"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.AudioFormat(); got != AudioPCM {
		t.Fatalf("缺省 audio → %q, want pcm", got)
	}
	ev, _ = ParseEvent([]byte(`{"event":"voice.start","audio":"ulaw"}`))
	if got := ev.AudioFormat(); got != AudioPCM {
		t.Fatalf("未知 audio → %q, want pcm（固件约定未知值按 pcm 兜底）", got)
	}
}

func TestSplitTranscriptUTF8(t *testing.T) {
	// ASCII：按字节切，段长 ≤ 127
	ascii := strings.Repeat("a", 200)
	segs := SplitTranscript(ascii, TranscriptTextMax)
	if len(segs) != 2 || len(segs[0]) > TranscriptTextMax || len(segs[1]) > TranscriptTextMax {
		t.Fatalf("ASCII 切分错误: %d 段，长度 %d/%d", len(segs), len(segs[0]), len(segs[1]))
	}
	if segs[0]+segs[1] != ascii {
		t.Fatal("切分后拼接必须还原原文")
	}

	// 中文 3B/字：不得切断多字节字符
	cjk := strings.Repeat("字", 100)
	segs = SplitTranscript(cjk, TranscriptTextMax)
	var rebuilt []byte
	for _, s := range segs {
		if len(s) > TranscriptTextMax {
			t.Fatalf("段长 %d > %d", len(s), TranscriptTextMax)
		}
		for _, r := range s {
			if r == '\uFFFD' {
				t.Fatal("切分切断了 UTF-8 字符")
			}
		}
		rebuilt = append(rebuilt, s...)
	}
	if string(rebuilt) != cjk {
		t.Fatal("切分后拼接必须还原原文")
	}

	if got := SplitTranscript("", TranscriptTextMax); got != nil {
		t.Fatalf("空文本应返回 nil，得到 %v", got)
	}
}

func TestTranscriptLine(t *testing.T) {
	line, err := Transcript("你好 <世界>", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(string(line), "\n") {
		t.Fatal("CTRL 行末尾不加 '\\n'")
	}
	if !strings.Contains(string(line), "<世界>") {
		t.Fatalf("不应转义 HTML 字符: %s", line)
	}
	var back wireTranscript
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != TypeTranscript || back.Text != "你好 <世界>" || !back.Final {
		t.Fatalf("回环解析错误: %+v", back)
	}
	lines, err := TranscriptLines(strings.Repeat("字", 100), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 3 {
		t.Fatalf("100 个汉字（300B）应切成 ≥3 条，得到 %d", len(lines))
	}
}

func TestAgentStatusAndTimeSet(t *testing.T) {
	line, err := AgentStatus(AgentDone, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"type":"agent.status","state":"done","message":""}` {
		t.Fatalf("done 行格式错误: %s", line)
	}
	line, err = TimeSet(1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"type":"time.set","epoch":1700000000}` {
		t.Fatalf("校时行格式错误: %s", line)
	}
}

func TestApprovalRequest(t *testing.T) {
	line, err := ApprovalRequest("t-1", "Deploy", "api.example.com", "+12 -3", "bogus")
	if err != nil {
		t.Fatal(err)
	}
	var back wireApproval
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.RiskLevel != RiskMedium {
		t.Fatalf("非法 riskLevel 应兜底 medium，得到 %q", back.RiskLevel)
	}
	if back.TaskID != "t-1" || back.DiffSummary != "+12 -3" {
		t.Fatalf("字段名不符: %s", line)
	}

	// 超过 2048B 必须拒绝，而不是发出去被设备丢弃。
	if _, err := ApprovalRequest("t", "t", "t", strings.Repeat("x", MaxCtrlLine), RiskHigh); err == nil {
		t.Fatal("超长行应当报错")
	}
}

func TestLineReassembler(t *testing.T) {
	r := NewLineReassembler(0)
	if lines := r.Feed([]byte(`{"event":"voice.`)); len(lines) != 0 {
		t.Fatalf("半行不应产出: %d", len(lines))
	}
	lines := r.Feed([]byte("start\"}\n{\"event\":\"voice.end\"}\n{\"event\":\"st"))
	if len(lines) != 2 {
		t.Fatalf("行数 %d，want 2", len(lines))
	}
	if string(lines[0]) != `{"event":"voice.start"}` || string(lines[1]) != `{"event":"voice.end"}` {
		t.Fatalf("切行错误: %q, %q", lines[0], lines[1])
	}
	if r.Pending() != len(`{"event":"st`) {
		t.Fatalf("残留 %d 字节", r.Pending())
	}
	// 失步：未成行缓冲超过上限即清空。
	r.Feed(make([]byte, MaxReassembleLine+1))
	if r.Pending() != 0 {
		t.Fatalf("超过上限应清空，残留 %d", r.Pending())
	}
}
