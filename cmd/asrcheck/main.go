// Command asrcheck 用零音频握手验证火山流式 ASR 的 Key / 资源 ID / 端点。
//
//	VOLCANO_API_KEY=xxx go run ./cmd/asrcheck
//	go run ./cmd/asrcheck -key xxx -resource volc.seedasr.sauc.duration
//
// 流程与向导里的"测试连接"一致：建 WebSocket → 发首帧 full client request →
// 等第一帧响应。不传音频，因此不消耗识别时长。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"sotto/internal/asr"
)

func main() {
	key := os.Getenv("VOLCANO_API_KEY")
	var (
		url      string
		resource string
		model    string
		timeout  time.Duration
		nonstrm  bool
	)
	flag.StringVar(&key, "key", key, "火山 API Key（缺省读环境变量 VOLCANO_API_KEY）")
	flag.StringVar(&url, "url", asr.DefaultWSURL, "WebSocket 端点")
	flag.StringVar(&resource, "resource", asr.DefaultResourceID,
		"资源 ID：2.0 小时版 volc.seedasr.sauc.duration（默认）/ 并发版 volc.seedasr.sauc.concurrent；"+
			"1.0 小时版 volc.bigasr.sauc.duration / 并发版 volc.bigasr.sauc.concurrent")
	flag.StringVar(&model, "model", asr.DefaultModel, "模型名（文档规定目前仅支持 bigmodel）")
	flag.BoolVar(&nonstrm, "nonstream", true, "enable_nonstream（二遍识别，配 bigmodel_async）")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "总超时")
	flag.Parse()

	if strings.TrimSpace(key) == "" {
		fmt.Fprintln(os.Stderr, "缺少 API Key：-key 或环境变量 VOLCANO_API_KEY")
		os.Exit(2)
	}

	cfg := asr.Config{
		APIKey:          key,
		WSURL:           url,
		ResourceID:      resource,
		Model:           model,
		EnableNonStream: nonstrm,
	}
	fmt.Printf("端点    %s\n资源    %s\n模型    %s（nonstream=%v）\n",
		url, resource, model, nonstrm)
	fmt.Print("握手…")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	err := asr.TestConnection(ctx, cfg)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Println(" 失败")
		fmt.Fprintf(os.Stderr, "\n%v\n\n", err)
		fmt.Fprintln(os.Stderr, "常见原因：")
		fmt.Fprintln(os.Stderr, "  401/403  Key 无效，或 Key 与资源代际不匹配（1.0 用 bigasr、2.0 用 seedasr）")
		fmt.Fprintln(os.Stderr, "  等不到响应 该资源 ID 未开通，或端点地址不对")
		fmt.Fprintln(os.Stderr, "  超时      网络/代理拦截 wss，或本机时间偏差过大")
		os.Exit(1)
	}
	fmt.Printf(" 成功（%d ms）\n", elapsed.Milliseconds())
}
