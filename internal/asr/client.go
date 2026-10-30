package asr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Result 是一条识别结果。Text 是**全量累计文本**（不是增量），Final 表示定稿。
// Err 非空表示流中断（服务端错误帧或连接断开）。
type Result struct {
	Text  string
	Final bool
	Err   error
}

// conn 抽象 WebSocket 连接，便于单测注入。
type conn interface {
	Write(ctx context.Context, p []byte) error
	Read(ctx context.Context) ([]byte, error)
	Close() error
}

// Client 服务一次语音会话。生命周期：Connect → SendFrame×N → SendEnd → Close。
type Client struct {
	cfg  Config
	dial func(ctx context.Context, cfg Config) (conn, error)

	mu         sync.Mutex
	cn         conn
	pending    []byte // 末帧保底：真正的最后一包由 SendEnd 带定稿标志发出
	hasPending bool
	closed     bool

	results chan Result
	done    chan struct{}
	once    sync.Once
}

// New 创建会话客户端。
func New(cfg Config) *Client {
	return &Client{
		cfg:     cfg.WithDefaults(),
		dial:    dialWS,
		results: make(chan Result, 32),
		done:    make(chan struct{}),
	}
}

// Results 产出识别结果；流结束时关闭。
func (c *Client) Results() <-chan Result { return c.results }

// Connect 建立连接并发出首帧（full client request）。
func (c *Client) Connect(ctx context.Context) error {
	if c.cfg.APIKey == "" {
		return errors.New("asr: 缺少火山 API Key（配置 volcano_api_key）")
	}
	cn, err := c.dial(ctx, c.cfg)
	if err != nil {
		return err
	}
	frame := FullRequestFrame(c.cfg)
	if frame == nil {
		cn.Close()
		return errors.New("asr: 构造首帧失败")
	}
	if err := cn.Write(ctx, frame); err != nil {
		cn.Close()
		return fmt.Errorf("asr: 发送首帧失败: %w", err)
	}
	c.mu.Lock()
	c.cn = cn
	c.mu.Unlock()
	go c.recvLoop(cn)
	return nil
}

// SendFrame 发送一个 100ms PCM 帧（内部保持一帧延迟，见 SendEnd）。
func (c *Client) SendFrame(ctx context.Context, pcm []byte) error {
	c.mu.Lock()
	cn := c.cn
	if cn == nil {
		c.mu.Unlock()
		return errors.New("asr: 未连接")
	}
	prev, hasPrev := c.pending, c.hasPending
	c.pending, c.hasPending = pcm, true
	c.mu.Unlock()

	if !hasPrev {
		return nil
	}
	if err := cn.Write(ctx, AudioFrame(prev, false)); err != nil {
		return fmt.Errorf("asr: 发送音频失败: %w", err)
	}
	return nil
}

// SendEnd 把最后一包音频带定稿标志（flags=0b0010）发出。零音频会话发空末帧。
func (c *Client) SendEnd(ctx context.Context) error {
	c.mu.Lock()
	cn := c.cn
	last := c.pending
	c.pending, c.hasPending = nil, false
	c.mu.Unlock()

	if cn == nil {
		return errors.New("asr: 未连接")
	}
	if err := cn.Write(ctx, AudioFrame(last, true)); err != nil {
		return fmt.Errorf("asr: 发送末帧失败: %w", err)
	}
	return nil
}

// Close 关闭连接。幂等。
func (c *Client) Close() error {
	c.mu.Lock()
	cn := c.cn
	c.cn = nil
	c.mu.Unlock()

	var err error
	if cn != nil {
		err = cn.Close()
	}
	c.once.Do(func() { close(c.done) })
	return err
}

// recvLoop 读服务端帧。注意 result.text 是全量累计文本，直接透传。
func (c *Client) recvLoop(cn conn) {
	defer close(c.results)
	for {
		raw, err := cn.Read(context.Background())
		if err != nil {
			if !c.isClosed() {
				c.emit(Result{Err: fmt.Errorf("asr: 连接中断: %w", err)}, true)
			}
			return
		}
		frame := ParseServerFrame(raw)
		switch frame.Kind {
		case "error":
			c.emit(Result{Err: frame.Error()}, true)
			return
		case "result":
			if frame.Text == "" && !frame.Final {
				continue // 空 ack / 无语义帧
			}
			c.emit(Result{Text: frame.Text, Final: frame.Final}, frame.Final)
			if frame.Final {
				return // 定稿即会话终点：之后连接关闭是正常收尾，不当错误
			}
		}
	}
}

// emit 投递结果。通道满时丢中间结果（预览滞后无碍），定稿与错误不丢。
func (c *Client) emit(r Result, keep bool) {
	select {
	case c.results <- r:
		return
	default:
	}
	if !keep {
		return
	}
	select {
	case c.results <- r:
	case <-c.done:
	}
}

func (c *Client) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// ---- WebSocket ----

type wsConn struct {
	c *websocket.Conn
}

func (w *wsConn) Write(ctx context.Context, p []byte) error {
	return w.c.Write(ctx, websocket.MessageBinary, p)
}

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}

func (w *wsConn) Close() error {
	return w.c.CloseNow()
}

func dialWS(ctx context.Context, cfg Config) (conn, error) {
	headers := http.Header{}
	headers.Set("X-Api-Key", cfg.APIKey)
	headers.Set("X-Api-Resource-Id", cfg.ResourceID)
	headers.Set("X-Api-Request-Id", newUUIDv4())
	headers.Set("X-Api-Sequence", "-1")

	c, resp, err := websocket.Dial(ctx, cfg.WSURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if resp != nil {
			// 401/403 通常意味着 Key 或资源 ID 不对；把状态码与判断线索一起带出来。
			hint := ""
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				hint = "：Key 无效，或 Key 与资源代际不匹配（1.0 → volc.bigasr.sauc.duration，" +
					"2.0 → volc.seedasr.sauc.duration）"
			}
			return nil, fmt.Errorf("asr: WebSocket 连接失败（HTTP %d）%s: %w", resp.StatusCode, hint, err)
		}
		return nil, fmt.Errorf("asr: WebSocket 连接失败: %w", err)
	}
	c.SetReadLimit(maxServerPayload)
	return &wsConn{c: c}, nil
}

// TestConnection 做一次零音频握手，验证 Key / 资源 ID / 端点（向导"测试连接"用）。
func TestConnection(ctx context.Context, cfg Config) error {
	cfg = cfg.WithDefaults()
	if cfg.APIKey == "" {
		return errors.New("asr: 缺少火山 API Key")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cn, err := dialWS(dialCtx, cfg)
	if err != nil {
		return err
	}
	defer cn.Close()

	frame := FullRequestFrame(cfg)
	if frame == nil {
		return errors.New("asr: 构造首帧失败")
	}
	if err := cn.Write(dialCtx, frame); err != nil {
		return fmt.Errorf("asr: 发送首帧失败: %w", err)
	}
	raw, err := cn.Read(dialCtx)
	if err != nil {
		return fmt.Errorf("asr: 等不到服务端响应（Key/资源 ID 是否正确？）: %w", err)
	}
	parsed := ParseServerFrame(raw)
	if err := parsed.Error(); err != nil {
		return err
	}
	return nil
}

// newUUIDv4 生成 UUID v4（X-Api-Request-Id 用，避免引入额外依赖）。
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 退化：时间戳 + 固定后缀，仍然唯一到会话级。
		return fmt.Sprintf("%d-0000-4000-8000-000000000000", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
