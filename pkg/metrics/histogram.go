package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultBuckets are upper bounds in seconds. They are spread wide rather than
// fine because the question these answer is "is this taking seconds when it
// should take milliseconds", not "is the median 4 ms or 6 ms" — a node whose
// gossip round takes longer than the eviction threshold flaps out of its own
// cluster, and that is orders of magnitude away from healthy.
var defaultBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30}

// Histogram records a distribution of durations.
//
// Every other measurement in this package is a counter, which can say a gossip
// round happened and never that it took eleven seconds. A counter cannot
// express the one failure that has actually been observed on hardware: a node
// whose loop falls far enough behind that its peers evict it while it is
// working perfectly.
type Histogram struct {
	mu      sync.Mutex
	bounds  []float64
	counts  []uint64
	sum     float64
	count   uint64
	max     float64
	lastObs float64
}

// NewHistogram builds a histogram over the default buckets.
func NewHistogram() *Histogram {
	return &Histogram{
		bounds: defaultBuckets,
		counts: make([]uint64, len(defaultBuckets)+1), // +1 for +Inf
	}
}

// Observe records one duration. A negative duration is recorded as zero: it
// means a clock went backwards, not that something took less than no time.
func (h *Histogram) Observe(d time.Duration) {
	// Nil-tolerant for the same reason every counter method here is: the
	// engine's metrics are optional to a caller that only wants the behaviour.
	if h == nil {
		return
	}
	v := d.Seconds()
	if v < 0 {
		v = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.count++
	h.lastObs = v
	if v > h.max {
		h.max = v
	}
	i := sort.SearchFloat64s(h.bounds, v)
	// SearchFloat64s finds the first bound >= v, but a Prometheus bucket is
	// "less than or equal to", so a value exactly on a bound belongs in it.
	if i < len(h.bounds) && v > h.bounds[i] {
		i++
	}
	h.counts[i]++
}

// HistogramSnapshot is a histogram at one instant.
type HistogramSnapshot struct {
	Bounds []float64 `json:"bounds"`
	// Counts are per-bucket, not cumulative; the exporter accumulates them.
	// The final element is the +Inf bucket.
	Counts []uint64 `json:"counts"`
	Sum    float64  `json:"sum"`
	Count  uint64   `json:"count"`
	Max    float64  `json:"max"`
	Last   float64  `json:"last"`
}

// Snapshot copies the histogram.
func (h *Histogram) Snapshot() HistogramSnapshot {
	if h == nil {
		return HistogramSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return HistogramSnapshot{
		Bounds: append([]float64(nil), h.bounds...),
		Counts: append([]uint64(nil), h.counts...),
		Sum:    h.sum,
		Count:  h.count,
		Max:    h.max,
		Last:   h.lastObs,
	}
}

// Mean is the average observation in seconds, or 0 when there are none.
func (s HistogramSnapshot) Mean() float64 {
	if s.Count == 0 {
		return 0
	}
	return s.Sum / float64(s.Count)
}

// Quantile estimates a quantile from the bucket counts.
//
// It is an estimate by construction: the bucket a value fell into is all that
// was kept, so the answer is that bucket's upper bound. That is enough to say
// "the slow rounds are over a second", which is the question, and it is why
// Max is carried separately — the worst round is the one that got a node
// evicted, and no bucketed estimate can report it exactly.
func (s HistogramSnapshot) Quantile(q float64) float64 {
	if s.Count == 0 {
		return 0
	}
	want := q * float64(s.Count)
	cumulative := uint64(0)
	for i, c := range s.Counts {
		cumulative += c
		if float64(cumulative) >= want {
			if i >= len(s.Bounds) {
				return s.Max
			}
			return s.Bounds[i]
		}
	}
	return s.Max
}

// prometheus renders the histogram in the text exposition format.
func (s HistogramSnapshot) prometheus(b *strings.Builder, name, help string) {
	if len(s.Counts) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP rezoagwe_%s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE rezoagwe_%s histogram\n", name)
	cumulative := uint64(0)
	for i, bound := range s.Bounds {
		cumulative += s.Counts[i]
		fmt.Fprintf(b, "rezoagwe_%s_bucket{le=%q} %d\n",
			name, strconv.FormatFloat(bound, 'g', -1, 64), cumulative)
	}
	cumulative += s.Counts[len(s.Counts)-1]
	fmt.Fprintf(b, "rezoagwe_%s_bucket{le=\"+Inf\"} %d\n", name, cumulative)
	fmt.Fprintf(b, "rezoagwe_%s_sum %s\n", name, strconv.FormatFloat(s.Sum, 'g', -1, 64))
	fmt.Fprintf(b, "rezoagwe_%s_count %d\n", name, s.Count)
	// Not part of the histogram proper, but the worst observation is the whole
	// point here and a bucketed quantile cannot recover it.
	fmt.Fprintf(b, "# HELP rezoagwe_%s_max %s (worst observed)\n", name, help)
	fmt.Fprintf(b, "# TYPE rezoagwe_%s_max gauge\n", name)
	fmt.Fprintf(b, "rezoagwe_%s_max %s\n", name, strconv.FormatFloat(s.Max, 'g', -1, 64))
}
