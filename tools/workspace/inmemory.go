package workspace

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// InMemoryBackend 内存版 Backend(单元测试 / 本地开发 / sandbox 用)。
//
// 线程安全。Read-before-Write 通过 readPaths 内部 set 维护:
// 在同一 Backend 实例生命周期内,Read 标记 path 已读,Write/Edit 已存在文件前必须已读。
type InMemoryBackend struct {
	mu        sync.RWMutex
	files     map[string]*memFile
	readPaths map[string]struct{} // 本实例生命周期内已 Read 过的 path
}

type memFile struct {
	Content    string
	ModifiedAt time.Time
}

// NewInMemoryBackend 构造空 backend。
func NewInMemoryBackend() *InMemoryBackend {
	return &InMemoryBackend{
		files:     map[string]*memFile{},
		readPaths: map[string]struct{}{},
	}
}

// Preload 灌入一组初始文件(测试用 / 跨 backend 恢复 state 用)。
// 不更新 readPaths。
func (b *InMemoryBackend) Preload(files map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for path, content := range files {
		b.files[path] = &memFile{Content: content, ModifiedAt: now}
	}
}

// Ls 列出 dir 下直接子项(虚拟目录 + 文件)。
func (b *InMemoryBackend) Ls(_ context.Context, dir string) ([]FileInfo, error) {
	dir, err := ValidateDirPath(dir)
	if err != nil {
		return nil, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	allPaths := make([]string, 0, len(b.files))
	for p := range b.files {
		allPaths = append(allPaths, p)
	}
	subDirs, subFiles := listVirtualDirs(dir, allPaths)

	out := make([]FileInfo, 0, len(subDirs)+len(subFiles))
	for _, d := range subDirs {
		out = append(out, FileInfo{Path: d, IsDir: true})
	}
	for _, f := range subFiles {
		mf := b.files[f]
		if mf == nil {
			continue // 不变量上不会发生:f 来自 listVirtualDirs 返回的存在文件列表
		}
		out = append(out, FileInfo{
			Path:       f,
			IsDir:      false,
			Size:       int64(len(mf.Content)),
			ModifiedAt: mf.ModifiedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// 目录排前
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Read 读文件,offset 1-based,limit=0 = 全文(受 DefaultReadLimit 兜底)。
// 同时把 path 加入 readPaths 满足 Read-before-Write 约束。
func (b *InMemoryBackend) Read(_ context.Context, path string, offset, limit int) (string, error) {
	path, err := ValidatePath(path)
	if err != nil {
		return "", err
	}

	b.mu.Lock() // 写 readPaths 要写锁
	defer b.mu.Unlock()

	mf, ok := b.files[path]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrFileNotFound, path)
	}
	b.readPaths[path] = struct{}{}

	return sliceLines(mf.Content, offset, limit), nil
}

// Write 创建或覆盖。已存在文件必须先 Read。
func (b *InMemoryBackend) Write(_ context.Context, path, content string) error {
	path, err := ValidatePath(path)
	if err != nil {
		return err
	}
	if len(content) > MaxFileBytes {
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrFileTooLarge, len(content), MaxFileBytes)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.files[path]; exists {
		if _, read := b.readPaths[path]; !read {
			return fmt.Errorf("%w: %s (call read_file first)", ErrMustReadFirst, path)
		}
	}
	if _, exists := b.files[path]; !exists {
		// 新建:检查文件数上限
		if len(b.files) >= MaxWorkspaceFiles {
			return fmt.Errorf("%w: %d (limit %d)", ErrTooManyFiles, len(b.files), MaxWorkspaceFiles)
		}
	}
	b.files[path] = &memFile{Content: content, ModifiedAt: time.Now()}
	// Write 自动标记已读(之后的 Edit 不需要重新 Read)
	b.readPaths[path] = struct{}{}
	return nil
}

// Edit 字符串替换。
func (b *InMemoryBackend) Edit(_ context.Context, path, oldString, newString string, replaceAll bool) (int, error) {
	path, err := ValidatePath(path)
	if err != nil {
		return 0, err
	}
	if oldString == "" {
		return 0, ErrEmptyOldString
	}
	if oldString == newString {
		return 0, ErrSameOldAndNew
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	mf, ok := b.files[path]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrFileNotFound, path)
	}
	if _, read := b.readPaths[path]; !read {
		return 0, fmt.Errorf("%w: %s (call read_file first)", ErrMustReadFirst, path)
	}

	count := strings.Count(mf.Content, oldString)
	if count == 0 {
		return 0, ErrEditNoMatch
	}
	if count > 1 && !replaceAll {
		return 0, fmt.Errorf("%w: %d occurrences", ErrEditAmbiguous, count)
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(mf.Content, oldString, newString)
	} else {
		newContent = strings.Replace(mf.Content, oldString, newString, 1)
		count = 1
	}
	if len(newContent) > MaxFileBytes {
		return 0, fmt.Errorf("%w: %d bytes after edit (limit %d)", ErrFileTooLarge, len(newContent), MaxFileBytes)
	}
	b.files[path] = &memFile{Content: newContent, ModifiedAt: time.Now()}
	return count, nil
}

// sliceLines 把 content 按 \n 拆,从 offset(1-based)开始取 limit 行。
// limit=0 走 DefaultReadLimit 兜底防 LLM context 爆炸。
func sliceLines(content string, offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	lines := strings.Split(content, "\n")
	start := offset - 1
	if start >= len(lines) {
		return ""
	}
	end := min(start+limit, len(lines))
	return strings.Join(lines[start:end], "\n")
}
