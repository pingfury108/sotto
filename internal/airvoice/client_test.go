package airvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer 模拟空音 App 的 HTTP 服务。
type fakeServer struct {
	ready     bool
	token     string
	busyTimes int32 // 前 N 次 /recognize 返回 409
	text      string

	gotPCM      []byte
	gotToken    string
	recognizeIn int32
}

func newFake(t *testing.T, f *fakeServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("X-Token") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"bad token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ready":%v}`, f.ready)
	})
	mux.HandleFunc("/recognize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.token != "" && r.Header.Get("X-Token") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"bad token"}`)
			return
		}
		// 服务端按 Content-Length 精确读：长度不对就必须失败。
		if r.ContentLength <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"empty body"}`)
			return
		}
		if r.URL.Query().Get("fmt") != "pcm" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"need fmt=pcm"}`)
			return
		}
		n := atomic.AddInt32(&f.recognizeIn, 1)
		if n <= f.busyTimes {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"busy, retry later"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.gotPCM = body
		f.gotToken = r.Header.Get("X-Token")
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"text": f.text, "ms": 12})
		w.Write(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func hostPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"192.168.32.145:8787":        "http://192.168.32.145:8787",
		" http://10.0.0.2:8787/ ":    "http://10.0.0.2:8787",
		"https://example.local:8443": "https://example.local:8443",
	}
	for in, want := range ok {
		got, err := Config{BaseURL: in}.normalize()
		if err != nil || got != want {
			t.Fatalf("normalize(%q) = %q, %v；want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "192.168.1.5", "http://host:8787/api"} {
		if _, err := (Config{BaseURL: bad}).normalize(); err == nil {
			t.Fatalf("%q 应当报错", bad)
		}
	}
}

func TestHealth(t *testing.T) {
	f := &fakeServer{ready: true, token: "s3cret"}
	srv := newFake(t, f)
	base := hostPort(t, srv)

	if err := CheckHealth(context.Background(), Config{BaseURL: base, Token: "s3cret"}); err != nil {
		t.Fatalf("健康检查应通过: %v", err)
	}
	if err := CheckHealth(context.Background(), Config{BaseURL: base, Token: "wrong"}); err == nil ||
		!strings.Contains(err.Error(), "Token") {
		t.Fatalf("Token 错应报 Token 错误，得到 %v", err)
	}
	f.ready = false
	if err := CheckHealth(context.Background(), Config{BaseURL: base, Token: "s3cret"}); err == nil ||
		!strings.Contains(err.Error(), "还没就绪") {
		t.Fatalf("ready=false 应报未就绪，得到 %v", err)
	}
}

// 完整链路：攒帧 → SendEnd 一次性 POST → Results 出一条 Final。
func TestRecognizeRoundTrip(t *testing.T) {
	f := &fakeServer{ready: true, text: "今天天气真不错。"}
	srv := newFake(t, f)

	c := New(Config{BaseURL: hostPort(t, srv)})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	frame := bytes.Repeat([]byte{0x11, 0x22}, frameBytes/2)
	for i := 0; i < 3; i++ {
		if err := c.SendFrame(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SendEnd(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-c.Results():
		if r.Err != nil {
			t.Fatalf("识别返回错误: %v", r.Err)
		}
		if r.Text != "今天天气真不错。" || !r.Final {
			t.Fatalf("结果不对: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等不到识别结果")
	}
	if len(f.gotPCM) != 3*frameBytes {
		t.Fatalf("服务端收到 %d 字节，want %d", len(f.gotPCM), 3*frameBytes)
	}
}

// 409 引擎忙要退避重试，最终成功。
func TestBusyRetry(t *testing.T) {
	f := &fakeServer{ready: true, busyTimes: 2, text: "重试成功"}
	srv := newFake(t, f)

	c := New(Config{BaseURL: hostPort(t, srv)})
	ctx := context.Background()
	if err := c.SendFrame(ctx, make([]byte, frameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := c.SendEnd(ctx); err != nil {
		t.Fatalf("两次 409 后应重试成功: %v", err)
	}
	r := <-c.Results()
	if r.Text != "重试成功" || r.Err != nil {
		t.Fatalf("重试结果不对: %+v", r)
	}
	if got := atomic.LoadInt32(&f.recognizeIn); got != 3 {
		t.Fatalf("请求次数 %d，want 3", got)
	}
}

// 一直忙：重试后放弃，但以错误结果收场（会话层仍会发 done）。
func TestBusyGivesUp(t *testing.T) {
	f := &fakeServer{ready: true, busyTimes: 99}
	srv := newFake(t, f)

	c := New(Config{BaseURL: hostPort(t, srv)})
	ctx := context.Background()
	if err := c.SendFrame(ctx, make([]byte, frameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := c.SendEnd(ctx); err == nil {
		t.Fatal("持续 409 最终应报错")
	}
	r := <-c.Results()
	if r.Err == nil || !strings.Contains(r.Err.Error(), "忙碌") {
		t.Fatalf("应给出忙碌提示，得到 %+v", r)
	}
}

func TestTokenSent(t *testing.T) {
	f := &fakeServer{ready: true, token: "abc", text: "ok"}
	srv := newFake(t, f)
	c := New(Config{BaseURL: hostPort(t, srv), Token: "abc"})
	ctx := context.Background()
	if err := c.SendFrame(ctx, make([]byte, frameBytes)); err != nil {
		t.Fatal(err)
	}
	if err := c.SendEnd(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-c.Results()
	if r.Text != "ok" {
		t.Fatalf("结果 %+v", r)
	}
	if f.gotToken != "abc" {
		t.Fatalf("X-Token 未发送，服务端收到 %q", f.gotToken)
	}
}

// 超过上限就地截断：只保留前 MaxSeconds 秒，并记一次日志。
func TestTruncateAtLimit(t *testing.T) {
	f := &fakeServer{ready: true, text: "截断也能识别"}
	srv := newFake(t, f)

	var logged []string
	c := New(Config{
		BaseURL:    hostPort(t, srv),
		MaxSeconds: 1, // 1 秒 = 10 帧
		Logf:       func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	})
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := c.SendFrame(ctx, make([]byte, frameBytes)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SendEnd(ctx); err != nil {
		t.Fatal(err)
	}
	<-c.Results()
	if len(f.gotPCM) != 10*frameBytes {
		t.Fatalf("应只发送 %d 字节，实际 %d", 10*frameBytes, len(f.gotPCM))
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "上限") {
		t.Fatalf("应记一次截断日志，得到 %v", logged)
	}
}

// 没有音频：不发请求，直接以空结果收场（会话层照常收尾）。
func TestEmptyAudio(t *testing.T) {
	f := &fakeServer{ready: true}
	srv := newFake(t, f)
	c := New(Config{BaseURL: hostPort(t, srv)})
	if err := c.SendEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := <-c.Results()
	if r.Err != nil || r.Text != "" || !r.Final {
		t.Fatalf("空音频应得空定稿: %+v", r)
	}
	if atomic.LoadInt32(&f.recognizeIn) != 0 {
		t.Fatal("空音频不该发请求")
	}
}

// 连不上：错误信息要能让人立刻知道查什么。
func TestUnreachable(t *testing.T) {
	err := CheckHealth(context.Background(), Config{BaseURL: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "同一 WiFi") {
		t.Fatalf("应给出排查提示，得到 %v", err)
	}
}
