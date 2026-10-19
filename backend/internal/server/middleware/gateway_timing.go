package middleware

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestbudget"
	"github.com/gin-gonic/gin"
)

const gatewayStageOffsetsKey = "gateway_stage_offsets_ms"

// MarkGatewayStage records cumulative offsets from HTTP ingress, not model
// latency. Include them in the existing access record for successes as well as
// failures, without extra per-stage log writes or a database schema change.
func MarkGatewayStage(c *gin.Context, stage string) {
	if c == nil || c.Request == nil {
		return
	}
	if _, enabled := requestbudget.Deadline(c.Request.Context()); !enabled {
		return
	}
	started, ok := c.Request.Context().Value(ctxkey.RequestStartedAt).(time.Time)
	if !ok {
		return
	}
	value, _ := c.Get(gatewayStageOffsetsKey)
	offsets, _ := value.(map[string]int64)
	if offsets == nil {
		offsets = make(map[string]int64, 8)
		c.Set(gatewayStageOffsetsKey, offsets)
	}
	if _, recorded := offsets[stage]; !recorded {
		offsets[stage] = time.Since(started).Milliseconds()
	}
}
