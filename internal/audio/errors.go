package audio

import "errors"

var (
	// ErrShortBlock 传入的块/载荷长度不足。
	ErrShortBlock = errors.New("audio: block too short")
	// ErrBadSamples 请求的样本数越界。
	ErrBadSamples = errors.New("audio: samples out of range")
)
