// Command injectcheck 手工验证注入链路：授权状态 → 聚焦元素探测 → 注入 → 落地校验。
//
//	go run ./cmd/injectcheck                     # 注入默认文本
//	go run ./cmd/injectcheck -text "你好"        # 指定文本
//	go run ./cmd/injectcheck -mode clipboard     # 强制走剪贴板
//	go run ./cmd/injectcheck -probe              # 只探测，不注入
//
// 主要用来看"看起来注入成功其实没进字"这一类问题：
// 如果 AX 探测显示 role 是 AXTextField/AXTextArea 且字数在注入后变了，就是真落地。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"sotto/internal/inject"
)

func main() {
	text := flag.String("text", "你好，这是 sotto 的注入测试。", "要注入的文本")
	mode := flag.String("mode", inject.ModeAuto, "注入方式：auto / unicode / clipboard")
	probeOnly := flag.Bool("probe", false, "只探测聚焦元素，不注入")
	flag.Parse()

	fmt.Printf("辅助功能授权：%v\n", inject.Trusted())
	p := inject.ProbeAX()
	fmt.Printf("前台 app：%q\n聚焦元素 role：%q\n聚焦元素字数：%d（可信=%v）\n",
		p.App, p.Role, p.Chars, p.Verifiable())
	if !inject.Trusted() {
		fmt.Fprintln(os.Stderr, "\n未授权：系统设置 → 隐私与安全性 → 辅助功能，把运行本命令的终端加进去。")
		os.Exit(2)
	}
	if *probeOnly {
		return
	}

	fmt.Printf("\n注入 %d 字符（mode=%s）…\n", len([]rune(*text)), *mode)
	start := time.Now()
	used, err := inject.Paste(*text, *mode)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "注入失败（%d ms）：%v\n", elapsed.Milliseconds(), err)
		os.Exit(1)
	}
	after := inject.ProbeAX()
	fmt.Printf("注入成功，走的是 %s（%d ms）\n注入后字数：%d（之前 %d）\n",
		used, elapsed.Milliseconds(), after.Chars, p.Chars)
}
