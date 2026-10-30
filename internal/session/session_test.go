package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sotto/internal/asr"
	"sotto/internal/audio"
	"sotto/internal/proto"
)

// ---- 测试替身 ----

type fakeASR struct {
	connectErr error
	results    chan asr.Result
	// sendBlock 非 nil 时 SendFrame 阻塞，用来确定性地造出"ASR 慢、队列满"。
	sendBlock chan struct{}

	mu      sync.Mutex
	sent    [][]byte
	endSent bool
}

func newFakeASR() *fakeASR { return &fakeASR{results: make(chan asr.Result, 8)} }

func (f *fakeASR) Connect(ctx context.Context) error {
	if f.connectErr != nil {
		return f.connectErr
	}
	return nil
}

func (f *fakeASR) SendFrame(ctx context.Context, pcm []byte) error {
	if f.sendBlock != nil {
		<-f.sendBlock
	}
	f.mu.Lock()
	cp := append([]byte(nil), pcm...)
	f.sent = append(f.sent, cp)
	f.mu.Unlock()
	return nil
}

func (f *fakeASR) SendEnd(ctx context.Context) error {
	f.mu.Lock()
	f.endSent = true
	f.mu.Unlock()
	return nil
}

func (f *fakeASR) Results() <-chan asr.Result { return f.results }

func (f *fakeASR) Close() error { return nil }

func (f *fakeASR) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

type fakeCtrl struct {
	mu    sync.Mutex
	lines [][]byte
}

func (c *fakeCtrl) WriteCTRL(line []byte) error {
	c.mu.Lock()
	c.lines = append(c.lines, append([]byte(nil), line...))
	c.mu.Unlock()
	return nil
}

func (c *fakeCtrl) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func (c *fakeCtrl) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.lines...)
}

// ---- 夹具 ----

// testBlock 造一块可解码的 ADPCM（804B）。
func testBlock(seed int) []byte {
	pcm := make([]int16, audio.ADPCMBlockSamples)
	for i := range pcm {
		pcm[i] = int16(seed*97 + (i%64)*11 - 320)
	}
	return audio.EncodeBlock(&audio.State{}, pcm)
}

// chunkify 按固件规则切分（body = mtu-3-2）。
func chunkify(seq uint8, block []byte, body int) [][]byte {
	var out [][]byte
	for off, idx := 0, 0; off < len(block); idx++ {
		n := body
		if len(block)-off < n {
			n = len(block) - off
		}
		last := off+n >= len(block)
		hdr := []byte{seq, byte(idx & audio.ChunkIndexMask)}
		if last {
			hdr[1] |= audio.ChunkLastFlag
		}
		out = append(out, append(hdr, block[off:off+n]...))
		off += n
	}
	return out
}

type harness struct {
	mgr      *Manager
	ctrl     *fakeCtrl
	fake     *fakeASR
	stats    chan Stats
	partials chan string
	finals   chan string
	frames   chan []byte

	mu       sync.Mutex
	injected []string
	logs     []string
}

func newHarness(t *testing.T, cfg Config, connectErr error) *harness {
	t.Helper()
	h := &harness{
		ctrl:     &fakeCtrl{},
		fake:     newFakeASR(),
		stats:    make(chan Stats, 4),
		partials: make(chan string, 8),
		finals:   make(chan string, 4),
		frames:   make(chan []byte, 64),
	}
	h.fake.connectErr = connectErr
	if cfg.WaitFinal == 0 {
		cfg.WaitFinal = 200 * time.Millisecond
	}
	if cfg.Inject == nil {
		cfg.Inject = func(text string) (string, error) {
			h.mu.Lock()
			h.injected = append(h.injected, text)
			h.mu.Unlock()
			return "unicode", nil
		}
	}
	h.mgr = NewManager(h.ctrl, func() Config { return cfg }, Handler{
		OnPartial: func(text string) { h.partials <- text },
		OnFinal:   func(text string) { h.finals <- text },
		OnFrame:   func(pcm []byte) { h.frames <- append([]byte(nil), pcm...) },
		OnStats:   func(s Stats) { h.stats <- s },
		OnLog: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, format)
			h.mu.Unlock()
		},
	})
	h.mgr.newASR = func(asr.Config) asrClient { return h.fake }
	return h
}

func (h *harness) injections() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.injected...)
}

func (h *harness) waitStats(t *testing.T) Stats {
	t.Helper()
	select {
	case s := <-h.stats:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("等不到会话统计（收尾没跑完？）")
		return Stats{}
	}
}

// ---- 用例 ----

// 定稿 → 注入一次 → 回 done → 统计。候选字要逐个回调。
func TestFinalInjectsOnceAndSendsDone(t *testing.T) {
	h := newHarness(t, Config{}, nil)
	defer close(h.fake.results)

	h.mgr.OnVoiceStart(proto.AudioADPCM)
	h.fake.results <- asr.Result{Text: "你好"}
	select {
	case got := <-h.partials:
		if got != "你好" {
			t.Fatalf("候选字 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等不到候选字")
	}

	h.fake.results <- asr.Result{Text: "你好，世界。", Final: true}
	h.mgr.OnVoiceEnd()
	stats := h.waitStats(t)

	if got := h.injections(); len(got) != 1 || got[0] != "你好，世界。" {
		t.Fatalf("注入次数/文本不对: %v", got)
	}
	if !stats.Injected || stats.FinalText != "你好，世界。" {
		t.Fatalf("统计不对: %+v", stats)
	}
	if h.ctrl.count() != 1 {
		t.Fatalf("done 应恰好发一次，实际 %d", h.ctrl.count())
	}
	if string(h.ctrl.all()[0]) == "" || !containsDone(h.ctrl.all()[0]) {
		t.Fatalf("下行不是 agent.status done: %s", h.ctrl.all()[0])
	}
	if got := <-h.finals; got != "你好，世界。" {
		t.Fatalf("OnFinal 文本 %q", got)
	}
}

// 没等到定稿：仍然必须回 done（否则设备卡在 TRANSCRIBING），且不注入。
func TestTimeoutStillSendsDone(t *testing.T) {
	h := newHarness(t, Config{WaitFinal: 80 * time.Millisecond}, nil)
	defer close(h.fake.results)

	h.mgr.OnVoiceStart(proto.AudioADPCM)
	h.mgr.OnVoiceEnd()
	stats := h.waitStats(t)

	if h.ctrl.count() != 1 {
		t.Fatalf("超时也必须发 done，实际 %d 次", h.ctrl.count())
	}
	if len(h.injections()) != 0 {
		t.Fatalf("超时不应注入: %v", h.injections())
	}
	if stats.FinalText != "" {
		t.Fatalf("超时不该有定稿文本: %+v", stats)
	}
}

// ASR 连接失败：仍要回 done，且不注入。
func TestConnectFailureStillSendsDone(t *testing.T) {
	h := newHarness(t, Config{WaitFinal: 80 * time.Millisecond}, errFake{})
	h.mgr.OnVoiceStart(proto.AudioADPCM)
	h.mgr.OnVoiceEnd()
	stats := h.waitStats(t)

	if h.ctrl.count() != 1 {
		t.Fatalf("连接失败也必须发 done，实际 %d", h.ctrl.count())
	}
	if !stats.ASRFailed {
		t.Fatalf("统计应标记 ASR 失败: %+v", stats)
	}
	if len(h.injections()) != 0 {
		t.Fatalf("连接失败不应注入")
	}
}

type errFake struct{}

func (errFake) Error() string { return "connect failed" }

// 音频分片被解码成 3200B PCM 并喂给 ASR。
func TestAudioDecodedAndFed(t *testing.T) {
	h := newHarness(t, Config{}, nil)
	defer close(h.fake.results)

	block := testBlock(1)
	h.mgr.OnVoiceStart(proto.AudioADPCM)
	// 一块 804B 切成两片，只有最后一片到齐才解出整块 → 全部喂完再等帧。
	for _, chunk := range chunkify(0, block, 512) {
		h.mgr.OnAudio(chunk)
	}
	select {
	case frame := <-h.frames:
		if len(frame) != PCMFrameBytes {
			t.Fatalf("PCM 帧长 %d，want %d", len(frame), PCMFrameBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等不到解码出的 PCM 帧")
	}
	h.mgr.OnVoiceEnd()
	stats := h.waitStats(t)

	if stats.Received != 1 {
		t.Fatalf("实收帧 %d，want 1", stats.Received)
	}
	fed := h.fake.frames()
	if len(fed) != 1 {
		t.Fatalf("ASR 收到 %d 帧，want 1", len(fed))
	}
	if len(fed[0]) != PCMFrameBytes {
		t.Fatalf("喂给 ASR 的帧长 %d，want %d", len(fed[0]), PCMFrameBytes)
	}
}

// 空闲期（没有会话）的音频不产帧。
func TestAudioIgnoredWhenIdle(t *testing.T) {
	h := newHarness(t, Config{}, nil)
	block := testBlock(2)
	h.mgr.OnAudio(chunkify(0, block, 512)[0])
	select {
	case <-h.frames:
		t.Fatal("空闲期不应产帧")
	case <-time.After(100 * time.Millisecond):
	}
}

// 队列满丢帧：不阻塞音频路径，且计数。
func TestQueueDropsWhenFull(t *testing.T) {
	h := newHarness(t, Config{QueueDepth: 1}, nil)
	defer close(h.fake.results)
	// ASR 卡住不消费，队列只有 1 格 → 后到的帧必然被丢。
	h.fake.sendBlock = make(chan struct{})

	block := testBlock(3)
	h.mgr.OnVoiceStart(proto.AudioADPCM)
	for seq := 0; seq < 5; seq++ {
		for _, chunk := range chunkify(uint8(seq), block, 512) {
			h.mgr.OnAudio(chunk) // 必须在 ASR 卡住时也不阻塞
		}
	}
	close(h.fake.sendBlock)
	h.mgr.OnVoiceEnd()
	stats := h.waitStats(t)

	if stats.Received != 5 {
		t.Fatalf("解码帧数 %d，want 5", stats.Received)
	}
	if stats.QueueDropped == 0 {
		t.Fatalf("应当有丢帧计数: %+v", stats)
	}
}

// 设备 status{drop} 会补记到最近一次会话统计。
func TestStatusUpdatesLastStats(t *testing.T) {
	h := newHarness(t, Config{WaitFinal: 50 * time.Millisecond}, nil)
	defer close(h.fake.results)

	h.mgr.OnVoiceStart(proto.AudioADPCM)
	h.mgr.OnVoiceEnd()
	h.waitStats(t)

	h.mgr.OnStatus(7)
	last, ok := h.mgr.Last()
	if !ok || !last.HasDeviceDrop || last.DeviceDrop != 7 {
		t.Fatalf("设备掉帧未记账: %+v ok=%v", last, ok)
	}
}

// 断连中断：不等定稿，直接收尾（仍尽力发 done）。
func TestAbortFinishes(t *testing.T) {
	h := newHarness(t, Config{WaitFinal: 5 * time.Second}, nil)
	defer close(h.fake.results)

	h.mgr.OnVoiceStart(proto.AudioADPCM)
	done := make(chan struct{})
	go func() {
		h.mgr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 应立刻收束，不该等定稿超时")
	}
	stats := h.waitStats(t)
	if h.ctrl.count() != 1 {
		t.Fatalf("中断也应发 done，实际 %d", h.ctrl.count())
	}
	if stats.FinalText != "" {
		t.Fatalf("没有定稿: %+v", stats)
	}
}

// 重复 voice.start 先收束旧会话。
func TestDuplicateVoiceStart(t *testing.T) {
	h := newHarness(t, Config{WaitFinal: 50 * time.Millisecond}, nil)
	defer close(h.fake.results)

	h.mgr.OnVoiceStart(proto.AudioADPCM)
	h.mgr.OnVoiceStart(proto.AudioADPCM)
	if stats := h.waitStats(t); stats.FinalText != "" {
		t.Fatalf("旧会话应被收束: %+v", stats)
	}
	h.mgr.Close()
}

func containsDone(line []byte) bool {
	return strings.Contains(string(line), `"agent.status"`) && strings.Contains(string(line), `"done"`)
}
