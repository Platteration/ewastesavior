package hwinfo

import (
	"testing"
	"time"
)

func TestBenchmark(t *testing.T) {
	start := time.Now()
	score := Benchmark(50 * time.Millisecond)
	if score < 1 {
		t.Fatalf("score = %d", score)
	}
	if el := time.Since(start); el < 50*time.Millisecond || el > 5*time.Second {
		t.Fatalf("Benchmark(50ms) took %v", el)
	}
}

func TestBenchmarkDefaultDuration(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 1 s")
	}
	start := time.Now()
	if score := Benchmark(-1); score < 1 {
		t.Fatalf("score = %d", score)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("Benchmark(-1) took only %v", el)
	}
}
