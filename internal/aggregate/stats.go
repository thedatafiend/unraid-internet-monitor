// Package aggregate turns per-second probe results into live ring buffers and
// per-minute rollups.
package aggregate

import (
	"math"
	"slices"
)

// Summary describes a window of samples. RTT fields are milliseconds and are
// only meaningful when Recv > 0.
type Summary struct {
	Sent   int
	Recv   int
	Min    float64
	Avg    float64
	P50    float64
	P95    float64
	P99    float64
	Max    float64
	Jitter float64
}

// LossPct returns packet loss in percent (0 when nothing was sent).
func (s Summary) LossPct() float64 {
	if s.Sent == 0 {
		return 0
	}
	return 100 * float64(s.Sent-s.Recv) / float64(s.Sent)
}

// Summarize computes statistics for sent probes whose successful RTTs are
// rtts, given in send order (order matters for jitter).
func Summarize(sent int, rtts []float64) Summary {
	s := Summary{Sent: sent, Recv: len(rtts)}
	if len(rtts) == 0 {
		return s
	}
	var sum, jsum float64
	for i, r := range rtts {
		sum += r
		if i > 0 {
			jsum += math.Abs(r - rtts[i-1])
		}
	}
	s.Avg = sum / float64(len(rtts))
	if len(rtts) > 1 {
		s.Jitter = jsum / float64(len(rtts)-1)
	}
	sorted := slices.Clone(rtts)
	slices.Sort(sorted)
	s.Min, s.Max = sorted[0], sorted[len(sorted)-1]
	s.P50 = percentile(sorted, 0.50)
	s.P95 = percentile(sorted, 0.95)
	s.P99 = percentile(sorted, 0.99)
	return s
}

// percentile uses the nearest-rank method on sorted input.
func percentile(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}

// MOS estimates a voice-call Mean Opinion Score (1–5) from latency, jitter
// (both ms) and loss (percent) with a simplified ITU-T G.107 E-model.
func MOS(avgMs, jitterMs, lossPct float64) float64 {
	eff := avgMs + 2*jitterMs + 10
	var r float64
	if eff < 160 {
		r = 93.2 - eff/40
	} else {
		r = 93.2 - (eff-120)/10
	}
	r -= 2.5 * lossPct
	if r < 0 {
		return 1
	}
	if r > 100 {
		r = 100
	}
	return 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
}
