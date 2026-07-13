package gobash

import "sync"

// limitedBuffer 是带上限的输出缓冲:累计写入到 cap 字节后丢弃剩余,并置 truncated。
// 只挂在最外层 stdout/stderr——管道中间的输出不能截断,否则 `grep | head` 这类
// 下游命令看不到完整上游,语义被破坏。
type limitedBuffer struct {
	cap       uint64
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func newLimitedBuffer(cap uint64) *limitedBuffer {
	return &limitedBuffer{cap: cap}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 始终返回完整长度:对上游而言写入"成功",只是我们内部丢弃超限部分,
	// 避免命令把短写当成 I/O 错误而提前中断。
	if b.cap == 0 || uint64(len(b.buf)) >= b.cap {
		if uint64(len(b.buf)) >= b.cap && b.cap != 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	room := b.cap - uint64(len(b.buf))
	if uint64(len(p)) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	b.truncated = true
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
