// Package session 管理一次 voice.start..voice.end 的语音会话：
// 音频分片 → 合并通知拆分 → 块重组 → IMA ADPCM 解码 → 3200B PCM → 火山流式 ASR
// → 候选字回调 / 定稿注入，并保证收尾一定给设备发 agent.status done。
//
// 为什么要有这一层：官方实现里 80% 的坑都在会话边界上——
//   - 收尾不发 done，设备会一直停在 TRANSCRIBING 直到自己的 STT 超时（"说完卡住"）；
//   - 会话切换不复位块状态，新会话头几块会被当成上一会话的迟到片整块丢掉；
//   - ASR 慢时不设上界，延迟越滚越大（宁可丢帧）。
//
// 所以四条收尾路径（定稿到达 / ASR 连接失败 / 等定稿超时 / 断连中断）都必须走
// 同一个 finish()，靠 sync.Once 保证 done 与注入各只发生一次。
package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sotto/internal/asr"
	"sotto/internal/audio"
	"sotto/internal/proto"
)

// PCMFrameBytes 一帧解码后的 PCM 字节数（100ms @ 16kHz/16bit/单声道）。
const PCMFrameBytes = audio.PCMFrameBytes

// 默认参数。
const (
	defaultWaitFinal  = 5 * time.Second
	defaultQueueDepth = 20 // ≈2s 抖动上限
	connectTimeout    = 10 * time.Second
	sendTimeout       = 5 * time.Second
)

// CtrlWriter 是下行通道（BLE 的 CTRL 特征）。
type CtrlWriter interface {
	WriteCTRL(line []byte) error
}

// Injector 把定稿文本注入焦点输入框，返回实际走通的通道名（"unicode" / "clipboard"）。
type Injector func(text string) (string, error)

// Config 是一次会话的参数。由调用方在 voice.start 时提供（用户可以中途改 Key）。
type Config struct {
	ASR        asr.Config
	Inject     Injector      // nil 表示不注入（CLI 调试）
	WaitFinal  time.Duration // voice.end 后等定稿的上限
	QueueDepth int           // 音频队列帧数上限
	SkipDone   bool          // 收尾不回 agent.status done（仅用于复现设备卡住）
}

// Handler 是 UI 侧回调。全部在非主线程调用，实现里自己切线程。
type Handler struct {
	// OnPartial 收到候选字（全量累计文本）。
	OnPartial func(text string)
	// OnFinal 会话结束（注入之后），text 可能为空（失败/超时）。
	OnFinal func(text string)
	// OnFrame 收到一帧解码后的 3200B PCM（录音诊断用，可为 nil）。
	OnFrame func(pcm []byte)
	// OnStats 会话收尾统计。
	OnStats func(Stats)
	// OnLog 日志。
	OnLog func(format string, args ...any)
}

// Stats 是一次会话的对账结果。
type Stats struct {
	Duration      time.Duration
	TheoryFrames  int // floor(时长/100ms)
	Received      int // 实收帧（已解码）
	MissedFrames  int // 理论 - 实收
	DropPercent   float64
	BlockMiss     int   // 块级丢失（重组层 miss）
	QueueDropped  int64 // 队列满丢帧
	DeviceDrop    int   // 设备侧 status{drop}
	HasDeviceDrop bool
	FinalText     string
	Injected      bool
	ASRFailed     bool
}

func (s Stats) Summary() string {
	dev := ""
	if s.HasDeviceDrop {
		dev = fmt.Sprintf("  设备掉帧 %d", s.DeviceDrop)
	}
	return fmt.Sprintf("会话 %dms：理论帧 %d 实收 %d 差 %d（%.1f%%） 块miss %d%s",
		s.Duration.Milliseconds(), s.TheoryFrames, s.Received,
		s.MissedFrames, s.DropPercent, s.BlockMiss, dev)
}

// asrClient 抽象 asr.Client，便于单测注入假实现。
type asrClient interface {
	Connect(ctx context.Context) error
	SendFrame(ctx context.Context, pcm []byte) error
	SendEnd(ctx context.Context) error
	Results() <-chan asr.Result
	Close() error
}

// Manager 是连接级的会话管理者：一次连接一个，跨会话保留重组器学到的满片长度。
type Manager struct {
	ctrl   CtrlWriter
	cfg    func() Config
	h      Handler
	newASR func(asr.Config) asrClient

	rrMu sync.Mutex
	rr   *audio.Reassembler

	mu      sync.Mutex
	sess    *session
	last    Stats // 最近一次会话统计（设备 status 到达后回填）
	hasLast bool
}

// NewManager 创建会话管理者。cfg 在每次 voice.start 时调用，可取到最新配置。
func NewManager(ctrl CtrlWriter, cfg func() Config, h Handler) *Manager {
	return &Manager{
		ctrl: ctrl,
		cfg:  cfg,
		h:    h,
		rr:   audio.NewReassembler(),
		newASR: func(c asr.Config) asrClient {
			return asr.New(c)
		},
	}
}

// Log 通过 handler 的日志回调输出（CLI 打印用）。
func (m *Manager) Log(format string, args ...any) { m.logf(format, args...) }

func (m *Manager) logf(format string, args ...any) {
	if m.h.OnLog != nil {
		m.h.OnLog(format, args...)
	}
}

// OnVoiceStart 处理 voice.start：收束上一个（若有）并开新会话。
func (m *Manager) OnVoiceStart(format string) {
	m.mu.Lock()
	prev := m.sess
	m.sess = nil // 先摘掉：旧会话收尾期间音频不能被喂进去
	m.mu.Unlock()
	if prev != nil {
		m.logf("收到重复 voice.start，先收束上一个会话")
		go prev.abort("重复 voice.start") // 注入/校验可能耗时，别挡住新会话
	}

	cfg := m.cfg()
	if cfg.WaitFinal <= 0 {
		cfg.WaitFinal = defaultWaitFinal
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = defaultQueueDepth
	}

	// 会话切换必须复位块状态：块序号是跨会话递增的 mod 256，上一会话的残留会让
	// 新会话头几块被当成"已终结块的迟到片"整块丢掉。unit 是连接级常量，保留。
	m.rrMu.Lock()
	m.rr.Reset(true)
	m.rrMu.Unlock()

	s := &session{
		mgr:    m,
		cfg:    cfg,
		format: format,
		frames: make(chan []byte, cfg.QueueDepth),
		final:  make(chan struct{}),
		ended:  make(chan struct{}),
	}
	s.asr = m.newASR(cfg.ASR)

	m.mu.Lock()
	m.sess = s
	m.mu.Unlock()

	m.logf("会话开始（audio=%s）", format)
	s.start()
}

// OnAudio 处理一段 AUDIO 通知载荷（可能已合并多条通知）。
func (m *Manager) OnAudio(chunk []byte) {
	m.mu.Lock()
	s := m.sess
	m.mu.Unlock()
	if s == nil {
		return // 空闲期音频丢弃，不计数
	}
	m.rrMu.Lock()
	frames := m.rr.Feed(chunk)
	m.rrMu.Unlock()
	s.push(frames)
}

// OnVoiceEnd 处理 voice.end：结束音频流、等定稿、注入并收尾。
func (m *Manager) OnVoiceEnd() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	m.mu.Unlock()
	if s == nil {
		m.logf("收到 voice.end 但没有进行中的会话，忽略")
		return
	}
	go s.end()
}

// OnStatus 处理设备补发的掉帧对账帧：挂到最近一次会话统计上。
//
// 设备在 voice.end 之后才发 status，此时会话很可能已经收尾打印过统计，
// 所以这里补记并单独记一行日志，不重打整条统计。
func (m *Manager) OnStatus(drop int) {
	m.mu.Lock()
	if m.hasLast && !m.last.HasDeviceDrop {
		m.last.HasDeviceDrop = true
		m.last.DeviceDrop = drop
	}
	m.mu.Unlock()
	m.logf("设备掉帧 %d（已计入对账）", drop)
}

// Last 返回最近一次会话的统计（含设备掉帧回填）。
func (m *Manager) Last() (Stats, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.hasLast
}

// Close 收束当前会话（断连/退出时调用）。
func (m *Manager) Close() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	m.mu.Unlock()
	if s != nil {
		// 收尾里可能含注入与落地校验（数百 ms），绝不能阻塞 BLE 回调线程。
		go s.abort("连接关闭")
	}
}

// session 是一次语音会话。
type session struct {
	mgr    *Manager
	cfg    Config
	asr    asrClient
	format string

	frames chan []byte
	final  chan struct{} // 定稿/失败时关闭
	ended  chan struct{} // end() 进入时关闭

	startAt time.Time
	endAt   time.Time

	rx      atomic.Int64
	dropped atomic.Int64

	pushMu     sync.Mutex
	framesDone bool

	mu        sync.Mutex
	finalText string
	asrFailed bool

	finishOnce sync.Once
	endOnce    sync.Once
	finalOnce  sync.Once
}

func (s *session) start() {
	s.startAt = time.Now()
	go s.pump()
}

// pump 连接 ASR 并把音频帧喂进去，最后发末帧。
func (s *session) pump() {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	err := s.asr.Connect(ctx)
	cancel()
	if err != nil {
		s.mu.Lock()
		s.asrFailed = true
		s.mu.Unlock()
		s.logf("ASR 连接失败，本次会话无结果：%v", err)
		s.closeFinal()
		for range s.frames { // 排空，避免音频路径堵塞
		}
		return
	}

	go s.consume()

	for frame := range s.frames {
		fctx, fcancel := context.WithTimeout(context.Background(), sendTimeout)
		err := s.asr.SendFrame(fctx, frame)
		fcancel()
		if err != nil {
			s.logf("ASR 发送失败：%v", err)
			break
		}
	}
	if err := s.asr.SendEnd(context.Background()); err != nil {
		s.logf("ASR 末帧发送失败：%v", err)
	}
}

// consume 消费识别结果：候选字回调，定稿后只记录文本（收尾统一走 finish）。
func (s *session) consume() {
	for r := range s.asr.Results() {
		if r.Err != nil {
			s.logf("ASR 错误：%v", r.Err)
			continue
		}
		if r.Text == "" {
			continue
		}
		if !r.Final {
			if s.mgr.h.OnPartial != nil {
				s.mgr.h.OnPartial(r.Text)
			}
			continue
		}
		s.mu.Lock()
		s.finalText = r.Text
		s.mu.Unlock()
		s.closeFinal()
	}
}

// push 收下解码后的 PCM 帧；队列满就丢（延迟比丢字更难受）。
//
// 与 closeFrames 用同一把锁：BLE 回调线程可能还在喂帧，而收尾已经在关通道，
// 直接往已关闭的 channel 发送会 panic（select 里也一样）。
func (s *session) push(frames [][]byte) {
	for _, frame := range frames {
		s.rx.Add(1)
		if s.mgr.h.OnFrame != nil {
			s.mgr.h.OnFrame(frame)
		}
		s.pushMu.Lock()
		if s.framesDone {
			s.pushMu.Unlock()
			s.dropped.Add(1)
			continue
		}
		select {
		case s.frames <- frame:
		default:
			s.dropped.Add(1)
		}
		s.pushMu.Unlock()
	}
}

// closeFrames 关闭音频输入（幂等）。
func (s *session) closeFrames() {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.framesDone {
		return
	}
	s.framesDone = true
	close(s.frames)
}

// end 处理 voice.end：关音频流 → 等定稿 → 收尾。
func (s *session) end() {
	s.endOnce.Do(func() {
		s.endAt = time.Now()
		close(s.ended)
		s.closeFrames()
	})
	select {
	case <-s.final:
	case <-time.After(s.cfg.WaitFinal):
		s.logf("%s 内没等到定稿（本次不注入）", s.cfg.WaitFinal)
	}
	s.finish()
}

// abort 是断连/重复 start 的快速收束：不等定稿。
func (s *session) abort(reason string) {
	s.logf("会话被中断：%s", reason)
	s.endOnce.Do(func() {
		s.endAt = time.Now()
		close(s.ended)
		s.closeFrames()
	})
	s.closeFinal()
	s.finish()
}

// finish 是唯一的收尾路径：注入（仅一次）→ 回调 UI → 发 done → 统计。
func (s *session) finish() {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		text := s.finalText
		s.mu.Unlock()

		injected := false
		if text != "" && s.cfg.Inject != nil {
			used, err := s.cfg.Inject(text)
			if err != nil {
				s.logf("注入失败：%v", err)
			} else {
				injected = true
				s.logf("已注入（%s）：%s", used, text)
			}
		}

		if s.mgr.h.OnFinal != nil {
			s.mgr.h.OnFinal(text)
		}

		// done 必须发：设备 TRANSCRIBING/AGENT_RUNNING 只有收到它才回 DONE。
		if !s.cfg.SkipDone {
			if line, err := proto.AgentStatus(proto.AgentDone, ""); err == nil {
				if err := s.mgr.ctrl.WriteCTRL(line); err != nil {
					s.logf("下发 agent.status done 失败：%v", err)
				}
			}
		}

		stats := s.stats(text, injected)
		s.mgr.mu.Lock()
		s.mgr.last, s.mgr.hasLast = stats, true
		s.mgr.mu.Unlock()
		if s.mgr.h.OnStats != nil {
			s.mgr.h.OnStats(stats)
		}
		s.logf("%s", stats.Summary())

		if err := s.asr.Close(); err != nil {
			s.logf("ASR 关闭异常：%v", err)
		}
	})
}

func (s *session) stats(text string, injected bool) Stats {
	s.mu.Lock()
	failed := s.asrFailed
	s.mu.Unlock()

	end := s.endAt
	if end.IsZero() {
		end = time.Now()
	}
	dur := end.Sub(s.startAt)
	theory := int(dur / (100 * time.Millisecond))
	if theory < 0 {
		theory = 0
	}
	rx := int(s.rx.Load())
	missed := theory - rx
	if missed < 0 {
		missed = 0
	}
	pct := 0.0
	if theory > 0 {
		pct = float64(missed) / float64(theory) * 100
	}

	s.mgr.rrMu.Lock()
	blockMiss := s.mgr.rr.Miss()
	s.mgr.rrMu.Unlock()

	return Stats{
		Duration:     dur,
		TheoryFrames: theory,
		Received:     rx,
		MissedFrames: missed,
		DropPercent:  pct,
		BlockMiss:    blockMiss,
		QueueDropped: s.dropped.Load(),
		FinalText:    text,
		Injected:     injected,
		ASRFailed:    failed,
	}
}

func (s *session) closeFinal() {
	s.finalOnce.Do(func() { close(s.final) })
}

func (s *session) logf(format string, args ...any) {
	if s.mgr.h.OnLog != nil {
		s.mgr.h.OnLog(format, args...)
	}
}
