package gobash

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/itchyny/gojq"
)

// cmdJq 用 gojq 跑 jq filter。支持 -r(raw 字符串)、-c(紧凑)、-n(null 输入)。
// 输入文件既支持单个 JSON 也支持 NDJSON(.jsonl):用 json.Decoder 逐值解码后逐个喂 filter,
// 这是 workspace 里 queries.jsonl / sources.jsonl / index.jsonl 的主力路径。
// 不支持 --arg / --slurpfile / -f(从文件读 filter)。
func cmdJq(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	raw := anySet(set, "r", "raw-output")
	compact := anySet(set, "c", "compact-output")
	nullInput := anySet(set, "n", "null-input")

	if len(pos) == 0 {
		io.WriteString(env.Stderr, "jq: 缺少 filter\n")
		return exitUsageError
	}
	queryStr := pos[0]
	files := pos[1:]

	query, err := gojq.Parse(queryStr)
	if err != nil {
		io.WriteString(env.Stderr, "jq: 解析 filter 失败: "+err.Error()+"\n")
		return exitUsageError
	}
	code, err := gojq.Compile(query)
	if err != nil {
		io.WriteString(env.Stderr, "jq: 编译 filter 失败: "+err.Error()+"\n")
		return exitUsageError
	}

	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()

	emit := func(input any) int {
		iter := code.RunWithContext(ctx, input)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if e, isErr := v.(error); isErr {
				if haltErr, isHalt := e.(*gojq.HaltError); isHalt && haltErr.Value() == nil {
					break
				}
				w.Flush()
				io.WriteString(env.Stderr, "jq: "+e.Error()+"\n")
				return exitGenericError
			}
			if err := writeJqValue(w, v, raw, compact); err != nil {
				w.Flush()
				return ioErrExit(env, "jq", err)
			}
		}
		return exitOK
	}

	if nullInput {
		return emit(nil)
	}

	readers, closers, hadErr := openInputs(env, files)
	defer closeAll(closers)
	for _, nr := range readers {
		data, e := readCapped(nr.r)
		if e != nil {
			w.Flush()
			return ioErrExit(env, "jq", e)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			if err := ctx.Err(); err != nil {
				return ioErrExit(env, "jq", err)
			}
			var input any
			if e := dec.Decode(&input); e != nil {
				if e == io.EOF {
					break
				}
				w.Flush()
				io.WriteString(env.Stderr, "jq: 解析 JSON 失败: "+e.Error()+"\n")
				return exitGenericError
			}
			if c := emit(input); c != exitOK {
				return c
			}
		}
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

// writeJqValue 按 jq 输出约定写一个结果值。-r 且为字符串则裸输出;否则 JSON(默认缩进 2,-c 紧凑)。
func writeJqValue(w io.Writer, v any, raw, compact bool) error {
	if raw {
		if s, ok := v.(string); ok {
			_, err := io.WriteString(w, s+"\n")
			return err
		}
	}
	var out []byte
	var err error
	if compact {
		out, err = gojq.Marshal(v)
	} else {
		out, err = marshalIndent(v)
	}
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

func marshalIndent(v any) ([]byte, error) {
	// gojq.Marshal 保证与 jq 一致的数字/排序语义;再用 json.Indent 美化。
	compact, err := gojq.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return compact, nil //nolint:nilerr // 缩进失败退回紧凑输出,不丢内容
	}
	return buf.Bytes(), nil
}

// readCapped 读全量但限 maxReadBytes,超限返回 errReadLimit(jq 需整体解析,内存受此软闸约束)。
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReadBytes {
		return nil, errReadLimit
	}
	return data, nil
}
