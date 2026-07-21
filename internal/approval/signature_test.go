package approval

import (
	"errors"
	"testing"
	"time"
)

func TestCallbackSignature(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_750_000_000, 0)
	timestamp := "1750000000"
	body := []byte(`{"task_id":"task-1"}`)
	signature := Sign([]byte("secret"), timestamp, body)
	if err := Verify([]byte("secret"), timestamp, signature, body, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Verify([]byte("wrong"), timestamp, signature, body, now, 5*time.Minute); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected invalid signature, got %v", err)
	}
	if err := Verify([]byte("secret"), timestamp, signature, body, now.Add(6*time.Minute), 5*time.Minute); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("expected stale timestamp, got %v", err)
	}
}
