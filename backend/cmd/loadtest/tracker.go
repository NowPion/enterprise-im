package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ExpectedMessage struct {
	MsgID       uint64
	ConvID      uint64
	Seq         uint64
	SenderID    uint64
	ClientMsgID string
	Recipients  []uint64
	ConvType    string
}

type PushDelivery struct {
	ReceiverUserID uint64
	ConvID         uint64
	MsgID          uint64
	Seq            uint64
	SenderID       uint64
	ClientMsgID    string
	ReceivedAt     time.Time
}

type LatencyStats struct {
	Count int
	Avg   time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

type TrackerSummary struct {
	TotalSentAttempts        int
	AckedMessages            int
	SingleMessagesAcked      int
	GroupMessagesAcked       int
	AckRetries               int64
	AckFailures              int
	AckSeqCollisions         int
	WSErrorFrames            int64
	ExpectedDeliveries       int
	ActualUniqueDeliveries   int
	MissingDeliveries        int
	DuplicateDeliveries      int
	OutOfOrderCount          int
	CheckedClientConvStreams int
	LossRate                 float64
	DuplicateRate            float64
	HeartbeatPings           int64
	HeartbeatPongs           int64
	HeartbeatErrors          int64
	ConnectLatency           LatencyStats
	PingLatency              LatencyStats
	AckLatency               LatencyStats
	DeliveryLatency          LatencyStats
	SampleFailures           []string
}

type clientConvKey struct {
	userID uint64
	convID uint64
}

type convSeqKey struct {
	convID uint64
	seq    uint64
}

type clientMsgKey struct {
	userID uint64
	msgID  uint64
}

type senderClientMsgKey struct {
	senderID    uint64
	clientMsgID string
}

type clientConvStream struct {
	lastSeq uint64
	seenMsg map[uint64]struct{}
}

type Tracker struct {
	mu sync.Mutex

	sendStartTimes    map[senderClientMsgKey]time.Time
	expectedByMsgID   map[uint64]ExpectedMessage
	expectedConvSeq   map[convSeqKey]uint64
	expectedDelivSum  int
	singleMsgAcked    int
	groupMsgAcked     int
	ackSeqCollisions  int
	ackFailures       []string

	streams          map[clientConvKey]*clientConvStream
	deliveredSet     map[clientMsgKey]struct{}
	duplicateDelivs  int
	outOfOrderCount  int
	ackFailureSamples []string
	dupSamples        []string
	oooSamples        []string
	missingSamples    []string

	connectLatencies  []time.Duration
	pingLatencies     []time.Duration
	ackLatencies      []time.Duration
	deliveryLatencies []time.Duration

	totalPushDeliveries atomic.Int64
	ackRetries          atomic.Int64
	wsErrorFrames       atomic.Int64
	heartbeatPings      atomic.Int64
	heartbeatPongs      atomic.Int64
	heartbeatErrors     atomic.Int64
}

func NewTracker() *Tracker {
	return &Tracker{
		sendStartTimes:  make(map[senderClientMsgKey]time.Time),
		expectedByMsgID: make(map[uint64]ExpectedMessage),
		expectedConvSeq: make(map[convSeqKey]uint64),
		streams:         make(map[clientConvKey]*clientConvStream),
		deliveredSet:    make(map[clientMsgKey]struct{}),
	}
}

func (t *Tracker) RecordConnectLatency(d time.Duration) {
	t.mu.Lock()
	t.connectLatencies = append(t.connectLatencies, d)
	t.mu.Unlock()
}

func (t *Tracker) RecordHeartbeatPing() {
	t.heartbeatPings.Add(1)
}

func (t *Tracker) RecordHeartbeatPong(d time.Duration) {
	t.heartbeatPongs.Add(1)
	t.mu.Lock()
	t.pingLatencies = append(t.pingLatencies, d)
	t.mu.Unlock()
}

func (t *Tracker) RecordHeartbeatError() {
	t.heartbeatErrors.Add(1)
}

func (t *Tracker) RecordWSErrorFrame() {
	t.wsErrorFrames.Add(1)
}

func (t *Tracker) AddAckRetries(n int) {
	t.ackRetries.Add(int64(n))
}

func (t *Tracker) RecordAckFailure(detail string) {
	t.mu.Lock()
	t.ackFailures = append(t.ackFailures, detail)
	if len(t.ackFailureSamples) < 10 {
		t.ackFailureSamples = append(t.ackFailureSamples, "ack_failure: "+detail)
	}
	t.mu.Unlock()
}

func (t *Tracker) RecordSendStart(senderID uint64, clientMsgID string, at time.Time) {
	t.mu.Lock()
	key := senderClientMsgKey{senderID: senderID, clientMsgID: clientMsgID}
	if _, exists := t.sendStartTimes[key]; !exists {
		t.sendStartTimes[key] = at
	}
	t.mu.Unlock()
}

func (t *Tracker) RecordAckLatency(d time.Duration) {
	t.mu.Lock()
	t.ackLatencies = append(t.ackLatencies, d)
	t.mu.Unlock()
}

func (t *Tracker) RegisterExpectedMessage(exp ExpectedMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if prev, exists := t.expectedByMsgID[exp.MsgID]; exists && prev.ClientMsgID != exp.ClientMsgID {
		t.ackSeqCollisions++
		if len(t.dupSamples) < 10 {
			t.dupSamples = append(t.dupSamples, fmt.Sprintf("duplicate msg_id=%d between client_msg_id %s and %s", exp.MsgID, prev.ClientMsgID, exp.ClientMsgID))
		}
		return
	}
	csk := convSeqKey{convID: exp.ConvID, seq: exp.Seq}
	if prevMsgID, exists := t.expectedConvSeq[csk]; exists && prevMsgID != exp.MsgID {
		t.ackSeqCollisions++
		if len(t.dupSamples) < 10 {
			t.dupSamples = append(t.dupSamples, fmt.Sprintf("conv=%d seq=%d collision msg_ids %d vs %d", exp.ConvID, exp.Seq, prevMsgID, exp.MsgID))
		}
	} else {
		t.expectedConvSeq[csk] = exp.MsgID
	}

	if _, exists := t.expectedByMsgID[exp.MsgID]; !exists {
		t.expectedByMsgID[exp.MsgID] = exp
		t.expectedDelivSum += len(exp.Recipients)
		if exp.ConvType == "single" {
			t.singleMsgAcked++
		} else if exp.ConvType == "group" {
			t.groupMsgAcked++
		}
	}
}

func (t *Tracker) RecordPushDelivery(d PushDelivery) {
	t.totalPushDeliveries.Add(1)
	t.mu.Lock()
	defer t.mu.Unlock()

	if sentAt, ok := t.sendStartTimes[senderClientMsgKey{senderID: d.SenderID, clientMsgID: d.ClientMsgID}]; ok {
		if lat := d.ReceivedAt.Sub(sentAt); lat >= 0 {
			t.deliveryLatencies = append(t.deliveryLatencies, lat)
		}
	}

	ccKey := clientConvKey{userID: d.ReceiverUserID, convID: d.ConvID}
	st := t.streams[ccKey]
	if st == nil {
		st = &clientConvStream{seenMsg: make(map[uint64]struct{})}
		t.streams[ccKey] = st
	}

	if _, dup := st.seenMsg[d.MsgID]; dup {
		t.duplicateDelivs++
		if len(t.dupSamples) < 10 {
			t.dupSamples = append(t.dupSamples, fmt.Sprintf("duplicate push user=%d conv=%d msg_id=%d seq=%d", d.ReceiverUserID, d.ConvID, d.MsgID, d.Seq))
		}
		return
	}
	st.seenMsg[d.MsgID] = struct{}{}

	if st.lastSeq > 0 && d.Seq <= st.lastSeq {
		t.outOfOrderCount++
		if len(t.oooSamples) < 10 {
			t.oooSamples = append(t.oooSamples, fmt.Sprintf("out_of_order push user=%d conv=%d prev_seq=%d curr_seq=%d msg_id=%d",
				d.ReceiverUserID, d.ConvID, st.lastSeq, d.Seq, d.MsgID))
		}
	}
	if d.Seq > st.lastSeq {
		st.lastSeq = d.Seq
	}

	t.deliveredSet[clientMsgKey{userID: d.ReceiverUserID, msgID: d.MsgID}] = struct{}{}
}

func (t *Tracker) TotalPushDeliveries() int64 {
	return t.totalPushDeliveries.Load()
}

func (t *Tracker) WaitForExpectedDeliveries(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		missing := t.countMissing()
		if missing == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (t *Tracker) countMissing() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	missing := 0
	for msgID, exp := range t.expectedByMsgID {
		for _, uid := range exp.Recipients {
			if _, ok := t.deliveredSet[clientMsgKey{userID: uid, msgID: msgID}]; !ok {
				missing++
			}
		}
	}
	return missing
}

func (t *Tracker) Summarize() TrackerSummary {
	t.mu.Lock()
	defer t.mu.Unlock()

	missing := 0
	actualUnique := 0
	for msgID, exp := range t.expectedByMsgID {
		for _, uid := range exp.Recipients {
			if _, ok := t.deliveredSet[clientMsgKey{userID: uid, msgID: msgID}]; ok {
				actualUnique++
			} else {
				missing++
				if len(t.missingSamples) < 10 {
					t.missingSamples = append(t.missingSamples, fmt.Sprintf("missing delivery user=%d conv=%d msg_id=%d seq=%d",
						uid, exp.ConvID, msgID, exp.Seq))
				}
			}
		}
	}

	ackedCount := len(t.expectedByMsgID)
	totalAttempts := ackedCount + len(t.ackFailures)
	var lossRate, dupRate float64
	if t.expectedDelivSum > 0 {
		lossRate = float64(missing) / float64(t.expectedDelivSum)
		dupRate = float64(t.duplicateDelivs) / float64(t.expectedDelivSum)
	}

	samples := make([]string, 0, len(t.ackFailureSamples)+len(t.dupSamples)+len(t.oooSamples)+len(t.missingSamples))
	samples = append(samples, t.oooSamples...)
	samples = append(samples, t.dupSamples...)
	samples = append(samples, t.missingSamples...)
	samples = append(samples, t.ackFailureSamples...)

	return TrackerSummary{
		TotalSentAttempts:        totalAttempts,
		AckedMessages:            ackedCount,
		SingleMessagesAcked:      t.singleMsgAcked,
		GroupMessagesAcked:       t.groupMsgAcked,
		AckRetries:               t.ackRetries.Load(),
		AckFailures:              len(t.ackFailures),
		AckSeqCollisions:         t.ackSeqCollisions,
		WSErrorFrames:            t.wsErrorFrames.Load(),
		ExpectedDeliveries:       t.expectedDelivSum,
		ActualUniqueDeliveries:   actualUnique,
		MissingDeliveries:        missing,
		DuplicateDeliveries:      t.duplicateDelivs,
		OutOfOrderCount:          t.outOfOrderCount,
		CheckedClientConvStreams: len(t.streams),
		LossRate:                 lossRate,
		DuplicateRate:            dupRate,
		HeartbeatPings:           t.heartbeatPings.Load(),
		HeartbeatPongs:           t.heartbeatPongs.Load(),
		HeartbeatErrors:          t.heartbeatErrors.Load(),
		ConnectLatency:           computeLatencyStats(t.connectLatencies),
		PingLatency:              computeLatencyStats(t.pingLatencies),
		AckLatency:               computeLatencyStats(t.ackLatencies),
		DeliveryLatency:          computeLatencyStats(t.deliveryLatencies),
		SampleFailures:           samples,
	}
}

func (t *Tracker) DuplicateCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.duplicateDelivs
}
