package loomfs

import (
	"context"
	"time"
)

func now() time.Time { return time.Now() }

func nilContext() context.Context { return context.Background() }
