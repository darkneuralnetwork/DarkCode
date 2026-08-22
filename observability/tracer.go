package observability

import (
	"context"
	"time"
)

type TraceSpan struct {
	ID        string
	Name      string
	StartTime time.Time
	EndTime   time.Time
}

func StartSpan(ctx context.Context, name string) (context.Context, *TraceSpan) {
	// Dummy trace implementation
	span := &TraceSpan{
		Name:      name,
		StartTime: time.Now(),
	}
	return ctx, span
}

func (s *TraceSpan) End() {
	s.EndTime = time.Now()
}
