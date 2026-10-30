// Command probe 是 M0/M1 的命令行闸门：不启动 UI，只验证 BLE + 音频 + ASR 全链路。
//
//	扫描 → 连接 → 配对（CTRL 确认写）→ 订阅 → 打印事件 →
//	语音会话（分片重组 → ADPCM 解码 → 火山流式 ASR）→ 打印候选/定稿。
//
// 跑法：
//
//	go run ./cmd/probe                          # 只验证链路与音频
//	VOLCANO_API_KEY=xxx go run ./cmd/probe       # 接 ASR，终端看候选/定稿
//	go run ./cmd/probe -record /tmp/s.wav        # 同时录音
//	go run ./cmd/probe -done=false               # 不回 agent.status done（复现设备卡住）
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"sotto/internal/asr"
	"sotto/internal/audio"
	"sotto/internal/ble"
	"sotto/internal/proto"
	"sotto/internal/session"
)

type config struct {
	name     string
	impl     string
	scan     time.Duration
	record   string
	sendDone bool
	timeSync bool
	confirm  bool

	asrKey      string
	asrURL      string
	asrResource string
	asrModel    string
}

func main() {
	cfg := parseFlags()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
	}

	// 回调线程（CoreBluetooth 队列）只做入队，满了就丢并计数。
	audioCh := make(chan []byte, 128)
	eventCh := make(chan []byte, 64)
	var droppedAudio, droppedEvent atomicCounters

	client, err := ble.New(ble.Handler{
		OnAudio: func(b []byte) {
			select {
			case audioCh <- b:
			default:
				droppedAudio.Add(1)
			}
		},
		OnEvent: func(b []byte) {
			select {
			case eventCh <- b:
			default:
				droppedEvent.Add(1)
			}
		},
		OnLog: logf,
	}, cfg.impl)
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}

	logf("BLE 实现 %s / 启用蓝牙…", cfg.impl)
	if err := client.Enable(); err != nil {
		logf("%v", err)
		os.Exit(1)
	}

	logf("扫描 %s（最多 %s）…", cfg.name, cfg.scan)
	target, err := client.Scan(cfg.name, int(cfg.scan.Milliseconds()))
	if err != nil {
		logf("扫描失败: %v", err)
		os.Exit(1)
	}
	logf("广播：RSSI %d dBm，服务 %v", target.RSSI, target.Adv)

	logf("连接…")
	if err := client.Connect(target); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
	defer client.Close()

	gatt, err := client.Discover()
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}
	logf("发现的服务：%v", gatt.Services)
	logf("采用服务 %s", gatt.Service)
	for _, c := range gatt.Chars {
		logf("  特征 %s", c)
	}
	logf("CTRL 单次写值上限 %d B", gatt.WriteLimit)

	if err := client.Subscribe(); err != nil {
		logf("%v", err)
		os.Exit(1)
	}

	// 校时同时充当配对探测：CTRL 带 WRITE_ENC，链路未加密时确认写会失败。
	if cfg.timeSync {
		line, err := proto.TimeSet(time.Now().Unix())
		if err != nil {
			logf("构造校时失败: %v", err)
		} else if cfg.confirm {
			if err := client.WriteCTRLConfirmed(line); err != nil {
				logf("%v", err)
			} else {
				logf("校时（确认写）成功 → 链路已加密，配对 OK")
			}
		} else if err := client.WriteCTRL(line); err != nil {
			logf("校时失败: %v", err)
		} else {
			logf("校时已下发")
		}
	}

	// 录音 + 分片长度分布（CoreBluetooth 合并通知的取证手段）。
	rec := &recorder{logf: logf, cfg: cfg, chunks: make(map[int]int)}
	if cfg.record != "" {
		if err := rec.openWAV(cfg.record); err != nil {
			logf("创建 WAV 失败: %v", err)
			os.Exit(1)
		}
		logf("录音到 %s（16kHz/16bit/单声道）", cfg.record)
	}

	useASR := cfg.asrKey != ""
	if useASR {
		logf("ASR 已启用：%s（资源 %s，模型 %s）", cfg.asrURL, cfg.asrResource, cfg.asrModel)
	} else {
		logf("ASR 未启用（没有 Key）：只验证链路与音频")
	}

	// 会话层与应用里是同一份代码（internal/session）。
	mgr := session.NewManager(client, func() session.Config {
		c := session.Config{
			WaitFinal: 6 * time.Second,
			SkipDone:  !cfg.sendDone,
		}
		if useASR {
			c.ASR = asr.Config{
				APIKey:          cfg.asrKey,
				WSURL:           cfg.asrURL,
				ResourceID:      cfg.asrResource,
				Model:           cfg.asrModel,
				EnableNonStream: true,
			}
		}
		return c
	}, session.Handler{
		OnFrame: rec.writeFrame,
		OnPartial: func(text string) {
			logf("[asr] 候选：%s", text)
		},
		OnFinal: func(text string) {
			if text == "" {
				logf("[asr] 本次没有定稿文本")
				return
			}
			logf("[asr] 定稿：%s", text)
		},
		OnStats: rec.printStats,
		OnLog:   logf,
	})

	go consumeAudio(mgr, rec, audioCh)
	go consumeEvents(mgr, eventCh)

	waitForSignal()
	mgr.Close()
	rec.closeWAV()
	logf("退出。丢弃：音频 %d 片 / 事件 %d 片", droppedAudio.Load(), droppedEvent.Load())
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.name, "name", ble.DeviceName, "设备广播名")
	flag.StringVar(&cfg.impl, "impl", "cb", "BLE 实现：cb（cbgo，默认）或 lib（tinygo.org/x/bluetooth，对照用）")
	flag.DurationVar(&cfg.scan, "scan", 15*time.Second, "扫描超时")
	flag.StringVar(&cfg.record, "record", "", "把解出的 PCM 落成 WAV（16kHz/16bit/单声道）")
	flag.BoolVar(&cfg.sendDone, "done", true, "voice.end 后回 agent.status done（false 复现设备卡在 TRANSCRIBING）")
	flag.BoolVar(&cfg.timeSync, "time-sync", true, "连接后下发 time.set（同时探测配对是否完成）")
	flag.BoolVar(&cfg.confirm, "confirm-write", true, "校时用带响应写（能拿到配对/加密错误）")

	key := os.Getenv("VOLCANO_API_KEY")
	flag.StringVar(&cfg.asrKey, "asr-key", key, "火山 API Key（缺省读环境变量 VOLCANO_API_KEY）")
	flag.StringVar(&cfg.asrURL, "asr-url", asr.DefaultWSURL, "ASR WebSocket 端点")
	flag.StringVar(&cfg.asrResource, "asr-resource", asr.DefaultResourceID,
		"资源 ID：2.0 小时版 volc.seedasr.sauc.duration（默认）/ 并发版 …concurrent；1.0 用 volc.bigasr.*")
	flag.StringVar(&cfg.asrModel, "asr-model", asr.DefaultModel, "模型名（文档规定目前仅支持 bigmodel）")
	flag.Parse()
	return cfg
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func consumeAudio(mgr *session.Manager, rec *recorder, chunks <-chan []byte) {
	for chunk := range chunks {
		rec.countChunk(len(chunk))
		mgr.OnAudio(chunk)
	}
}

func consumeEvents(mgr *session.Manager, chunks <-chan []byte) {
	lines := proto.NewLineReassembler(0)
	for chunk := range chunks {
		for _, line := range lines.Feed(chunk) {
			ev, err := proto.ParseEvent(line)
			if err != nil {
				continue
			}
			switch ev.Kind {
			case proto.EventDeviceHello:
				mgr.Log("设备 hello，proto=%d", ev.Proto)
			case proto.EventVoiceStart:
				mgr.OnVoiceStart(ev.AudioFormat())
			case proto.EventVoiceEnd:
				mgr.OnVoiceEnd()
			case proto.EventStatus:
				mgr.OnStatus(ev.Drop)
			case proto.EventKeyAction:
				mgr.Log("设备按键：%s", ev.Action)
			case proto.EventAgentAction:
				mgr.Log("审批结果：%s → %s", ev.TaskID, ev.Action)
			default:
				mgr.Log("未知事件：%s", ev.Kind)
			}
		}
	}
}

// recorder 负责 WAV 落盘与会话统计上报。
type recorder struct {
	logf func(string, ...any)
	cfg  config

	mu     sync.Mutex
	wav    *audio.WAVWriter
	chunks map[int]int
}

func (r *recorder) openWAV(path string) error {
	w, err := audio.NewWAVWriter(path, 16000)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.wav = w
	r.mu.Unlock()
	return nil
}

func (r *recorder) writeFrame(pcm []byte) {
	r.mu.Lock()
	w := r.wav
	r.mu.Unlock()
	if w == nil {
		return
	}
	if err := w.Write(pcm); err != nil {
		r.logf("写 WAV 失败：%v", err)
		r.mu.Lock()
		r.wav = nil
		r.mu.Unlock()
	}
}

func (r *recorder) countChunk(n int) {
	r.mu.Lock()
	r.chunks[n]++
	r.mu.Unlock()
}

func (r *recorder) printStats(s session.Stats) {
	r.mu.Lock()
	dist := formatChunkDist(r.chunks)
	r.chunks = make(map[int]int)
	r.mu.Unlock()
	if dist != "" {
		r.logf("   通知载荷长度分布: %s（成倍长度 = CoreBluetooth 合并了多条通知）", dist)
	}
}

func (r *recorder) closeWAV() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wav == nil {
		return
	}
	if err := r.wav.Close(); err != nil {
		r.logf("WAV 落盘失败: %v", err)
		return
	}
	r.logf("WAV 已保存: %s", r.cfg.record)
	r.wav = nil
}

func formatChunkDist(lens map[int]int) string {
	if len(lens) == 0 {
		return ""
	}
	keys := make([]int, 0, len(lens))
	for k := range lens {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%dB×%d", k, lens[k]))
	}
	return strings.Join(parts, ", ")
}

// atomicCounters 是小号计数器（probe 只用来统计回调线程里的丢弃）。
type atomicCounters struct {
	mu sync.Mutex
	n  int64
}

func (c *atomicCounters) Add(d int64) {
	c.mu.Lock()
	c.n += d
	c.mu.Unlock()
}

func (c *atomicCounters) Load() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
