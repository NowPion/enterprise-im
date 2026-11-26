package snowflake

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// CustomEpochMillis is 2026-01-01T00:00:00.000Z in Unix milliseconds.
	CustomEpochMillis int64 = 1767225600000

	nodeBits     = 10
	sequenceBits = 12

	MaxNodeID   = (1 << nodeBits) - 1     // 1023
	maxSequence = (1 << sequenceBits) - 1 // 4095

	nodeShift      = sequenceBits
	timestampShift = nodeBits + sequenceBits

	maxClockRollbackWaitMs = 5
)

var (
	// ErrInvalidNodeID is returned when nodeID is outside [0, MaxNodeID].
	ErrInvalidNodeID = errors.New("snowflake node id out of range")
	// ErrClockRollback is returned when system clock moves backward beyond tolerance.
	ErrClockRollback = errors.New("snowflake clock moved backwards")
)

// Generator produces globally unique, monotonically trending 64-bit Snowflake IDs.
type Generator struct {
	mu            sync.Mutex
	nodeID        int64
	lastTimestamp int64
	sequence      int64
	nowMillis     func() int64
}

// NewGenerator constructs a Generator bound to a unique nodeID in [0, 1023].
func NewGenerator(nodeID int64) (*Generator, error) {
	if nodeID < 0 || nodeID > MaxNodeID {
		return nil, fmt.Errorf("%w: got %d, want [0, %d]", ErrInvalidNodeID, nodeID, MaxNodeID)
	}
	return &Generator{
		nodeID:    nodeID,
		nowMillis: func() int64 { return time.Now().UnixMilli() },
	}, nil
}

// NextID generates the next unique positive uint64 Snowflake identifier.
// Small backward clock drifts (<= 5ms) wait for the clock to catch up; larger drifts fail fast
// so previously issued IDs are never reused.
func (g *Generator) NextID() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.nowMillis()
	if now < g.lastTimestamp {
		drift := g.lastTimestamp - now
		if drift > maxClockRollbackWaitMs {
			return 0, fmt.Errorf("%w: %dms behind last timestamp", ErrClockRollback, drift)
		}
		now = g.waitUntil(g.lastTimestamp)
		if now < g.lastTimestamp {
			return 0, fmt.Errorf("%w: clock remained behind after wait", ErrClockRollback)
		}
	}

	if now == g.lastTimestamp {
		g.sequence = (g.sequence + 1) & maxSequence
		if g.sequence == 0 {
			now = g.waitUntil(g.lastTimestamp + 1)
			if now <= g.lastTimestamp {
				return 0, fmt.Errorf("%w: clock did not advance after sequence exhaustion", ErrClockRollback)
			}
		}
	} else {
		g.sequence = 0
	}

	if now < CustomEpochMillis {
		return 0, fmt.Errorf("%w: timestamp %d precedes epoch %d", ErrClockRollback, now, CustomEpochMillis)
	}

	g.lastTimestamp = now
	id := ((now - CustomEpochMillis) << timestampShift) | (g.nodeID << nodeShift) | g.sequence
	return uint64(id), nil
}

func (g *Generator) waitUntil(targetMs int64) int64 {
	now := g.nowMillis()
	for attempt := 0; now < targetMs && attempt < 50; attempt++ {
		time.Sleep(time.Millisecond)
		now = g.nowMillis()
	}
	return now
}
