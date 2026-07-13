package workspacebash

// Result 是一条 workspace shell 命令的执行结果。中性类型,不绑定任何具体
// 执行后端(进程内 gobash / 历史上的 agent-sandbox daemon),供 Runner 契约共用。
type Result struct {
	Stdout          string
	Stderr          string
	ExitCode        uint64
	StdoutTruncated bool
	StderrTruncated bool
	// TimedOut: 命令命中墙钟超时被终止。Stdout/Stderr 是终止前的部分输出,
	// ExitCode 约定为 124(对齐 coreutils timeout)。
	TimedOut bool
}
