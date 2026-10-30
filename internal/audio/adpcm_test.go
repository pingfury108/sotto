package audio

import (
	"encoding/hex"
	"math"
	"testing"
)

// 共享向量：与固件 tests/test_adpcm.c 和官方客户端 companion/tests/test_adpcm.py
// 逐字节一致。任一端改动编码/表/位序，这个用例即断——这是"无缝"最硬的保障。
var sharedSamples = []int16{
	0, 1000, 2000, 4000, 8000, 4000, 0, -4000,
	-8000, -4000, 0, 100, 50, 0, -50, -100,
}

var sharedDecoded = []int16{
	0, 11, 41, 104, 240, 533, -14, -1134,
	-3537, -3880, 180, -373, 130, -327, 88, -290,
}

func TestSharedVectorEncode(t *testing.T) {
	st := &State{}
	got := hex.EncodeToString(EncodeBlock(st, sharedSamples))
	const want = "000000007777e7ff68080808"
	if got != want {
		t.Fatalf("编码共享向量不一致\n got %s\nwant %s", got, want)
	}
}

func TestSharedVectorDecode(t *testing.T) {
	st := &State{}
	block := EncodeBlock(st, sharedSamples)
	got, err := DecodeBlockSamples(block, len(sharedSamples))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	for i, want := range sharedDecoded {
		if got[i] != want {
			t.Fatalf("第 %d 个样本 %d，want %d（全部: %v）", i, got[i], want, got)
		}
	}
	if got[0] != sharedSamples[0] {
		t.Fatalf("首样本必须精确: %d != %d", got[0], sharedSamples[0])
	}
}

// makeSignal 生成语音状测试信号（基频 + 二次谐波 + 缓变包络），
// 与固件/官方客户端的 make_signal 同参，便于跨实现比较 SNR。
func makeSignal(n, seed int) []int16 {
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		tt := float64(i+seed*n) / 16000.0
		env := 0.55 + 0.45*math.Sin(2*math.Pi*3.0*tt)
		v := 0.62*math.Sin(2*math.Pi*220.0*tt) +
			0.28*math.Sin(2*math.Pi*440.0*tt) +
			0.10*math.Sin(2*math.Pi*1750.0*tt)
		out[i] = int16(env * v * 11000.0)
	}
	return out
}

func snrDB(ref, got []int16) float64 {
	var sig, errAcc float64
	for i := range ref {
		s := float64(ref[i])
		e := s - float64(got[i])
		sig += s * s
		errAcc += e * e
	}
	if errAcc == 0 {
		return 999
	}
	return 10 * math.Log10(sig/errAcc)
}

func TestRoundtripSNR(t *testing.T) {
	pcm := makeSignal(ADPCMBlockSamples, 0)
	st := &State{}
	block := EncodeBlock(st, pcm)
	if len(block) != ADPCMBlockBytes {
		t.Fatalf("块长 %d，want %d", len(block), ADPCMBlockBytes)
	}
	got, err := DecodeBlock(block)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if snr := snrDB(pcm, got); snr < 18.0 {
		t.Fatalf("往返 SNR %.1f dB < 18 dB：说明表/位序/重建公式错了", snr)
	}
}

// 块独立可解：每块头自带 predictor/index，丢任意块不影响其后各块的解码结果。
func TestBlockIndependence(t *testing.T) {
	const blocks = 5
	st := &State{}
	encoded := make([][]byte, blocks)
	decodedAlone := make([][]int16, blocks)
	for i := 0; i < blocks; i++ {
		pcm := makeSignal(ADPCMBlockSamples, i)
		encoded[i] = EncodeBlock(st, pcm) // 编码端 index 跨块延续（音质）
		got, err := DecodeBlock(encoded[i])
		if err != nil {
			t.Fatalf("第 %d 块解码失败: %v", i, err)
		}
		decodedAlone[i] = got
	}
	// 重新按任意顺序独立解码，结果必须与单块解码逐字节一致。
	for _, i := range []int{4, 2, 0, 3, 1} {
		got, err := DecodeBlock(encoded[i])
		if err != nil {
			t.Fatalf("第 %d 块重复解码失败: %v", i, err)
		}
		for j := range got {
			if got[j] != decodedAlone[i][j] {
				t.Fatalf("第 %d 块第 %d 样本不稳定: %d != %d", i, j, got[j], decodedAlone[i][j])
			}
		}
	}
}

func TestDecodeBounds(t *testing.T) {
	if _, err := DecodeBlock(make([]byte, ADPCMBlockBytes-1)); err == nil {
		t.Fatal("短块应当报错")
	}
	if _, err := DecodeBlockSamples(make([]byte, ADPCMBlockBytes), 0); err == nil {
		t.Fatal("samples=0 应当报错")
	}
	// 越界 index（> 88）夹紧而不是越表读。
	blk := make([]byte, ADPCMBlockBytes)
	blk[2] = 200
	if _, err := DecodeBlock(blk); err != nil {
		t.Fatalf("越界 index 不应失败: %v", err)
	}
}

func TestPCMFrame(t *testing.T) {
	samples := []int16{1, -1, 0x1234, -32768}
	got := PCMFrame(samples)
	want := []byte{0x01, 0x00, 0xFF, 0xFF, 0x34, 0x12, 0x00, 0x80}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PCM 帧字节序错误: %x != %x", got, want)
		}
	}
}
