package llm_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm"
)

func micro(v int64) *int64 { return &v }

// Made-up prices in micro-USD per million tokens. No real price is used
// anywhere in patchbot.
var testPricing = llm.Pricing{Models: map[string]llm.Price{
	"plain":  {Input: 3_000_000, Output: 15_000_000},
	"cached": {Input: 3_000_000, Output: 15_000_000, CacheRead: micro(300_000), CacheWrite: micro(3_750_000)},
	"zero":   {},
	"odd":    {Input: 1, Output: 7},
}}

func TestCost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pricing   llm.Pricing
		model     string
		usage     *llm.Usage
		want      int64
		wantKnown bool
	}{
		{name: "one million of each", pricing: testPricing, model: "plain", usage: &llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, want: 18_000_000, wantKnown: true},
		{name: "exact small call", pricing: testPricing, model: "plain", usage: &llm.Usage{InputTokens: 1000, OutputTokens: 500}, want: 3000 + 7500, wantKnown: true},
		{name: "one input token", pricing: testPricing, model: "plain", usage: &llm.Usage{InputTokens: 1}, want: 3, wantKnown: true},
		{name: "cache prices", pricing: testPricing, model: "cached", usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 10_000, CacheWriteTokens: 2_000}, want: 300 + 150 + 3000 + 7500, wantKnown: true},
		{name: "missing cache prices fall back to input", pricing: testPricing, model: "plain", usage: &llm.Usage{CacheReadTokens: 1000, CacheWriteTokens: 1000}, want: 6000, wantKnown: true},
		{name: "explicit zero cache price is used", pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: 5_000_000, CacheRead: micro(0)}}}, model: "m", usage: &llm.Usage{CacheReadTokens: 1_000_000}, want: 0, wantKnown: true},

		// Rounding: always up, and once for the whole call.
		{name: "fraction rounds up", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 1}, want: 1, wantKnown: true},
		{name: "just under a micro rounds up", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 999_999}, want: 1, wantKnown: true},
		{name: "exactly one micro", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 1_000_000}, want: 1, wantKnown: true},
		{name: "just over a micro rounds up", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 1_000_001}, want: 2, wantKnown: true},
		// 1*1 + 1*7 = 8 millionths of a micro in total: one round-up, not two.
		{name: "classes are summed before rounding", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 1, OutputTokens: 1}, want: 1, wantKnown: true},
		// 500000*1 + 71429*7 = 1000003 -> ceil = 2.
		{name: "sum crosses a micro", pricing: testPricing, model: "odd", usage: &llm.Usage{InputTokens: 500_000, OutputTokens: 71_429}, want: 2, wantKnown: true},

		// Known zero versus unknown.
		{name: "zero usage costs nothing", pricing: testPricing, model: "plain", usage: &llm.Usage{}, want: 0, wantKnown: true},
		{name: "zero price is known", pricing: testPricing, model: "zero", usage: &llm.Usage{InputTokens: 5_000_000}, want: 0, wantKnown: true},
		{name: "free provider prices every model at zero", pricing: llm.Pricing{Free: true}, model: "anything", usage: &llm.Usage{InputTokens: 5_000_000, OutputTokens: 5_000_000}, want: 0, wantKnown: true},
		{name: "model with no price", pricing: testPricing, model: "unlisted", usage: &llm.Usage{InputTokens: 1000}, want: 0, wantKnown: false},
		{name: "empty pricing", pricing: llm.Pricing{}, model: "plain", usage: &llm.Usage{InputTokens: 1000}, want: 0, wantKnown: false},
		{name: "unknown usage", pricing: testPricing, model: "plain", usage: nil, want: 0, wantKnown: false},
		{name: "unknown usage on a free provider", pricing: llm.Pricing{Free: true}, model: "m", usage: nil, want: 0, wantKnown: false},

		// Hostile or huge inputs.
		{name: "negative tokens count as zero", pricing: testPricing, model: "plain", usage: &llm.Usage{InputTokens: -1_000_000, OutputTokens: 1_000_000}, want: 15_000_000, wantKnown: true},
		{name: "product beyond 64 bits is exact", pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: 4_000_000_000}}}, model: "m", usage: &llm.Usage{InputTokens: 9_000_000_000_000}, want: 36_000_000_000_000_000, wantKnown: true},
		{name: "result beyond int64 saturates", pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: math.MaxInt64, Output: math.MaxInt64}}}, model: "m", usage: &llm.Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}, want: math.MaxInt64, wantKnown: true},
		{name: "largest quotient that fits", pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: 1_000_000}}}, model: "m", usage: &llm.Usage{InputTokens: math.MaxInt64}, want: math.MaxInt64, wantKnown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, known := tt.pricing.Cost(tt.model, tt.usage)
			if got != tt.want || known != tt.wantKnown {
				t.Errorf("Cost = %d, %v; want %d, %v", got, known, tt.want, tt.wantKnown)
			}
		})
	}
}

// TestCostMatchesBigInt checks the 128-bit math against math/big over a grid
// of awkward values.
func TestCostMatchesBigInt(t *testing.T) {
	t.Parallel()
	values := []int64{0, 1, 2, 999_999, 1_000_000, 1_000_001, 123_456_789, 1 << 31, 1<<32 + 1, 1 << 40, 987_654_321_987}
	million := big.NewInt(1_000_000)
	for _, tokens := range values {
		for _, in := range values {
			for _, out := range values {
				pricing := llm.Pricing{Models: map[string]llm.Price{"m": {Input: in, Output: out, CacheRead: micro(in / 3), CacheWrite: micro(out / 7)}}}
				usage := &llm.Usage{InputTokens: tokens, OutputTokens: tokens / 2, CacheReadTokens: tokens * 3, CacheWriteTokens: tokens / 5}

				sum := new(big.Int)
				for _, term := range [][2]int64{{usage.InputTokens, in}, {usage.OutputTokens, out}, {usage.CacheReadTokens, in / 3}, {usage.CacheWriteTokens, out / 7}} {
					sum.Add(sum, new(big.Int).Mul(big.NewInt(term[0]), big.NewInt(term[1])))
				}
				quo, rem := new(big.Int).QuoRem(sum, million, new(big.Int))
				if rem.Sign() != 0 {
					quo.Add(quo, big.NewInt(1))
				}
				want := int64(math.MaxInt64)
				if quo.IsInt64() {
					want = quo.Int64()
				}

				if got, known := pricing.Cost("m", usage); got != want || !known {
					t.Fatalf("Cost(tokens=%d, in=%d, out=%d) = %d, %v; want %d", tokens, in, out, got, known, want)
				}
			}
		}
	}
}

func TestCostIsAdditiveWithinRounding(t *testing.T) {
	t.Parallel()
	// Summing per-call costs may round up once per call, never down: the
	// tracked total can only overstate the exact cost, by less than one
	// micro-USD per call.
	usage := &llm.Usage{InputTokens: 333, OutputTokens: 77, CacheReadTokens: 1234}
	const calls = 1000
	var total int64
	sumUsage := &llm.Usage{}
	for range calls {
		cost, _ := testPricing.Cost("cached", usage)
		total += cost
		sumUsage = sumUsage.Add(usage)
	}
	exact, _ := testPricing.Cost("cached", sumUsage)
	if total < exact || total-exact >= calls {
		t.Errorf("sum of per-call costs = %d, exact cost of the summed usage = %d; want exact <= sum < exact+%d", total, exact, calls)
	}
}

func TestReserve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pricing   llm.Pricing
		model     string
		input     int64
		maxOutput int64
		want      int64
		wantKnown bool
	}{
		{name: "input plus full output cap", pricing: testPricing, model: "plain", input: 10_000, maxOutput: 8192, want: 30_000 + 122_880, wantKnown: true},
		{name: "input at the cache-write rate when it is dearest", pricing: testPricing, model: "cached", input: 10_000, maxOutput: 0, want: 37_500, wantKnown: true},
		{
			name:    "input at the plain rate when cache write is cheaper",
			pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: 3_000_000, CacheRead: micro(1), CacheWrite: micro(2)}}},
			model:   "m", input: 1_000_000, want: 3_000_000, wantKnown: true,
		},
		{name: "rounds up", pricing: testPricing, model: "odd", input: 1, maxOutput: 1, want: 1, wantKnown: true},
		{name: "free provider", pricing: llm.Pricing{Free: true}, model: "m", input: 1 << 40, maxOutput: 1 << 20, want: 0, wantKnown: true},
		{name: "no price", pricing: testPricing, model: "unlisted", input: 10, maxOutput: 10, want: 0, wantKnown: false},
		{name: "negative counts", pricing: testPricing, model: "plain", input: -5, maxOutput: -5, want: 0, wantKnown: true},
		{name: "saturates", pricing: llm.Pricing{Models: map[string]llm.Price{"m": {Input: math.MaxInt64, Output: math.MaxInt64}}}, model: "m", input: math.MaxInt64, maxOutput: math.MaxInt64, want: math.MaxInt64, wantKnown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, known := tt.pricing.Reserve(tt.model, tt.input, tt.maxOutput)
			if got != tt.want || known != tt.wantKnown {
				t.Errorf("Reserve = %d, %v; want %d, %v", got, known, tt.want, tt.wantKnown)
			}
		})
	}
}

// TestReserveCoversCost checks the point of Reserve: no split of the same
// tokens across the input classes can cost more than was reserved.
func TestReserveCoversCost(t *testing.T) {
	t.Parallel()
	const input, output = 123_457, 4_321
	for _, model := range []string{"plain", "cached", "odd", "zero"} {
		reserved, _ := testPricing.Reserve(model, input, output)
		for _, split := range []llm.Usage{
			{InputTokens: input, OutputTokens: output},
			{CacheReadTokens: input, OutputTokens: output},
			{CacheWriteTokens: input, OutputTokens: output},
			{InputTokens: input / 3, CacheReadTokens: input / 3, CacheWriteTokens: input - 2*(input/3), OutputTokens: output},
			{InputTokens: input, OutputTokens: 1},
		} {
			cost, _ := testPricing.Cost(model, &split)
			if cost > reserved {
				t.Errorf("model %s: cost %d of %+v exceeds the reserve %d", model, cost, split, reserved)
			}
		}
	}
}

func TestUSDToMicro(t *testing.T) {
	t.Parallel()
	tests := []struct {
		usd    float64
		want   int64
		wantOK bool
	}{
		{0, 0, true},
		{1, 1_000_000, true},
		{2.0, 2_000_000, true},
		{0.1, 100_000, true}, // not exact in binary
		{0.3, 300_000, true}, // 0.3*1e6 is 299999.99999999994 in floats
		{1.25, 1_250_000, true},
		{0.000001, 1, true},
		{0.0000004, 0, true}, // below half a micro
		{0.0000005, 1, true},
		{15.075, 15_075_000, true},
		{-0.01, 0, false},
		{math.Inf(1), 0, false},
		{math.NaN(), 0, false},
		{1e13, 0, false}, // 1e19 micro does not fit
	}
	for _, tt := range tests {
		got, ok := llm.USDToMicro(tt.usd)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("USDToMicro(%v) = %d, %v; want %d, %v", tt.usd, got, ok, tt.want, tt.wantOK)
		}
	}
}
