package loomfs

import (
	"fmt"
	"strings"
)

func RenderContextBlock(snapshot ContextSnapshot, opts RenderOptions) string {
	if len(snapshot.PriorTurns) == 0 && len(snapshot.Queries) == 0 && len(snapshot.Sources) == 0 {
		return ""
	}
	if opts.MaxPriorTurns == 0 {
		opts.MaxPriorTurns = 4
	}
	if opts.MaxQueries == 0 {
		opts.MaxQueries = 12
	}
	if opts.MaxSources == 0 {
		opts.MaxSources = 16
	}

	var b strings.Builder
	b.WriteString("【对话工作区】\n这些资料来自同一 conversation 的历史执行过程,可作为本轮上下文参考。\n")

	fmt.Fprintf(&b, "\n历史轮次(共 %d 轮):\n", len(snapshot.PriorTurns))
	if len(snapshot.PriorTurns) == 0 {
		b.WriteString("(暂无)\n")
	} else {
		start := tailStart(len(snapshot.PriorTurns), opts.MaxPriorTurns)
		if start > 0 {
			fmt.Fprintf(&b, "(仅列最近 %d 轮;更早 %d 轮可查 prior_turns.jsonl)\n", len(snapshot.PriorTurns)-start, start)
		}
		for _, t := range snapshot.PriorTurns[start:] {
			fmt.Fprintf(&b, "- turn=%d executor=%s mode=%s user=%s answer=%s\n",
				t.TurnIndex, t.Executor, t.ChatModeID, truncate(t.UserText, 100), truncate(t.FinalAnswer, 180))
		}
	}

	fmt.Fprintf(&b, "\n历史搜索(共 %d 条):\n", len(snapshot.Queries))
	if len(snapshot.Queries) == 0 {
		b.WriteString("(暂无)\n")
	} else {
		start := tailStart(len(snapshot.Queries), opts.MaxQueries)
		if start > 0 {
			fmt.Fprintf(&b, "(仅列最近 %d 条;更早 %d 条可查 queries.jsonl)\n", len(snapshot.Queries)-start, start)
		}
		for _, q := range snapshot.Queries[start:] {
			fmt.Fprintf(&b, "- %s turn=%d saved=%d query=%s\n", q.QueryID, q.TurnIndex, q.NumSaved, truncate(q.Text, 140))
		}
	}

	fmt.Fprintf(&b, "\n已保存信源(共 %d 源;raw 共 %d 篇):\n", len(snapshot.Sources), snapshot.Stats.RawCount)
	if len(snapshot.Sources) == 0 {
		b.WriteString("(暂无)\n")
		return b.String()
	}
	start := tailStart(len(snapshot.Sources), opts.MaxSources)
	if start > 0 {
		fmt.Fprintf(&b, "(仅列最近 %d 源;更早 %d 源可查 sources.jsonl)\n", len(snapshot.Sources)-start, start)
	}
	for _, s := range snapshot.Sources[start:] {
		fmt.Fprintf(&b, "- %s | %s | %s | %s | %s | %s\n", s.ID, s.Tier, s.Domain, SourceDateLabel(s), s.RawPath, truncate(s.Summary, 180))
	}
	return b.String()
}

func tailStart(length int, max uint64) int {
	if max == 0 || uint64(length) <= max {
		return 0
	}
	return length - int(max)
}
