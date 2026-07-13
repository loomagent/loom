// Package gettime provides a current-time tool with a fixed Beijing timezone.
package gettime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/loomagent/loom"
)

// ToolName is the name exposed to models.
const ToolName = "get_time"

// Response is the JSON result returned by the tool.
type Response struct {
	UTCTime      string `json:"utcTime"`
	LocalTime    string `json:"localTime"`
	Timezone     string `json:"timezone"`
	LocalWeekday string `json:"localWeekday"`
}

// New constructs a time tool fixed to Asia/Shanghai (UTC+08:00). The tool has
// no arguments so a model cannot silently change the application's timezone.
func New() loom.Tool {
	description := "Get the current date and time. Local time is fixed to Asia/Shanghai (UTC+08:00). Returns UTC and local time in ISO 8601 format plus the weekday in English and Chinese."
	return loom.NewTool(ToolName, description, nil, invoke)
}

func invoke(ctx context.Context, _ string) (string, error) {
	_, span := otel.Tracer("github.com/loomagent/loom/tools/gettime").Start(ctx, "get_time.now")
	defer span.End()

	response := At(time.Now())
	out, err := json.Marshal(response)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("gettime: marshal result: %w", err)
	}
	span.SetStatus(codes.Ok, "")
	return string(out), nil
}

// At formats a point in time using the tool's fixed Beijing timezone. It is
// exported for deterministic rendering and tests; New always uses time.Now.
func At(now time.Time) Response {
	localTime := now.In(beijing)
	weekday := localTime.Weekday()
	return Response{
		UTCTime:      now.UTC().Format(time.RFC3339),
		LocalTime:    localTime.Format(time.RFC3339),
		Timezone:     "Asia/Shanghai",
		LocalWeekday: fmt.Sprintf("%s (%s)", weekday.String(), weekdayCN[weekday]),
	}
}

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

var weekdayCN = map[time.Weekday]string{
	time.Sunday:    "星期日",
	time.Monday:    "星期一",
	time.Tuesday:   "星期二",
	time.Wednesday: "星期三",
	time.Thursday:  "星期四",
	time.Friday:    "星期五",
	time.Saturday:  "星期六",
}
