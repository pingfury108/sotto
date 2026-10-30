package proto

import "bytes"

// LineReassembler 把 EVENT 通道的通知分片重组成整行。
//
// 事件行以 '\n' 结尾、可能跨通知；未成行的尾部留在缓冲等下一片。缓冲超过
// MaxReassembleLine（远大于 MaxEventLine=512）视为失步，清空重新对齐，防内存
// 无限增长。与官方 relay.reassemble_event 语义一致。
type LineReassembler struct {
	buf []byte
	max int
}

// NewLineReassembler 返回空缓冲的重组器；max ≤ 0 时用 MaxReassembleLine。
func NewLineReassembler(max int) *LineReassembler {
	if max <= 0 {
		max = MaxReassembleLine
	}
	return &LineReassembler{max: max}
}

// Feed 吃一段通知载荷，返回其中已完整的行（不含 '\n'）。
func (l *LineReassembler) Feed(chunk []byte) [][]byte {
	l.buf = append(l.buf, chunk...)
	var lines [][]byte
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := make([]byte, i)
		copy(line, l.buf[:i])
		lines = append(lines, line)
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) == 0 {
		l.buf = nil // 归还底层数组
	} else if len(l.buf) > l.max {
		l.buf = nil // 失步：清空等下一片重新对齐
	}
	return lines
}

// Pending 返回尚未成行的字节数（诊断用）。
func (l *LineReassembler) Pending() int { return len(l.buf) }
