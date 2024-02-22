package metrics

import (
	"time"

	"github.com/relab/hotstuff"
	"github.com/relab/hotstuff/eventloop"
	"github.com/relab/hotstuff/logging"
	"github.com/relab/hotstuff/metrics/types"
	"github.com/relab/hotstuff/modules"
)

func init() {
	RegisterReplicaMetric("qclength", func() any {
		return &QCLength{}
	})
}

// QCLength measures average QC length
type QCLength struct {
	metricsLogger      Logger
	opts               *modules.Options
	cumulativeQCLength uint64
	commits            uint64
}

// InitModule gives the module access to the other modules.
func (q *QCLength) InitModule(mods *modules.Core) {
	var (
		eventLoop *eventloop.EventLoop
		logger    logging.Logger
	)

	mods.Get(
		&q.metricsLogger,
		&q.opts,
		&eventLoop,
		&logger,
	)

	eventLoop.RegisterHandler(hotstuff.QCCreateEvent{}, func(event any) {
		qcEvent := event.(hotstuff.QCCreateEvent)
		q.recordQC(qcEvent.QCLength)
	})

	eventLoop.RegisterObserver(types.TickEvent{}, func(event any) {
		q.tick(event.(types.TickEvent))
	})

	logger.Info("QCLength metric enabled")
}

func (q *QCLength) recordQC(qcLength int) {
	q.commits++
	q.cumulativeQCLength += uint64(qcLength)
}

func (q *QCLength) tick(_ types.TickEvent) {
	if q.commits == 0 {
		return
	}
	now := time.Now()
	event := &types.QCLength{
		Event:       types.NewReplicaEvent(uint32(q.opts.ID()), now),
		AvgQCLength: q.cumulativeQCLength / q.commits,
	}
	q.metricsLogger.Log(event)
	// reset count for next tick
	q.commits = 0
	q.cumulativeQCLength = 0
}
