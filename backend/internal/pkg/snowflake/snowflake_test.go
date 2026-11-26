package snowflake

import (
	"errors"
	"sync"
	"testing"
)

func TestGeneratorConcurrencyAndUniqueness(t *testing.T) {
	gen, err := NewGenerator(1)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	const workers = 32
	const perWorker = 500
	results := make(chan uint64, workers*perWorker)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id, genErr := gen.NextID()
				if genErr != nil {
					t.Errorf("NextID failed: %v", genErr)
					return
				}
				if id == 0 {
					t.Error("NextID returned zero")
					return
				}
				results <- id
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[uint64]struct{}, workers*perWorker)
	for id := range results {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate snowflake id generated: %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("expected %d unique IDs, got %d", workers*perWorker, len(seen))
	}
}

func TestGeneratorClockRollbackRejected(t *testing.T) {
	gen, err := NewGenerator(3)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	now := CustomEpochMillis + 10000
	gen.nowMillis = func() int64 { return now }

	first, err := gen.NextID()
	if err != nil {
		t.Fatalf("first NextID failed: %v", err)
	}
	if first == 0 {
		t.Fatal("expected non-zero id")
	}

	// Simulate severe clock rollback (> 5ms tolerance)
	now = CustomEpochMillis + 9000
	if _, err := gen.NextID(); !errors.Is(err, ErrClockRollback) {
		t.Fatalf("expected ErrClockRollback, got %v", err)
	}
}

func TestNewGeneratorNodeValidation(t *testing.T) {
	if _, err := NewGenerator(-1); !errors.Is(err, ErrInvalidNodeID) {
		t.Fatalf("expected ErrInvalidNodeID for -1, got %v", err)
	}
	if _, err := NewGenerator(MaxNodeID + 1); !errors.Is(err, ErrInvalidNodeID) {
		t.Fatalf("expected ErrInvalidNodeID for %d, got %v", MaxNodeID+1, err)
	}
}

func TestGeneratorSmallClockRollbackWaitsAndNeverReusesID(t *testing.T) {
	gen, err := NewGenerator(7)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	base := CustomEpochMillis + 20000
	calls := 0
	gen.nowMillis = func() int64 {
		calls++
		switch calls {
		case 1:
			return base
		case 2:
			// Simulate 3ms backward clock drift (within <=5ms wait tolerance).
			return base - 3
		default:
			// Clock catches up back to base.
			return base
		}
	}

	first, err := gen.NextID()
	if err != nil {
		t.Fatalf("first NextID failed: %v", err)
	}
	second, err := gen.NextID()
	if err != nil {
		t.Fatalf("second NextID after small drift catch-up failed: %v", err)
	}
	if second <= first {
		t.Fatalf("expected strictly increasing ID after small drift recovery, got first=%d second=%d", first, second)
	}
}

func TestGeneratorSequenceExhaustionAdvancesTimestampWithoutReuse(t *testing.T) {
	gen, err := NewGenerator(9)
	if err != nil {
		t.Fatalf("NewGenerator failed: %v", err)
	}

	now := CustomEpochMillis + 30000
	gen.nowMillis = func() int64 { return now }

	// Fill sequences 0..4095 at timestamp `now`, then simulate clock advancing on waitUntil.
	seen := make(map[uint64]struct{}, maxSequence+2)
	for i := 0; i <= maxSequence; i++ {
		id, err := gen.NextID()
		if err != nil {
			t.Fatalf("NextID #%d failed: %v", i, err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at sequence %d: %d", i, id)
		}
		seen[id] = struct{}{}
	}

	var reads int
	gen.nowMillis = func() int64 {
		reads++
		if reads == 1 {
			return now
		}
		return now + 1
	}
	rolloverID, err := gen.NextID()
	if err != nil {
		t.Fatalf("rollover NextID failed: %v", err)
	}
	if _, dup := seen[rolloverID]; dup {
		t.Fatalf("rollover ID reused previously issued ID: %d", rolloverID)
	}
}

func TestGeneratorDistinctNodesProduceDisjointIDs(t *testing.T) {
	g1, err := NewGenerator(1)
	if err != nil {
		t.Fatalf("NewGenerator(1): %v", err)
	}
	g2, err := NewGenerator(2)
	if err != nil {
		t.Fatalf("NewGenerator(2): %v", err)
	}
	fixedNow := CustomEpochMillis + 50000
	g1.nowMillis = func() int64 { return fixedNow }
	g2.nowMillis = func() int64 { return fixedNow }

	id1, err := g1.NextID()
	if err != nil {
		t.Fatalf("g1.NextID: %v", err)
	}
	id2, err := g2.NextID()
	if err != nil {
		t.Fatalf("g2.NextID: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("distinct nodes at same millisecond produced identical ID: %d", id1)
	}
}
