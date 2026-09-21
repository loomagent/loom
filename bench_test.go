package loom

import "testing"

// The contract decode and the handle reads sit on the path of every tool call, so their
// cost is worth a number. In particular the declared-name check on Get and Present has to
// stay cheaper than the work it guards.

func BenchmarkArgsContractDecode(b *testing.B) {
	contract := MustArgsContract("web_search",
		String("query").Required().MinLen(1).MaxLen(200).Desc("Query."),
		Enum("type", "search", "news").Desc("Type."),
		Date("date_from").Desc("Lower bound."),
		Date("date_to").Desc("Upper bound."),
		Uint("limit").Max(20).Desc("Limit."),
	)
	raw := `{"query":"golang generics","type":"search","date_from":"2026-08-17","date_to":"2026-08-18","limit":10}`
	b.ReportAllocs()
	for b.Loop() {
		if _, err := contract.Decode(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkArgRead(b *testing.B) {
	query := String("query").Required().Desc("Query.")
	limit := Uint("limit").Desc("Limit.")
	contract := MustArgsContract("t", query, limit)
	args, err := contract.Decode(`{"query":"golang generics","limit":10}`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if query.Get(args) == "" || !query.Present(args) || limit.Get(args) != 10 {
			b.Fatal("read failed")
		}
	}
}
