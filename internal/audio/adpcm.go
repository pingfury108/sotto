// Package audio 是音频通路的纯逻辑：IMA ADPCM 解码与 BLE 分片重组。
//
// 契约来源：固件 main/adpcm.c、main/ble_audio.c 与官方客户端
// companion/adpcm.py、companion/relay.py。所有表、位序、重建公式与状态机
// 必须逐字节对齐——对拍见 adpcm_test.go 的共享向量与 reassemble_test.go。
//
// 链路事实（不可按"更合理"的方式改）：
//   - 一块 = 100ms = 1600 样本 = 804B：4B 头（int16 LE predictor、uint8 index、
//     uint8 rsv） + 800B 半字节流；块独立可解，丢一块只损失 100ms。
//   - 每条 ATT 通知载荷 = 2B 片头（块序号、片序号|0x80 末片） + 数据。
//   - CoreBluetooth 会把连续多条通知合并进一次回调，必须先拆开。
package audio

const (
	// ADPCMHeaderBytes 块头：[int16 LE predictor][uint8 index][uint8 rsv=0]。
	ADPCMHeaderBytes = 4
	// ADPCMBlockSamples 一块的样本数（100ms @ 16kHz）。
	ADPCMBlockSamples = 1600
	// ADPCMBlockBytes 一块的字节数（4 + 1600/2）。
	ADPCMBlockBytes = ADPCMHeaderBytes + ADPCMBlockSamples/2
	// PCMFrameBytes 一帧解码后的 16bit PCM 字节数（100ms @ 16kHz 单声道）。
	PCMFrameBytes = ADPCMBlockSamples * 2

	// ChunkHeaderBytes 通知分片的片头字节数。
	ChunkHeaderBytes = 2
	// ChunkLastFlag 片序号字节的末片标志位。
	ChunkLastFlag = 0x80
	// ChunkIndexMask 片序号的掩码。
	ChunkIndexMask = 0x7F
)

// stepTable IMA ADPCM 标准步长表（与固件、WAV、libsndfile 一致）。
var stepTable = [89]int32{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17,
	19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118,
	130, 143, 157, 173, 190, 209, 230, 253, 279, 307,
	337, 371, 408, 449, 494, 544, 598, 658, 724, 796,
	876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
	2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358,
	5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899,
	15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

// indexDelta 每个 4bit code 对应的 index 增量。
var indexDelta = [16]int32{
	-1, -1, -1, -1, 2, 4, 6, 8,
	-1, -1, -1, -1, 2, 4, 6, 8,
}

const (
	indexMax = 88
	s16Max   = 32767
	s16Min   = -32768
)

// State 是编解码共享的自适应状态（predictor + index）。
// 解码时每块由块头重新初始化，跨块不延续；编码时跨块延续可提升音质。
type State struct {
	Predictor int32
	Index     int32
}

func clampIndex(i int32) int32 {
	if i < 0 {
		return 0
	}
	if i > indexMax {
		return indexMax
	}
	return i
}

func clampS16(v int32) int32 {
	if v > s16Max {
		return s16Max
	}
	if v < s16Min {
		return s16Min
	}
	return v
}

// decodeSample 解一个 4bit code，并就地推进状态。
func (s *State) decodeSample(code uint8) int16 {
	step := stepTable[s.Index]
	diff := step >> 3
	if code&4 != 0 {
		diff += step
	}
	if code&2 != 0 {
		diff += step >> 1
	}
	if code&1 != 0 {
		diff += step >> 2
	}
	if code&8 != 0 {
		s.Predictor -= diff
	} else {
		s.Predictor += diff
	}
	s.Predictor = clampS16(s.Predictor)
	s.Index = clampIndex(s.Index + indexDelta[code])
	return int16(s.Predictor)
}

// encodeSample 编一个样本，返回 4bit code 并就地推进状态。
// 必须走与 decodeSample 相同的重建公式，否则两端 predictor 分叉、失真累积。
func (s *State) encodeSample(sample int16) uint8 {
	step := stepTable[s.Index]
	diff := int32(sample) - s.Predictor
	var code uint8
	if diff < 0 {
		code = 8
		diff = -diff
	}
	if diff >= step {
		code |= 4
		diff -= step
	}
	half := step >> 1
	if diff >= half {
		code |= 2
		diff -= half
	}
	quarter := half >> 1
	if diff >= quarter {
		code |= 1
	}
	s.decodeSample(code) // 用同一份重建推进状态
	return code
}

// DecodeBlockSamples 解一个块的前 samples 个样本（samples ≤ 1600）。
// 无需外部状态：predictor/index 来自块头，越界 index 夹紧。
func DecodeBlockSamples(data []byte, samples int) ([]int16, error) {
	if samples <= 0 || samples > ADPCMBlockSamples {
		return nil, ErrBadSamples
	}
	need := ADPCMHeaderBytes + samples/2
	if len(data) < need {
		return nil, ErrShortBlock
	}
	s := State{
		Predictor: int32(int16(uint16(data[0]) | uint16(data[1])<<8)),
		Index:     clampIndex(int32(data[2])),
	}
	out := make([]int16, samples)
	out[0] = int16(s.Predictor)
	r := ADPCMHeaderBytes
	for i := 1; i < samples; i++ {
		var code uint8
		if i&1 == 1 {
			code = data[r] & 0x0F
		} else {
			code = (data[r] >> 4) & 0x0F
			r++
		}
		out[i] = s.decodeSample(code)
	}
	return out, nil
}

// DecodeBlock 解一个完整块（1600 样本）。
func DecodeBlock(data []byte) ([]int16, error) {
	return DecodeBlockSamples(data, ADPCMBlockSamples)
}

// EncodeBlock 把 pcm 编成一个块（首样本进块头，数据区从第 2 个样本起）。
// 仅用于测试与对拍：上行压缩在固件侧。
func EncodeBlock(s *State, pcm []int16) []byte {
	n := len(pcm)
	if n == 0 {
		return nil
	}
	first := pcm[0]
	s.Predictor = clampS16(int32(first))
	out := make([]byte, ADPCMHeaderBytes+n/2)
	out[0] = byte(uint16(first) & 0xFF)
	out[1] = byte((uint16(first) >> 8) & 0xFF)
	out[2] = byte(s.Index)
	out[3] = 0
	w := ADPCMHeaderBytes
	for i := 1; i < n; i++ {
		code := s.encodeSample(pcm[i])
		if i&1 == 1 {
			out[w] = code & 0x0F
			if i+1 == n {
				w++ // 收尾：高半字节留 0
			}
		} else {
			out[w] |= (code << 4) & 0xF0
			w++
		}
	}
	return out
}

// PCMFrame 把 1600 个样本打成 3200B 的 16bit LE 单声道 PCM 帧。
func PCMFrame(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		u := uint16(v)
		out[2*i] = byte(u & 0xFF)
		out[2*i+1] = byte(u >> 8)
	}
	return out
}
