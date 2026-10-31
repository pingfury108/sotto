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

	"sotto/internal/airvoice"
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
		backend  string
		avURL    string
		avToken  string
		hotwords string
		contexts string
		endWin   int
		ddc      bool
	)
	flag.StringVar(&key, "key", key, "火山 API Key（缺省读环境变量 VOLCANO_API_KEY）")
	flag.StringVar(&url, "ws-url", asr.DefaultWSURL, "火山 WebSocket 端点（volcano 后端）")
	flag.StringVar(&resource, "resource", asr.DefaultResourceID,
		"资源 ID：2.0 小时版 volc.seedasr.sauc.duration（默认）/ 并发版 volc.seedasr.sauc.concurrent；"+
			"1.0 小时版 volc.bigasr.sauc.duration / 并发版 volc.bigasr.sauc.concurrent")
	flag.StringVar(&model, "model", asr.DefaultModel, "模型名（文档规定目前仅支持 bigmodel）")
	flag.BoolVar(&nonstrm, "nonstream", true, "enable_nonstream（二遍识别，配 bigmodel_async）")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "总超时")
	flag.StringVar(&backend, "backend", "volcano", "识别后端：volcano（云端流式）/ airvoice（局域网整段）")
	flag.StringVar(&avURL, "airvoice-url", "", "airvoice：host:port，如 192.168.32.145:8787")
	urlAlias := ""
	flag.StringVar(&urlAlias, "url", "", "airvoice 地址别名（等同 -airvoice-url）")
	flag.StringVar(&avToken, "token", "", "airvoice：X-Token（手机端未开鉴权则留空）")
	flag.StringVar(&hotwords, "hotwords", "", "热词（逗号分隔），例如 \"伏羲,sotto\"")
	flag.StringVar(&contexts, "contexts", "", "对话上下文（竖线分隔，按 user/bot 交替）")
	flag.IntVar(&endWin, "end-window", 800, "静音判停阈值 ms（300–5000）")
	flag.BoolVar(&ddc, "ddc", false, "enable_ddc 语义顺滑")
	flag.Parse()
	if avURL == "" {
		avURL = urlAlias
	}

	if backend == "airvoice" {
		runAirvoice(avURL, avToken, timeout)
		return
	}
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
		Hotwords:        splitList(hotwords),
		Contexts:        splitList(contexts),
		EndWindowSize:   endWin,
		EnableDDC:       ddc,
	}
	fmt.Printf("端点    %s\n资源    %s\n模型    %s（nonstream=%v）\n",
		url, resource, model, nonstrm)
	if len(cfg.Hotwords) > 0 {
		fmt.Printf("热词    %v\n", cfg.Hotwords)
	}
	if len(cfg.Contexts) > 0 {
		fmt.Printf("上下文  %v\n", cfg.Contexts)
	}
	fmt.Printf("判停    %d ms（ddc=%v）\n", endWin, ddc)
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

// splitList 与 config.SplitList 同规则（这里不 import config，避免 CLI 依赖应用层）。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', '，', '、', ';', '；', '|', '｜', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// runAirvoice 探活局域网识别服务（GET /health）。
func runAirvoice(url, token string, timeout time.Duration) {
	fmt.Printf("后端    airvoice（局域网整段识别）\n地址    %s\n", url)
	fmt.Print("探活…")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	err := airvoice.CheckHealth(ctx, airvoice.Config{BaseURL: url, Token: token})
	elapsed := time.Since(start)
	if err != nil {
		fmt.Println(" 失败")
		fmt.Fprintf(os.Stderr, "\n%v\n\n", err)
		fmt.Fprintln(os.Stderr, "排查：")
		fmt.Fprintln(os.Stderr, "  同一 WiFi？手机 App 里「局域网识别 API」开关打开了吗？")
		fmt.Fprintln(os.Stderr, "  地址是手机通知栏里显示的那个 host:port（只填地址，不带 http:// 与路径）")
		fmt.Fprintln(os.Stderr, "  Token 与 App 里设置的一致吗？")
		os.Exit(1)
	}
	fmt.Printf(" 就绪（%d ms）\n", elapsed.Milliseconds())
	fmt.Println("\n提示：整段识别没有候选字，说话期间的音频会攒在内存里，松手后一次性识别。")
}
