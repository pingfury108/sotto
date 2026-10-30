package audio

// Reassembler 把 AUDIO 通知分片还原成 3200B PCM 帧。
//
// 逐条对齐官方实现 companion/relay.py 的 split_merged_notify /
// reassemble_adpcm / _finalize_adpcm_block：
//
//	同 seq，idx 递增        → 追加
//	同 seq，idx ≤ 已收最大  → 重复/乱序，丢弃
//	新 seq，前块未终结      → 前块补零终结（静音帧 + miss），开新块
//	seq == 已终结块的 seq   → 迟到片，丢弃
//	载荷 < 2B               → 记 miss，丢弃
//
// 另外负责拆开 CoreBluetooth 合并进一次回调的多条通知：不拆会把第二个分片的
// 片头当数据，整块解错、后续全部错位（真机取证，官方为此单独实现了 unit 学习）。
type Reassembler struct {
	seq        uint8
	hasSeq     bool
	buf        []byte
	lastIdx    int
	lastSeq    uint8
	hasLastSeq bool

	miss    int
	unit    int
	hasUnit bool
}

// NewReassembler 返回空状态的重组器。
func NewReassembler() *Reassembler {
	return &Reassembler{lastIdx: -1}
}

// Reset 复位块状态。keepUnit 保留学到的满片长度——它由连接协商的 MTU 决定，
// 是连接级常量，跨会话保留能免得每次会话开头重新学。
func (r *Reassembler) Reset(keepUnit bool) {
	r.seq, r.hasSeq = 0, false
	r.buf = nil
	r.lastIdx = -1
	r.lastSeq, r.hasLastSeq = 0, false
	r.miss = 0
	if !keepUnit {
		r.unit, r.hasUnit = 0, false
	}
}

// Miss 是本会话丢弃的块/碎片数（对账用）。
func (r *Reassembler) Miss() int { return r.miss }

// Unit 是学到的满片载荷长度，0 表示尚未学到。
func (r *Reassembler) Unit() int {
	if !r.hasUnit {
		return 0
	}
	return r.unit
}

// Feed 吃一段 ATT 通知载荷（2B 片头 + 块数据），返回解出的 PCM 帧。
func (r *Reassembler) Feed(chunk []byte) [][]byte {
	pieces := r.split(chunk)
	if len(pieces) > 1 {
		var out [][]byte
		for _, piece := range pieces {
			out = append(out, r.Feed(piece)...)
		}
		return out
	}
	return r.feedOne(chunk)
}

// split 拆开一次回调里合并的多条通知。
//
// 定界依据全部来自固件 ble_audio_notify_audio() 的分片规则：一次连接内
// body = mtu-3-2 恒定（非末片长度恒为 unit = body+2）；末片长度 = 该块还缺的
// 数据 + 2B 片头；合并只是整片拼接，不会切断单片。对不上时整段交给下游，
// 绝不硬拆出错位的片。
func (r *Reassembler) split(payload []byte) [][]byte {
	n := len(payload)
	if n < ChunkHeaderBytes {
		return [][]byte{payload}
	}
	if payload[1]&ChunkLastFlag == 0 {
		// 满片必然不带 LAST：非末片载荷的最小长度收敛到真实 unit。
		if !r.hasUnit || n < r.unit {
			r.unit, r.hasUnit = n, true
		}
	}
	unit := r.unit
	if !r.hasUnit {
		unit = ChunkHeaderBytes + ADPCMBlockBytes // 一片装得下整块的硬上限
	}
	if n <= unit {
		return [][]byte{payload}
	}

	pieces := make([][]byte, 0, 4)
	off := 0
	curSeq, hasCurSeq := r.seq, r.hasSeq
	acc := 0
	if hasCurSeq {
		acc = len(r.buf)
	}
	for off+ChunkHeaderBytes <= n {
		seq := payload[off]
		if !hasCurSeq || seq != curSeq {
			curSeq, hasCurSeq, acc = seq, true, 0 // 片头换块：重新计数
		}
		if payload[off+1]&ChunkLastFlag != 0 {
			want := lastFragmentLen(payload, off, seq, acc)
			pieces = append(pieces, payload[off:off+want])
			off += want
			hasCurSeq, acc = false, 0 // 块已终结
			continue
		}
		end := off + unit
		if end > n {
			end = n
		}
		pieces = append(pieces, payload[off:end])
		off += unit
		acc += unit - ChunkHeaderBytes
	}
	if off < n {
		pieces = append(pieces, payload[off:]) // 尾部不足片头：下游记 miss
	}
	return pieces
}

// lastFragmentLen 是合并载荷里一个末片（0x80）的长度。
//
// 首选"该块还缺多少 + 2B 片头"这个账面值；但块内丢过片时账面会算大，所以用
// "切口后面必须是下一块的第 0 片"来校验，校验不过就往后找第一个对得上的切口；
// 都找不到就把剩余整段当一片。
func lastFragmentLen(payload []byte, off int, seq uint8, acc int) int {
	n := len(payload)
	nxt := seq + 1
	ok := func(cut int) bool {
		if cut == n {
			return true
		}
		if cut+ChunkHeaderBytes > n {
			return false
		}
		return payload[cut] == nxt && payload[cut+1]&ChunkIndexMask == 0
	}
	want := ADPCMBlockBytes - acc + ChunkHeaderBytes
	if want > ChunkHeaderBytes && off+want <= n && ok(off+want) {
		return want
	}
	for cut := off + ChunkHeaderBytes + 1; cut < n; cut++ {
		if ok(cut) {
			return cut - off
		}
	}
	return n - off
}

// feedOne 处理单片载荷。
func (r *Reassembler) feedOne(chunk []byte) [][]byte {
	if len(chunk) < ChunkHeaderBytes {
		r.miss++
		return nil
	}
	seq := chunk[0]
	idx := int(chunk[1] & ChunkIndexMask)
	last := chunk[1]&ChunkLastFlag != 0
	data := chunk[ChunkHeaderBytes:]

	var out [][]byte
	switch {
	case r.hasSeq && seq == r.seq:
		if idx <= r.lastIdx {
			return nil // 重复/乱序片
		}
		r.buf = append(r.buf, data...)
		r.lastIdx = idx
	case r.hasLastSeq && seq == r.lastSeq:
		return nil // 已终结块的迟到片
	default:
		// 新块：前块未终结（缺末片标志）→ 补零终结保节拍，再开新块
		if r.hasSeq && len(r.buf) > 0 {
			out = append(out, r.finalize(true)...)
		}
		r.seq, r.hasSeq = seq, true
		r.buf = append(r.buf[:0], data...)
		r.lastIdx = idx
	}
	if last {
		out = append(out, r.finalize(false)...)
	}
	return out
}

// finalize 终结当前块：校验长度 → 解码成 3200B PCM。
//
//	804B        → 全量解码
//	> 804B      → 协议失步，清空末尾序号等新 seq 重新对齐，记 miss
//	4..803B     → 丢片：记 miss；pad 时补零解出静音帧保住 100ms 节拍
//	< 4B        → 碎片，记 miss 丢弃
func (r *Reassembler) finalize(pad bool) [][]byte {
	payload := r.buf
	seq := r.seq
	r.seq, r.hasSeq = 0, false
	r.lastSeq, r.hasLastSeq = seq, true // 终结后该 seq 的迟到片一律丢弃
	r.buf = nil
	r.lastIdx = -1

	if len(payload) == ADPCMBlockBytes {
		if frame := decodeFrame(payload); frame != nil {
			return [][]byte{frame}
		}
		r.miss++
		return nil
	}
	r.miss++
	if len(payload) > ADPCMBlockBytes {
		r.hasLastSeq = false // 失步：不可能的正常块，重对齐
		return nil
	}
	if pad && len(payload) >= ADPCMHeaderBytes {
		padded := make([]byte, ADPCMBlockBytes)
		copy(padded, payload)
		if frame := decodeFrame(padded); frame != nil {
			return [][]byte{frame}
		}
	}
	return nil
}

func decodeFrame(block []byte) []byte {
	samples, err := DecodeBlock(block)
	if err != nil || len(samples) != ADPCMBlockSamples {
		return nil
	}
	return PCMFrame(samples)
}
