// Package asr 是火山引擎豆包流式语音识别（v3 双向流式）的客户端。
//
// 协议出处：https://docs.volcengine.com/docs/6561/1354869
//
//	端点   wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async
//	        （优化版二遍识别；配 request.enable_nonstream=true 得到定稿）
//	鉴权   X-Api-Key / X-Api-Resource-Id / X-Api-Request-Id(UUID) / X-Api-Sequence: -1
//	帧格式 4B header + 4B 大端 payload 长度 + payload
//	        byte0: version(4b)=0001 | header size(4b)=0001
//	        byte1: message type(4b) | flags(4b)
//	        byte2: serialization(4b) | compression(4b)
//	        byte3: 保留 0x00
//	首帧   full client request（type=0x1、JSON+gzip）
//	音频帧 audio only request（type=0x2、无序列化+gzip），最后一包 flags=0b0010
//	结果   full server response（type=0x9，多 4B sequence），result.text 是
//	        **全量累计文本**；flags & 0x2 = 定稿
//	错误   type=0xF：4B 错误码 + 4B 长度 + UTF-8 信息
//
// 音频恒为 16kHz/16bit/单声道/100ms 一包（3200B）：BLE 的 IMA ADPCM 块在重组层
// 已解回 PCM，ASR 只认裸 PCM。
package asr

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 默认参数。四个资源 ID 都是**同一套双向流式协议**（sauc = streaming）：
//
//	豆包流式语音识别 2.0（推荐）  小时版 volc.seedasr.sauc.duration
//	                              并发版 volc.seedasr.sauc.concurrent
//	豆包流式语音识别 1.0（历史）  小时版 volc.bigasr.sauc.duration
//	                              并发版 volc.bigasr.sauc.concurrent
//
// 文档：docs.volcengine.com/docs/doubao-voice/bidirectional-streaming-automatic-speech-recognition-websocket
// 端点、帧格式、enable_nonstream 语义完全一致，区别只在服务端用哪代模型与计费方式。
// 注意别填成 auc 那一族（录音文件/非流式识别），它走的是另一套协议。
const (
	DefaultWSURL = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async"

	ResourceID20           = "volc.seedasr.sauc.duration" // 2.0 小时版（默认）
	ResourceID20Concurrent = "volc.seedasr.sauc.concurrent"
	ResourceID10           = "volc.bigasr.sauc.duration" // 1.0 小时版（历史）
	ResourceID10Concurrent = "volc.bigasr.sauc.concurrent"

	// DefaultResourceID 默认用 2.0 小时版：文档明确"新接入用户请优先选择 2.0"。
	DefaultResourceID = ResourceID20

	// DefaultModel 文档规定 model_name"目前仅支持 bigmodel"（必选）。
	DefaultModel = "bigmodel"

	// PCMFrameBytes 一个音频包：100ms @ 16kHz/16bit/单声道。
	PCMFrameBytes = 3200
)

// 帧类型。
const (
	typeFullClientRequest  = 0x1
	typeAudioOnlyRequest   = 0x2
	typeFullServerResponse = 0x9
	typeServerError        = 0xF
)

// 序列化与压缩方式（byte2 的高低半字节）。
const (
	serializationNone = 0x0
	serializationJSON = 0x1
	compressionNone   = 0x0
	compressionGzip   = 0x1
)

// flagFinal 表示"定稿"：客户端用在最后一个音频帧上，服务端用在末包结果上。
const flagFinal = 0x2

// 服务端帧上限：语音 JSON 通常 <10KB，无上限时畸形 gzip 可 OOM。
const (
	maxServerPayload = 256 * 1024
	maxGunzipBytes   = 64 * 1024
)

// Config 是会话参数。
type Config struct {
	APIKey          string
	WSURL           string
	ResourceID      string
	Model           string
	EnableNonStream bool
}

// WithDefaults 补齐缺省值。
func (c Config) WithDefaults() Config {
	if c.WSURL == "" {
		c.WSURL = DefaultWSURL
	}
	if c.ResourceID == "" {
		c.ResourceID = DefaultResourceID
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	return c
}

// EncodeFrame 拼一帧：4B header + 4B 大端长度 + payload。
func EncodeFrame(msgType, flags, serialization, compression byte, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = 0x11 // version 1, header size 1
	out[1] = msgType<<4 | (flags & 0x0F)
	out[2] = serialization<<4 | (compression & 0x0F)
	out[3] = 0x00
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

// gzipBytes 压缩，失败返回 nil（调用方按发送失败处理）。
func gzipBytes(p []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(p); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// gunzipLimited 解压并限制输出长度（防 zip bomb）。
func gunzipLimited(p []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxGunzipBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxGunzipBytes {
		return nil, errors.New("asr: 解压结果超过上限")
	}
	return out, nil
}

// fullRequest 构造首帧的 JSON 体。
func fullRequest(cfg Config) []byte {
	req := map[string]any{
		"model_name":  cfg.Model,
		"enable_punc": true,
		"enable_itn":  true,
	}
	if cfg.EnableNonStream {
		req["enable_nonstream"] = true
	}

	body := map[string]any{
		"user":    map[string]any{"uid": "sotto"},
		"audio":   map[string]any{"format": "pcm", "rate": 16000, "bits": 16, "channel": 1},
		"request": req,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return raw
}

// FullRequestFrame 返回首帧字节（JSON + gzip）。
func FullRequestFrame(cfg Config) []byte {
	body := fullRequest(cfg.WithDefaults())
	if body == nil {
		return nil
	}
	return EncodeFrame(typeFullClientRequest, 0, serializationJSON, compressionGzip, gzipBytes(body))
}

// AudioFrame 返回一个音频帧（裸 PCM + gzip）。last=true 时带上定稿标志。
func AudioFrame(pcm []byte, last bool) []byte {
	var flags byte
	if last {
		flags = flagFinal
	}
	return EncodeFrame(typeAudioOnlyRequest, flags, serializationNone, compressionGzip, gzipBytes(pcm))
}

// ServerFrame 是解析后的服务端帧。
type ServerFrame struct {
	Kind    string // "result" | "error" | "unknown"
	Flags   byte
	Text    string // result.text（全量累计文本）
	Final   bool   // flags & 0x2
	ErrCode uint32
	ErrMsg  string
}

// ParseServerFrame 解析服务端帧。
//
// 服务端比客户端多一个 4B 大端 sequence 字段：
//
//	result: header + seq(4) + size(4) + payload(JSON，可 gzip)
//	error : header + code(4) + size(4) + message(UTF-8)
func ParseServerFrame(raw []byte) ServerFrame {
	if len(raw) < 12 {
		return ServerFrame{Kind: "unknown"}
	}
	flags := raw[1] & 0x0F
	comp := raw[2] & 0x0F
	switch msgType := raw[1] >> 4; msgType {
	case typeFullServerResponse:
		size := binary.BigEndian.Uint32(raw[8:12])
		if int(size) > maxServerPayload || 12+int(size) > len(raw) {
			return ServerFrame{Kind: "unknown", Flags: flags}
		}
		payload := raw[12 : 12+int(size)]
		if len(payload) == 0 { // 配置帧的 ack，尚无结果
			return ServerFrame{Kind: "result", Flags: flags}
		}
		data := payload
		if comp == compressionGzip {
			var err error
			if data, err = gunzipLimited(payload); err != nil {
				return ServerFrame{Kind: "unknown", Flags: flags}
			}
		}
		var parsed struct {
			Result struct {
				Text string `json:"text"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return ServerFrame{Kind: "result", Flags: flags}
		}
		return ServerFrame{
			Kind:  "result",
			Flags: flags,
			Text:  parsed.Result.Text,
			Final: flags&flagFinal != 0,
		}
	case typeServerError:
		if len(raw) < 12 {
			return ServerFrame{Kind: "unknown", Flags: flags}
		}
		code := binary.BigEndian.Uint32(raw[4:8])
		size := binary.BigEndian.Uint32(raw[8:12])
		if int(size) > maxGunzipBytes || 12+int(size) > len(raw) {
			return ServerFrame{Kind: "error", Flags: flags, ErrCode: code, ErrMsg: "（信息过长）"}
		}
		return ServerFrame{
			Kind:    "error",
			Flags:   flags,
			ErrCode: code,
			ErrMsg:  strings.ToValidUTF8(string(raw[12:12+int(size)]), ""),
		}
	default:
		return ServerFrame{Kind: "unknown", Flags: flags}
	}
}

// Error 把错误帧变成 error，便于调用方直接返回。
func (f ServerFrame) Error() error {
	if f.Kind != "error" {
		return nil
	}
	return fmt.Errorf("asr: 服务端错误 %d: %s", f.ErrCode, f.ErrMsg)
}
