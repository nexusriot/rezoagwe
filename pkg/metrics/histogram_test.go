package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestHistogramCountsAndSums(t *testing.T) {
	h := NewHistogram()
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, time.Second} {
		h.Observe(d)
	}
	s := h.Snapshot()
	if s.Count != 3 {
		t.Errorf("count = %d, want 3", s.Count)
	}
	if want := 1.021; s.Sum < want-0.001 || s.Sum > want+0.001 {
		t.Errorf("sum = %v, want ~%v", s.Sum, want)
	}
	if s.Max != 1 {
		t.Errorf("max = %v, want 1", s.Max)
	}
	if s.Last != 1 {
		t.Errorf("last = %v, want 1", s.Last)
	}
	if mean := s.Mean(); mean < 0.34 || mean > 0.341 {
		t.Errorf("mean = %v, want ~0.3403", mean)
	}
}

// A value exactly on a bucket bound belongs in that bucket: Prometheus buckets
// are "less than or equal to".
func TestHistogramBucketBoundsAreInclusive(t *testing.T) {
	h := NewHistogram()
	h.Observe(5 * time.Millisecond) // exactly the 0.005 bound
	s := h.Snapshot()

	cumulative := uint64(0)
	for i, bound := range s.Bounds {
		cumulative += s.Counts[i]
		if bound == 0.005 {
			if cumulative != 1 {
				t.Errorf("le=0.005 holds %d, want 1", cumulative)
			}
			return
		}
	}
	t.Fatal("no 0.005 bucket")
}

func TestHistogramSeparatesFastFromSlow(t *testing.T) {
	h := NewHistogram()
	for i := 0; i < 99; i++ {
		h.Observe(time.Millisecond)
	}
	h.Observe(20 * time.Second)

	s := h.Snapshot()
	if p50 := s.Quantile(0.5); p50 > 0.005 {
		t.Errorf("p50 = %v, want a millisecond bucket", p50)
	}
	// The outlier is the one that matters and no bucketed estimate recovers
	// it, which is why Max is carried alongside.
	if s.Max != 20 {
		t.Errorf("max = %v, want 20", s.Max)
	}
}

func TestHistogramNegativeObservationIsZero(t *testing.T) {
	h := NewHistogram()
	h.Observe(-time.Second)
	s := h.Snapshot()
	if s.Sum != 0 || s.Max != 0 {
		t.Errorf("sum = %v, max = %v, want 0 for a backwards clock", s.Sum, s.Max)
	}
	if s.Count != 1 {
		t.Errorf("count = %d, want the observation still counted", s.Count)
	}
}

func TestHistogramNilIsSafe(t *testing.T) {
	var h *Histogram
	h.Observe(time.Second) // must not panic
	if s := h.Snapshot(); s.Count != 0 {
		t.Errorf("nil snapshot count = %d, want 0", s.Count)
	}
}

func TestHistogramPrometheusIsCumulative(t *testing.T) {
	m := New()
	m.GossipRound.Observe(2 * time.Millisecond)
	m.GossipRound.Observe(3 * time.Second)
	out := m.Snapshot().Prometheus()

	if !strings.Contains(out, "# TYPE rezoagwe_gossip_round_seconds histogram") {
		t.Error("no histogram TYPE line")
	}
	if !strings.Contains(out, `rezoagwe_gossip_round_seconds_bucket{le="+Inf"} 2`) {
		t.Errorf("+Inf bucket should hold every observation:\n%s", out)
	}
	if !strings.Contains(out, "rezoagwe_gossip_round_seconds_count 2") {
		t.Error("no count line")
	}
	if !strings.Contains(out, "rezoagwe_gossip_round_seconds_max 3") {
		t.Error("no max gauge")
	}
	// Cumulative: the 0.005 bucket holds the fast observation and not the slow.
	if !strings.Contains(out, `rezoagwe_gossip_round_seconds_bucket{le="0.005"} 1`) {
		t.Errorf("buckets are not cumulative:\n%s", out)
	}
}

// An un-exercised histogram must not emit an empty, malformed block.
func TestHistogramPrometheusSkipsUnusedHistograms(t *testing.T) {
	var s Snapshot
	if out := s.Prometheus(); strings.Contains(out, "gossip_round_seconds_bucket") {
		t.Errorf("zero-value snapshot emitted buckets:\n%s", out)
	}
}
