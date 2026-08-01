package experiment

import (
	"errors"
	"math"
	"math/rand"
	"sort"
)

func Type7(values []float64, probability float64) (float64, error) {
	if len(values) == 0 || probability < 0 || probability > 1 {
		return 0, errors.New("invalid quantile input")
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	if len(ordered) == 1 {
		return ordered[0], nil
	}
	h := float64(len(ordered)-1) * probability
	lower := int(math.Floor(h))
	fraction := h - float64(lower)
	if lower+1 >= len(ordered) {
		return ordered[len(ordered)-1], nil
	}
	return ordered[lower] + fraction*(ordered[lower+1]-ordered[lower]), nil
}

func DeterministicOrder(length int, seed int64) []int {
	if length < 0 {
		length = 0
	}
	return rand.New(rand.NewSource(seed)).Perm(length)
}

type Interval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

func BootstrapMedian(values []float64, seed int64, iterations int) (Interval, error) {
	if len(values) == 0 || seed == 0 || iterations < 1 {
		return Interval{}, errors.New("invalid bootstrap input")
	}
	rng := rand.New(rand.NewSource(seed))
	medians := make([]float64, iterations)
	sample := make([]float64, len(values))
	for i := range medians {
		for j := range sample {
			sample[j] = values[rng.Intn(len(values))]
		}
		median, _ := Type7(sample, .5)
		medians[i] = median
	}
	low, _ := Type7(medians, .025)
	high, _ := Type7(medians, .975)
	return Interval{Low: low, High: high}, nil
}
