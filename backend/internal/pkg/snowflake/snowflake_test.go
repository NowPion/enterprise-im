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
