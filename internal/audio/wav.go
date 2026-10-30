package audio

import (
	"bufio"
	"encoding/binary"
	"os"
)

// WAVWriter 把 16kHz/16bit/单声道 PCM 帧落成 WAV 文件（诊断/对拍用）。
// 头部长度在 Close 时回填，因此流式写入不需要预先知道总长。
type WAVWriter struct {
	f      *os.File
	w      *bufio.Writer
	onDisk int64 // 已写入的数据字节数
	closed bool
}

// NewWAVWriter 创建文件并写好占位头。
func NewWAVWriter(path string, sampleRate uint32) (*WAVWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &WAVWriter{f: f, w: bufio.NewWriterSize(f, 64*1024)}
	if err := w.writeHeader(sampleRate); err != nil {
		f.Close()
		return nil, err
	}
	return w, nil
}

const wavHeaderBytes = 44

func (w *WAVWriter) writeHeader(sampleRate uint32) error {
	h := make([]byte, wavHeaderBytes)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], 0) // 回填：36 + data
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // fmt 块长度
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:24], 1)  // 单声道
	binary.LittleEndian.PutUint32(h[24:28], sampleRate)
	binary.LittleEndian.PutUint32(h[28:32], sampleRate*2) // 字节率
	binary.LittleEndian.PutUint16(h[32:34], 2)            // 块对齐
	binary.LittleEndian.PutUint16(h[34:36], 16)           // 位深
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], 0) // 回填：数据字节数
	_, err := w.w.Write(h)
	return err
}

// Write 追加一帧 PCM。
func (w *WAVWriter) Write(pcm []byte) error {
	if w.closed {
		return os.ErrClosed
	}
	n, err := w.w.Write(pcm)
	w.onDisk += int64(n)
	return err
}

// Close 回填头长度并落盘。
func (w *WAVWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.w.Flush(); err != nil {
		w.f.Close()
		return err
	}
	// RIFF 长度在 offset 4，data 长度在 offset 40；取 32 位小端。
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(36+w.onDisk))
	if _, err := w.f.WriteAt(size[:], 4); err != nil {
		w.f.Close()
		return err
	}
	binary.LittleEndian.PutUint32(size[:], uint32(w.onDisk))
	if _, err := w.f.WriteAt(size[:], 40); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}
