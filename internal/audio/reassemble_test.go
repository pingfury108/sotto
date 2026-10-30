package audio

import (
	"bytes"
	"testing"
)

// testBody 对应 MTU 517：body = mtu-3-2 = 512，804B 一块切成 512+292 两片。
const testBody = 512

// chunkify 按固件 ble_audio_notify_audio() 的规则切分一块：body = mtu-3-2，
// 每片带 [seq][idx|0x80 LAST] 片头。body 由测试指定，便于复现 517/185 等 MTU。
func chunkify(seq uint8, block []byte, body int) [][]byte {
	var out [][]byte
	for off, idx := 0, 0; off < len(block); idx++ {
		n := body
		if len(block)-off < n {
			n = len(block) - off
		}
		last := off+n >= len(block)
		hdr := []byte{seq, byte(idx & ChunkIndexMask)}
		if last {
			hdr[1] |= ChunkLastFlag
		}
		out = append(out, append(hdr, block[off:off+n]...))
		off += n
	}
	return out
}

// testBlock 造一块可解码的 ADPCM（语音状信号编码）。
func testBlock(t *testing.T, seed int) []byte {
	t.Helper()
	pcm := makeSignal(ADPCMBlockSamples, seed)
	block := EncodeBlock(&State{}, pcm)
	if len(block) != ADPCMBlockBytes {
		t.Fatalf("块长 %d，want %d", len(block), ADPCMBlockBytes)
	}
	return block
}

func wantFrame(t *testing.T, block []byte) []byte {
	t.Helper()
	samples, err := DecodeBlock(block)
	if err != nil {
		t.Fatalf("参考解码失败: %v", err)
	}
	return PCMFrame(samples)
}

func TestReassembleSingleBlockTwoChunks(t *testing.T) {
	block := testBlock(t, 0)
	chunks := chunkify(0, block, testBody) // 804B → 2 片（MTU 517）
	if len(chunks) != 2 {
		t.Fatalf("片数 %d，want 2", len(chunks))
	}
	r := NewReassembler()
	var frames [][]byte
	for _, c := range chunks {
		frames = append(frames, r.Feed(c)...)
	}
	if len(frames) != 1 {
		t.Fatalf("帧数 %d，want 1（miss=%d）", len(frames), r.Miss())
	}
	if !bytes.Equal(frames[0], wantFrame(t, block)) {
		t.Fatal("解码结果与参考不一致")
	}
	if r.Unit() != testBody+ChunkHeaderBytes {
		t.Fatalf("学到的满片长度 %d，want %d", r.Unit(), testBody+ChunkHeaderBytes)
	}
}

// CoreBluetooth 会把连续多条通知合并进一次回调：拆开后必须与逐片喂入结果一致。
func TestReassembleMergedNotify(t *testing.T) {
	block := testBlock(t, 1)
	chunks := chunkify(0, block, testBody)

	separate := NewReassembler()
	var want [][]byte
	for _, c := range chunks {
		want = append(want, separate.Feed(c)...)
	}
	if len(want) != 1 {
		t.Fatalf("逐片参考帧数 %d，want 1", len(want))
	}

	// 先喂一片干净满片学到 unit（真机上第一条回调通常是单片）。
	merged := NewReassembler()
	if frames := merged.Feed(chunks[0]); len(frames) != 0 {
		t.Fatalf("首片不应产帧，得到 %d", len(frames))
	}
	got := merged.Feed(chunks[1])
	if len(got) != 1 {
		t.Fatalf("合并载荷帧数 %d，want 1（miss=%d）", len(got), merged.Miss())
	}
	if !bytes.Equal(got[0], want[0]) {
		t.Fatal("合并通知拆分后解码结果与逐片不一致")
	}
}

// 多块合并进一次回调：末片按"该块还缺多少 + 2B 片头"定界，并在切口校验下一块片头。
func TestReassembleMergedTwoBlocks(t *testing.T) {
	b0, b1 := testBlock(t, 0), testBlock(t, 1)
	c0 := chunkify(0, b0, testBody) // 2 片
	c1 := chunkify(1, b1, testBody) // 2 片

	r := NewReassembler()
	if frames := r.Feed(c0[0]); len(frames) != 0 {
		t.Fatalf("首片不应产帧，得到 %d", len(frames))
	}
	payload := bytes.Join([][]byte{c0[1], c1[0], c1[1]}, nil)
	frames := r.Feed(payload)
	if len(frames) != 2 {
		t.Fatalf("帧数 %d，want 2（miss=%d）", len(frames), r.Miss())
	}
	if !bytes.Equal(frames[0], wantFrame(t, b0)) {
		t.Fatal("第 0 块解码不一致")
	}
	if !bytes.Equal(frames[1], wantFrame(t, b1)) {
		t.Fatal("第 1 块解码不一致")
	}
}

func TestReassembleDuplicateAndLateChunks(t *testing.T) {
	block := testBlock(t, 2)
	chunks := chunkify(0, block, testBody)

	r := NewReassembler()
	r.Feed(chunks[0])
	r.Feed(chunks[0]) // 重复片：丢弃
	frames := r.Feed(chunks[1])
	if len(frames) != 1 {
		t.Fatalf("帧数 %d，want 1", len(frames))
	}
	if got := r.Feed(chunks[1]); len(got) != 0 { // 已终结块的迟到片：丢弃
		t.Fatalf("迟到片不应产帧，得到 %d", len(got))
	}
	if r.Miss() != 0 {
		t.Fatalf("miss %d，want 0", r.Miss())
	}
}

// 新 seq 时前块缺末片：补零终结出静音帧保住 100ms 节拍，并记 miss。
func TestReassemblePadOnMissingTail(t *testing.T) {
	b0, b1 := testBlock(t, 0), testBlock(t, 1)
	c0 := chunkify(0, b0, testBody)
	c1 := chunkify(1, b1, testBody)

	r := NewReassembler()
	r.Feed(c0[0]) // 只有前半块
	// 新 seq 到达：前块补零终结出静音帧，同时开新块。
	frames := r.Feed(c1[0])
	frames = append(frames, r.Feed(c1[1])...)
	if len(frames) != 2 {
		t.Fatalf("帧数 %d，want 2（补零帧 + 正常帧，miss=%d）", len(frames), r.Miss())
	}
	if r.Miss() != 1 {
		t.Fatalf("miss %d，want 1", r.Miss())
	}
}

// 超过 804B 的块：协议失步，丢块并重对齐（不硬解）。
func TestReassembleOversizedBlock(t *testing.T) {
	r := NewReassembler()
	bogus := make([]byte, ChunkHeaderBytes+ADPCMBlockBytes+8)
	bogus[1] = ChunkLastFlag
	if frames := r.Feed(bogus); len(frames) != 0 {
		t.Fatalf("超长块不应产帧，得到 %d", len(frames))
	}
	if r.Miss() != 1 {
		t.Fatalf("miss %d，want 1", r.Miss())
	}
}

// 碎片（不足片头）记 miss 丢弃。
func TestReassembleTinyChunk(t *testing.T) {
	r := NewReassembler()
	if frames := r.Feed([]byte{0x01}); len(frames) != 0 {
		t.Fatalf("碎片不应产帧，得到 %d", len(frames))
	}
	if r.Miss() != 1 {
		t.Fatalf("miss %d，want 1", r.Miss())
	}
}

// 会话切换复位：块状态清零，但保留了连接级学到的 unit。
func TestResetKeepsUnit(t *testing.T) {
	block := testBlock(t, 0)
	r := NewReassembler()
	for _, c := range chunkify(3, block, testBody) {
		r.Feed(c)
	}
	if r.Unit() != testBody+ChunkHeaderBytes {
		t.Fatalf("unit %d，want %d", r.Unit(), testBody+ChunkHeaderBytes)
	}
	r.Reset(true)
	if r.Unit() != testBody+ChunkHeaderBytes {
		t.Fatalf("Reset(keepUnit=true) 后 unit %d，want %d", r.Unit(), testBody+ChunkHeaderBytes)
	}
	if r.Miss() != 0 || r.hasSeq || r.hasLastSeq {
		t.Fatal("Reset 后块状态未清零")
	}
	// 序号从 0 重新开始的第一个块必须正常解出（不被当成迟到片丢掉）。
	frames := [][]byte{}
	for _, c := range chunkify(0, block, testBody) {
		frames = append(frames, r.Feed(c)...)
	}
	if len(frames) != 1 {
		t.Fatalf("新会话首块帧数 %d，want 1", len(frames))
	}
	r.Reset(false)
	if r.Unit() != 0 {
		t.Fatalf("Reset(keepUnit=false) 后 unit %d，want 0", r.Unit())
	}
}
