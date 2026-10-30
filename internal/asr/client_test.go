package asr

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConn 是脚本化的 WebSocket 连接：写全记下来，读按队列喂。
type fakeConn struct {
	mu     sync.Mutex
	writes [][]byte
	reads  chan []byte
	closed bool
}

func newFakeConn() *fakeConn {
	return &fakeConn{reads: make(chan []byte, 16)}
}

func (f *fakeConn) Write(ctx context.Context, p []byte) error {
	cp := make([]byte, len(p))
	copy(cp, p)
	f.mu.Lock()
	f.writes = append(f.writes, cp)
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) Read(ctx context.Context) ([]byte, error) {
	p, ok := <-f.reads
	if !ok {
		return nil, io.EOF
	}
	return p, nil
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) writesSoFar() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.writes...)
}

func newTestClient(t *testing.T, fake *fakeConn) *Client {
	t.Helper()
	c := New(Config{APIKey: "test-key"})
	c.dial = func(ctx context.Context, cfg Config) (conn, error) { return fake, nil }
	return c
}

func frameFlags(frame []byte) byte { return frame[1] & 0x0F }

func audioPayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	if typ := frame[1] >> 4; typ != typeAudioOnlyRequest {
		t.Fatalf("帧类型 %#x，want 音频帧", typ)
	}
	if ser := frame[2] >> 4; ser != serializationNone {
		t.Fatalf("音频帧序列化 %#x，want 0", ser)
	}
	size := binary.BigEndian.Uint32(frame[4:8])
	zr, err := gzip.NewReader(bytes.NewReader(frame[8 : 8+size]))
	if err != nil {
		t.Fatalf("音频帧不是 gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	return out
}

// SendFrame 保持一帧延迟：真正的最后一包由 SendEnd 带定稿标志发出。
func TestSendFrameKeepsOneFrameDelay(t *testing.T) {
	fake := newFakeConn()
	c := newTestClient(t, fake)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	a, b := bytes.Repeat([]byte{0xAA}, PCMFrameBytes), bytes.Repeat([]byte{0xBB}, PCMFrameBytes)
	if err := c.SendFrame(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.writesSoFar()); n != 1 {
		t.Fatalf("首帧后写了 %d 次，want 1（只发了 full client request）", n)
	}
	if err := c.SendFrame(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	writes := fake.writesSoFar()
	if len(writes) != 2 {
		t.Fatalf("写了 %d 次，want 2", len(writes))
	}
	if got := audioPayload(t, writes[1]); !bytes.Equal(got, a) {
		t.Fatal("第二帧应发出上一包 a（保持一帧延迟）")
	}
	if flags := frameFlags(writes[1]); flags != 0 {
		t.Fatalf("中间帧 flags=%#x，want 0", flags)
	}
	if err := c.SendEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	writes = fake.writesSoFar()
	if len(writes) != 3 {
		t.Fatalf("写了 %d 次，want 3", len(writes))
	}
	if got := audioPayload(t, writes[2]); !bytes.Equal(got, b) {
		t.Fatal("末帧应是最后一包 b")
	}
	if flags := frameFlags(writes[2]); flags != flagFinal {
		t.Fatalf("末帧 flags=%#x，want %#x（0b0010 定稿）", flags, flagFinal)
	}
}

func TestSendEndWithNoAudio(t *testing.T) {
	fake := newFakeConn()
	c := newTestClient(t, fake)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SendEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	writes := fake.writesSoFar()
	if len(writes) != 2 {
		t.Fatalf("写了 %d 次，want 2（首帧 + 空末帧）", len(writes))
	}
	if flags := frameFlags(writes[1]); flags != flagFinal {
		t.Fatalf("空末帧 flags=%#x，want %#x", flags, flagFinal)
	}
	if got := audioPayload(t, writes[1]); len(got) != 0 {
		t.Fatalf("空末帧载荷应为空，得到 %d 字节", len(got))
	}
}

func TestResultsPartialThenFinal(t *testing.T) {
	fake := newFakeConn()
	c := newTestClient(t, fake)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fake.reads <- serverResultFrame(t, 0, map[string]any{"result": map[string]any{"text": "你"}})
	fake.reads <- serverResultFrame(t, flagFinal, map[string]any{"result": map[string]any{"text": "你好。"}})
	close(fake.reads)

	var got []Result
	timeout := time.After(3 * time.Second)
	for {
		select {
		case r, ok := <-c.Results():
			if !ok {
				if len(got) != 2 {
					t.Fatalf("结果数 %d，want 2: %+v", len(got), got)
				}
				if got[0].Text != "你" || got[0].Final {
					t.Fatalf("第 1 条结果不对: %+v", got[0])
				}
				if got[1].Text != "你好。" || !got[1].Final {
					t.Fatalf("第 2 条应是定稿: %+v", got[1])
				}
				return
			}
			got = append(got, r)
		case <-timeout:
			t.Fatalf("等待结果超时，已收到 %+v", got)
		}
	}
}

func TestResultsServerError(t *testing.T) {
	fake := newFakeConn()
	c := newTestClient(t, fake)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	msg := []byte("invalid api key")
	raw := make([]byte, 12+len(msg))
	raw[0] = 0x11
	raw[1] = typeServerError << 4
	binary.BigEndian.PutUint32(raw[4:8], 45000001)
	binary.BigEndian.PutUint32(raw[8:12], uint32(len(msg)))
	copy(raw[12:], msg)
	fake.reads <- raw
	close(fake.reads)

	select {
	case r, ok := <-c.Results():
		if !ok || r.Err == nil {
			t.Fatalf("应收到错误结果，得到 %+v ok=%v", r, ok)
		}
		if !strings.Contains(r.Err.Error(), "45000001") {
			t.Fatalf("错误信息应带错误码: %v", r.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待错误结果超时")
	}
}

func TestConnectWithoutAPIKey(t *testing.T) {
	c := New(Config{})
	if err := c.Connect(context.Background()); err == nil {
		t.Fatal("缺 API Key 应报错")
	}
}

func TestUUIDv4(t *testing.T) {
	a, b := newUUIDv4(), newUUIDv4()
	if len(a) != 36 || a == b {
		t.Fatalf("UUID 生成异常: %q / %q", a, b)
	}
	if a[14] != '4' {
		t.Fatalf("应是 v4: %q", a)
	}
}
