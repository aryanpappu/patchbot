package llm

import (
	"math"
	"math/bits"
)

// Money is kept as whole micro-USD (millionths of a dollar) in an int64, so
// sums are exact and never drift the way floats do.

// MicroPerUSD is the number of micro-USD in one dollar.
const MicroPerUSD = 1_000_000

const tokensPerMillion = 1_000_000

// Price is what one model costs, in micro-USD per million tokens.
type Price struct {
	Input  int64
	Output int64
	// CacheRead and CacheWrite fall back to Input when nil, which never
	// undercounts: cached reads cost less than fresh input everywhere.
	CacheRead  *int64
	CacheWrite *int64
}

func (p Price) cacheRead() int64 {
	if p.CacheRead != nil {
		return *p.CacheRead
	}
	return p.Input
}

func (p Price) cacheWrite() int64 {
	if p.CacheWrite != nil {
		return *p.CacheWrite
	}
	return p.Input
}

// Pricing holds the prices of one provider's models. No prices are compiled
// into patchbot: they all come from the user's config. The zero value knows
// no price.
type Pricing struct {
	// Free states that every model of the provider costs nothing, as with a
	// local server. That is a known price of zero, not an unknown one.
	Free bool
	// Models maps a model ID to its price.
	Models map[string]Price
}

// Price returns the price of a model and whether it is known.
func (p Pricing) Price(model string) (Price, bool) {
	if p.Free {
		return Price{}, true
	}
	price, ok := p.Models[model]
	return price, ok
}

// Cost returns what a call cost in micro-USD. known is false when the model
// has no price or the usage is nil, and then micro is 0 and must be treated
// as unknown, never as free.
//
// The sum over all token classes is computed exactly in 128 bits and divided
// once, rounding up, so a cost is never understated and rounding does not
// pile up per class. Negative token counts are treated as 0. A result too
// large for an int64 is returned as math.MaxInt64.
func (p Pricing) Cost(model string, u *Usage) (micro int64, known bool) {
	price, ok := p.Price(model)
	if !ok || u == nil {
		return 0, false
	}
	var sum uint128
	sum.addMul(u.InputTokens, price.Input)
	sum.addMul(u.OutputTokens, price.Output)
	sum.addMul(u.CacheReadTokens, price.cacheRead())
	sum.addMul(u.CacheWriteTokens, price.cacheWrite())
	return sum.divCeil(tokensPerMillion), true
}

// Reserve returns the most a call can cost, for the budget check made before
// the call is sent: every input token at the dearest input rate (fresh,
// cache read or cache write) and the full output cap at the output rate.
// known is false when the model has no price.
func (p Pricing) Reserve(model string, inputTokens, maxOutput int64) (micro int64, known bool) {
	price, ok := p.Price(model)
	if !ok {
		return 0, false
	}
	var sum uint128
	sum.addMul(inputTokens, max(price.Input, price.cacheRead(), price.cacheWrite()))
	sum.addMul(maxOutput, price.Output)
	return sum.divCeil(tokensPerMillion), true
}

// USDToMicro converts dollars to micro-USD, rounding to the nearest unit. It
// is for reading prices and budgets from the config, the only place a float
// amount exists. ok is false for a negative, infinite or NaN amount, or one
// too large for an int64.
func USDToMicro(usd float64) (micro int64, ok bool) {
	scaled := math.Round(usd * MicroPerUSD)
	if math.IsNaN(scaled) || scaled < 0 || scaled >= math.MaxInt64 {
		return 0, false
	}
	return int64(scaled), true
}

// uint128 is an unsigned 128-bit accumulator that saturates.
type uint128 struct {
	hi, lo   uint64
	overflow bool
}

// addMul adds a*b. Negative factors count as 0.
func (u *uint128) addMul(a, b int64) {
	if a <= 0 || b <= 0 {
		return
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	var carry uint64
	u.lo, carry = bits.Add64(u.lo, lo, 0)
	u.hi, carry = bits.Add64(u.hi, hi, carry)
	if carry != 0 {
		u.overflow = true
	}
}

// divCeil returns ceil(u/d) as an int64, saturating at math.MaxInt64.
func (u uint128) divCeil(d uint64) int64 {
	if u.overflow || u.hi >= d {
		return math.MaxInt64 // the quotient would not fit in 64 bits
	}
	quo, rem := bits.Div64(u.hi, u.lo, d)
	if quo >= math.MaxInt64 {
		return math.MaxInt64
	}
	if rem != 0 {
		quo++
	}
	return int64(quo)
}
