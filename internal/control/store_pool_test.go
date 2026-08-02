package control

import (
	"testing"
	"time"
)

func TestNormalizedPoolConfigUsesBoundedDefaults(t *testing.T) {
	config, err := normalizedPoolConfig(PoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxOpenConns != 10 || config.MaxIdleConns != 4 ||
		config.ConnMaxLifetime != 30*time.Minute {
		t.Fatalf("default pool configuration = %+v", config)
	}
}

func TestNormalizedPoolConfigPreservesExplicitCapacity(t *testing.T) {
	want := PoolConfig{MaxOpenConns: 128, MaxIdleConns: 32, ConnMaxLifetime: 5 * time.Minute}
	got, err := normalizedPoolConfig(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("pool configuration = %+v, want %+v", got, want)
	}
}

func TestNormalizedPoolConfigRejectsUnboundedOrIncoherentValues(t *testing.T) {
	for _, config := range []PoolConfig{
		{MaxOpenConns: -1},
		{MaxOpenConns: 4097},
		{MaxOpenConns: 8, MaxIdleConns: 9},
		{MaxOpenConns: 8, MaxIdleConns: -1},
		{MaxOpenConns: 8, MaxIdleConns: 2, ConnMaxLifetime: time.Millisecond},
	} {
		if _, err := normalizedPoolConfig(config); err == nil {
			t.Fatalf("invalid pool configuration was accepted: %+v", config)
		}
	}
}
