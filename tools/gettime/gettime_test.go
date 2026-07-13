package gettime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAt(t *testing.T) {
	got := At(time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC))
	if got.UTCTime != "2026-07-13T01:02:03Z" {
		t.Fatalf("UTCTime = %q", got.UTCTime)
	}
	if got.LocalTime != "2026-07-13T09:02:03+08:00" {
		t.Fatalf("LocalTime = %q", got.LocalTime)
	}
	if got.Timezone != "Asia/Shanghai" || got.LocalWeekday != "Monday (星期一)" {
		t.Fatalf("response = %+v", got)
	}
}

func TestToolIgnoresTimezoneArgument(t *testing.T) {
	out, err := New().Invoke(context.Background(), `{"timezone":"UTC"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q", got.Timezone)
	}
}
