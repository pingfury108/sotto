package asr

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
)

// serverResultFrame 造一个服务端结果帧（header + seq + size + payload）。
func serverResultFrame(t *testing.T, flags byte, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	payload := buf.Bytes()

	out := make([]byte, 12+len(payload))
	out[0] = 0x11
	out[1] = typeFullServerResponse<<4 | flags&0x0F
	out[2] = serializationJSON<<4 | compressionGzip
	binary.BigEndian.PutUint32(out[4:8], 1) // sequence
	binary.BigEndian.PutUint32(out[8:12], uint32(len(payload)))
	copy(out[12:], payload)
	return out
}

func TestEncodeFrameLayout(t *testing.T) {
	got := EncodeFrame(typeAudioOnlyRequest, flagFinal, serializationNone, compressionNone, []byte{1, 2, 3})
	if len(got) != 8+3 {
		t.Fatalf("帧长 %d，want 11", len(got))
	}
	if got[0] != 0x11 {
		t.Fatalf("byte0 = %#x，want 0x11（version 1 / header size 1）", got[0])
	}
	if got[1] != 0x22 {
		t.Fatalf("byte1 = %#x，want 0x22（type 0x2 | flags 0b0010）", got[1])
	}
	if got[2] != 0x00 {
		t.Fatalf("byte2 = %#x，want 0x00（无序列化/无压缩）", got[2])
	}
	if size := binary.BigEndian.Uint32(got[4:8]); size != 3 {
		t.Fatalf("长度字段 %d，want 3（大端）", size)
	}
	if !bytes.Equal(got[8:], []byte{1, 2, 3}) {
		t.Fatal("payload 不一致")
	}
}

func TestFullRequestFrame(t *testing.T) {
	frame := FullRequestFrame(Config{APIKey: "k", EnableNonStream: true})
	if frame == nil {
		t.Fatal("首帧为空")
	}
	if typ := frame[1] >> 4; typ != typeFullClientRequest {
		t.Fatalf("type = %#x，want 0x1", typ)
	}
	if ser, comp := frame[2]>>4, frame[2]&0x0F; ser != serializationJSON || comp != compressionGzip {
		t.Fatalf("序列化/压缩 = %#x/%#x，want JSON/gzip", ser, comp)
	}
	size := binary.BigEndian.Uint32(frame[4:8])
	zr, err := gzip.NewReader(bytes.NewReader(frame[8 : 8+size]))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	req, _ := parsed["request"].(map[string]any)
	if req["enable_nonstream"] != true {
		t.Fatalf("缺 enable_nonstream: %v", req)
	}
	audio, _ := parsed["audio"].(map[string]any)
	if audio["format"] != "pcm" || audio["rate"].(float64) != 16000 {
		t.Fatalf("音频参数不对: %v", audio)
	}
	if parsed["user"].(map[string]any)["uid"] != "sotto" {
		t.Fatalf("uid 不对: %v", parsed["user"])
	}
}

func TestParseServerResult(t *testing.T) {
	raw := serverResultFrame(t, 0, map[string]any{"result": map[string]any{"text": "你好世界"}})
	f := ParseServerFrame(raw)
	if f.Kind != "result" || f.Text != "你好世界" || f.Final {
		t.Fatalf("解析结果 %+v", f)
	}

	final := ParseServerFrame(serverResultFrame(t, flagFinal, map[string]any{"result": map[string]any{"text": "你好世界。"}}))
	if !final.Final {
		t.Fatal("flags&0x2 应判定为定稿")
	}
	if err := final.Error(); err != nil {
		t.Fatalf("结果帧不应报错: %v", err)
	}
}

func TestParseServerEmptyAck(t *testing.T) {
	raw := make([]byte, 12)
	raw[0] = 0x11
	raw[1] = typeFullServerResponse << 4
	if f := ParseServerFrame(raw); f.Kind != "result" || f.Text != "" {
		t.Fatalf("空 ack 应解析成空结果: %+v", f)
	}
}

func TestParseServerError(t *testing.T) {
	msg := []byte("invalid api key")
	raw := make([]byte, 12+len(msg))
	raw[0] = 0x11
	raw[1] = typeServerError << 4
	binary.BigEndian.PutUint32(raw[4:8], 45000001)
	binary.BigEndian.PutUint32(raw[8:12], uint32(len(msg)))
	copy(raw[12:], msg)

	f := ParseServerFrame(raw)
	if f.Kind != "error" || f.ErrCode != 45000001 || f.ErrMsg != "invalid api key" {
		t.Fatalf("错误帧解析 %+v", f)
	}
	if f.Error() == nil {
		t.Fatal("错误帧应转成 error")
	}
}

func TestParseServerGarbage(t *testing.T) {
	for _, raw := range [][]byte{nil, {1}, make([]byte, 11), make([]byte, 12)} {
		f := ParseServerFrame(raw)
		if f.Kind == "result" && f.Text != "" {
			t.Fatalf("垃圾数据不应解析出文本: %+v", f)
		}
	}
	// 长度字段远大于实际载荷：不能越界读。
	raw := make([]byte, 16)
	raw[0] = 0x11
	raw[1] = typeFullServerResponse << 4
	binary.BigEndian.PutUint32(raw[8:12], 1<<20)
	if f := ParseServerFrame(raw); f.Kind != "unknown" {
		t.Fatalf("越界长度应判为 unknown: %+v", f)
	}
}
