// Package config 持久化 sotto 的设置。
//
// 数据目录是 sotto 自己的（~/Library/Application Support/sotto），不读写官方
// 客户端的 AI Passport 目录，避免互相改坏。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"sotto/internal/asr"
)

// 识别后端。
const (
	BackendVolcano  = "volcano"  // 火山云端流式（有候选字）
	BackendAirvoice = "airvoice" // 空音局域网整段识别（零出站、无候选字）
)

// 注入方式。
const (
	InjectAuto      = "auto"      // 先键盘事件，失败回退剪贴板
	InjectUnicode   = "unicode"   // CGEvent 逐字符键盘事件，不碰剪贴板
	InjectClipboard = "clipboard" // pbcopy + Cmd+V 粘贴
)

// Config 是用户可以调整的全部设置。
type Config struct {
	DeviceName  string `json:"deviceName"`
	AutoConnect bool   `json:"autoConnect"`

	VolcanoAPIKey     string `json:"volcanoApiKey"`
	VolcanoWSURL      string `json:"volcanoWsUrl"`
	VolcanoResourceID string `json:"volcanoResourceId"`
	VolcanoModel      string `json:"volcanoModel"`
	VolcanoNonstream  bool   `json:"volcanoEnableNonstream"`

	// 识别后端与局域网（空音）参数。
	ASRBackend    string `json:"asrBackend"`    // volcano | airvoice
	AirvoiceURL   string `json:"airvoiceUrl"`   // host:port，如 192.168.32.145:8787
	AirvoiceToken string `json:"airvoiceToken"` // 留空 = 服务端未开鉴权

	// 识别调优：热词 / 对话上下文 / 静音判停阈值（ms）/ 语义顺滑。
	VolcanoHotwords  string `json:"volcanoHotwords"`    // 逗号、顿号等分隔
	VolcanoContexts  string `json:"volcanoContexts"`    // 竖线或换行分隔
	VolcanoEndWindow int    `json:"volcanoEndWindowMs"` // 0 用服务端默认
	VolcanoDDC       bool   `json:"volcanoEnableDDC"`

	InjectMode   string `json:"injectMode"`
	InjectSample string `json:"injectSample"`

	ShowFloating bool `json:"showFloating"`
}

// Default 返回出厂设置。
func Default() Config {
	return Config{
		DeviceName:        "AI Passport",
		AutoConnect:       true,
		VolcanoWSURL:      asr.DefaultWSURL,
		VolcanoResourceID: asr.DefaultResourceID,
		VolcanoModel:      asr.DefaultModel,
		VolcanoNonstream:  true,
		VolcanoEndWindow:  800, // 服务端默认值，显式写出来让界面能显示"默认"
		ASRBackend:        BackendVolcano,
		InjectMode:        InjectAuto,
		InjectSample:      "你好，这是 sotto 的注入测试。",
		ShowFloating:      true,
	}
}

// ASR 把设置转成 ASR 会话参数。
func (c Config) ASR() asr.Config {
	return asr.Config{
		APIKey:          c.VolcanoAPIKey,
		WSURL:           c.VolcanoWSURL,
		ResourceID:      c.VolcanoResourceID,
		Model:           c.VolcanoModel,
		EnableNonStream: c.VolcanoNonstream,
		Hotwords:        SplitList(c.VolcanoHotwords),
		Contexts:        SplitList(c.VolcanoContexts),
		EndWindowSize:   c.VolcanoEndWindow,
		EnableDDC:       c.VolcanoDDC,
	}
}

// SplitList 把用户输入的列表文本拆成条目：逗号、顿号、分号以及竖线/换行都算分隔符。
// 汉字的全角标点一并处理，免得用户切输入法后被当成一个词。
func SplitList(s string) []string {
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

// Dir 返回 sotto 的数据目录（不存在时创建）。
func Dir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "sotto")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Store 并发安全地读写 config.json。
type Store struct {
	mu   sync.Mutex
	path string
	c    Config
}

// Load 从 dir/config.json 读取；文件不存在时用默认值。
func Load(dir string) *Store {
	st := &Store{path: filepath.Join(dir, "config.json"), c: Default()}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return st
	}
	// 缺字段用默认值补齐：老配置文件/手工编辑都能用。
	merged := Default()
	if err := json.Unmarshal(data, &merged); err != nil {
		return st
	}
	st.c = merged
	return st
}

// Get 返回设置快照。
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// Update 修改并落盘。
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.c)
	data, err := json.MarshalIndent(s.c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, append(data, '\n'), 0o644)
}

// Path 返回配置文件路径（诊断页显示用）。
func (s *Store) Path() string { return s.path }
