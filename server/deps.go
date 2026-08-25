package server

import (
	"github.com/dernate/gopcxmlda-server/backend"
	"github.com/dernate/gopcxmlda-server/clock"
	"github.com/dernate/gopcxmlda-server/telemetry"
)

// Deps are the dependencies a Handler/Server needs. Clock, Logger, and
// Metrics may be left zero; they default to clock.Real{},
// telemetry.NoopLogger(), and telemetry.NoopMetrics() respectively.
type Deps struct {
	Backend backend.Backend
	Clock   clock.Clock
	Logger  telemetry.Logger
	Metrics telemetry.Metrics
}
