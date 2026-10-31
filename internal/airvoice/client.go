// Package airvoice 是局域网整段识别客户端，对接「空音输入法」的 HTTP API
// （App 里打开「局域网识别 API」后监听 <手机IP>:8787）。
//
//	GET  /health              → {"ready":true|false}
//	POST /recognize?fmt=pcm   → {"text":"...","ms":320}
//	鉴权：请求头 X-Token（服务端 token 非空时校验）
//	忙：409（引擎串行，IME 正在语音输入时会排队）
//
// 与火山的差别是**没有流式**：音频先攒在内存里，松手（SendEnd）时一次性 POST。
// 所以本实现实现的是 session.Engine 的"批量"语义：SendFrame 只追加，
// Simple 结果在 SendEnd 之后才投递一条 Final。
//
// 三个从服务端实现（AsrServer.kt）读出来的约束，必须照做：
//  1. 请求必须带 Content-Length —— NanoHTTPD 按头精确读，chunked 会挂到超时；
//  2. 409 要退避重试（引擎串行）；
//  3. 服务端 60s 会静默截断，客户端提前在 55s 截断并记日志，行为可预期。
package airvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"sotto/internal/asr"
)

const (
	// DefaultMaxSeconds 客户端在 55s 截断（服务端 60s 会静默截断）。
	DefaultMaxSeconds = 55
	// frameBytes 一帧解码后的 PCM 字节数（100ms @16k/16bit/mono）。
	frameBytes = 3200
	// 引擎忙（409）的重试节奏：150ms、400ms。
	retryDelay = 150 * time.Millisecond
	maxRetries = 2
	// 单次识别请求的超时（上传 + 手机端推理）。
	recognizeTimeout = 25 * time.Second
	healthTimeout    = 2 * time.Second
)

// Config 是连接参数。
type Config struct {
	// BaseURL 只填 host:port（如 192.168.32.145:8787），自动补 http://；
	// 也容忍带 scheme 或末尾斜杠的写法。
	BaseURL string
	// Token 可选，与服务端 App 里设置的 X-Token 一致；留空表示服务端不鉴权。
	Token string
	// MaxSeconds 客户端截断秒数，0 用 DefaultMaxSeconds。
	MaxSeconds int
	// Logf 可选日志（截断提示等）。
	Logf func(format string, args ...any)
}

// normalize 把用户输入整理成 baseURL（无末尾斜杠）。
func (c Config) normalize() (string, error) {
	u := strings.TrimSpace(c.BaseURL)
	if u == "" {
		return "", errors.New("airvoice: 没有填局域网地址（形如 192.168.32.145:8787）")
	}
	u = strings.TrimSuffix(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	// 只接受 host:port；路径前缀会让 URL 拼错，提前说清楚。
	rest := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	if strings.Contains(rest, "/") {
		return "", fmt.Errorf("airvoice: 地址只填 host:port，不要带路径：%s", c.BaseURL)
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		return "", fmt.Errorf("airvoice: 地址需要 host:port 形式：%s", c.BaseURL)
	}
	return u, nil
}

func (c Config) maxSeconds() int {
	if c.MaxSeconds <= 0 {
		return DefaultMaxSeconds
	}
	return c.MaxSeconds
}

func (c Config) maxBytes() int { return c.maxSeconds() * frameBytes * 10 }

// CheckHealth 探活：GET /health 且 ready=true 才算可用。
//
// 给界面"测试"按钮和 CLI 用；它不参与会话流程（会话里由 Connect 调）。
func CheckHealth(ctx context.Context, cfg Config) error {
	base, err := cfg.normalize()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	if cfg.Token != "" {
		req.Header.Set("X-Token", cfg.Token)
	}
	resp, err := (&http.Client{Timeout: healthTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s：%v（手机和电脑在同一 WiFi？手机 IP 变了吗？）", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return errors.New("Token 不匹配：检查手机端 App 里设置的 X-Token")
	default:
		return fmt.Errorf("健康检查返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var h struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("健康检查响应不是预期 JSON：%s", strings.TrimSpace(string(body)))
	}
	if !h.Ready {
		return errors.New("服务在线，但识别模型还没就绪（手机端刚开机？稍等再试）")
	}
	return nil
}

// Client 实现 session.Engine：攒帧 → 松手一次性识别。
type Client struct {
	cfg   Config
	limit int

	mu        sync.Mutex
	buf       []byte
	truncated bool
	dropped   int

	results chan asr.Result
	once    sync.Once
}

// New 创建客户端（一次会话一个实例）。
func New(cfg Config) *Client {
	return &Client{
		cfg:     cfg,
		limit:   cfg.maxBytes(),
		results: make(chan asr.Result, 1),
	}
}

// Connect 探活。ready=false 或连不上都直接报错，由会话层收尾（仍会发 done）。
func (c *Client) Connect(ctx context.Context) error {
	return CheckHealth(ctx, c.cfg)
}

// SendFrame 把一帧 PCM 追加到缓冲。超过上限就地截断并只记一次。
func (c *Client) SendFrame(ctx context.Context, pcm []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		c.dropped++
		return nil
	}
	if len(c.buf)+len(pcm) > c.limit {
		c.truncated = true
		c.dropped++
		return nil
	}
	c.buf = append(c.buf, pcm...)
	return nil
}

// SendEnd 发起识别并把结果投递到 Results（只有一条 Final）。
func (c *Client) SendEnd(ctx context.Context) error {
	c.mu.Lock()
	pcm := c.buf
	c.buf = nil
	truncated := c.truncated
	c.mu.Unlock()

	if len(pcm) == 0 {
		// 没有音频（误碰/极短）：不发请求，但仍给一条"空定稿"，
		// 让会话层立刻收尾而不是干等超时。
		c.emit(asr.Result{Final: true})
		return nil
	}

	base, err := c.cfg.normalize()
	if err != nil {
		c.emit(asr.Result{Err: err})
		return err
	}

	if truncated && c.cfg.Logf != nil {
		// 只是提示，不影响识别（服务端也会截断到 60s）。
		c.cfg.Logf("音频已达 %d 秒上限，超出部分未发送", c.cfg.maxSeconds())
	}

	text, err := c.recognize(ctx, base, pcm)
	if err != nil {
		c.emit(asr.Result{Err: err})
		return err
	}
	c.emit(asr.Result{Text: text, Final: true})
	return nil
}

// Results 产出识别结果；SendEnd 之后关闭。
func (c *Client) Results() <-chan asr.Result { return c.results }

// Close 幂等。
func (c *Client) Close() error {
	c.once.Do(func() { close(c.results) })
	return nil
}

// recognize 发一次 POST，409 退避重试。
func (c *Client) recognize(ctx context.Context, base string, pcm []byte) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryDelay * time.Duration(attempt)):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, retry, err := c.postOnce(ctx, base, pcm)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retry {
			return "", err
		}
	}
	return "", fmt.Errorf("%w（手机端持续忙碌：可能正在用语音输入，稍后再试）", lastErr)
}

// postOnce 发一次请求。retry=true 表示"引擎忙，可以重试"。
func (c *Client) postOnce(ctx context.Context, base string, pcm []byte) (text string, retry bool, err error) {
	// Content-Length 必须是准确长度：NanoHTTPD 不支持 chunked。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/recognize?fmt=pcm", bytes.NewReader(pcm))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.cfg.Token != "" {
		req.Header.Set("X-Token", c.cfg.Token)
	}

	resp, err := (&http.Client{Timeout: recognizeTimeout}).Do(req)
	if err != nil {
		return "", false, fmt.Errorf("连不上 %s：%v", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	snippet := strings.TrimSpace(string(body))

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Text string `json:"text"`
			MS   int    `json:"ms"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", false, fmt.Errorf("识别响应不是预期 JSON：%s", snippet)
		}
		return out.Text, false, nil
	case http.StatusConflict:
		return "", true, errors.New("引擎忙（409）")
	case http.StatusUnauthorized:
		return "", false, errors.New("Token 不匹配（401）：检查手机端 App 的 X-Token")
	case http.StatusRequestEntityTooLarge:
		return "", false, errors.New("音频超过服务端 16MB 上限（413）")
	case http.StatusBadRequest:
		return "", false, fmt.Errorf("服务端拒绝（400）：%s", snippet)
	default:
		return "", false, fmt.Errorf("识别失败（HTTP %d）：%s", resp.StatusCode, snippet)
	}
}

// emit 投递一条结果（Results 只有一条，缓冲 1）。
func (c *Client) emit(r asr.Result) {
	select {
	case c.results <- r:
	default:
	}
}
