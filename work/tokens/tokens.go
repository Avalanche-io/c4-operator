// Package tokens weighs a model's token classes by what they cost, so a
// unit's model spend is honest when no price came with it.
//
// Token classes are not priced alike. Relative to a model's base input
// price, Anthropic's published rates (sources below, as of 2026-10-06)
// are:
//
//	input (uncached)       1
//	output                 5     (every current Claude model: $20/$4 Opus 5.5,
//	                              $10/$2 Sonnet 5.5, $0.50/$0.10 Haiku 5.5)
//	cache write, 5 minutes 1.25
//	cache write, 1 hour    2
//	cache read             0.1   (0.05 on Claude Opus 5.5, 0.025 on Claude
//	                              Fable 5.1 and Claude Mythos 5.1)
//
// Weighted tokens are input-equivalent: input + 5 output + 1.25 cache
// writes + the model's cache-read weight times cache reads. The usage a
// unit reports does not say which cache lifetime a write used, so writes
// are weighed at the 5-minute rate, the default and the cheaper. An agent
// session reading its context from the cache, 514M cache reads against
// 1.06M output, weighs 51.4M + 5.3M, not 515M.
//
// Sources: https://platform.claude.com/docs/en/about-claude/pricing and
// https://platform.claude.com/docs/en/build-with-claude/prompt-caching.
// Models it does not know get the default weights, and no price.
package tokens

import "strings"

// Weights of a token class against base input.
const (
	Output        = 5.0
	CacheWrite    = 1.25
	CacheWrite1h  = 2.0
	CacheRead     = 0.1
	cacheReadOpus = 0.05
	cacheReadTop  = 0.025
)

// Counts are one model's tokens by class.
type Counts struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Raw is every token, unweighted.
func (c Counts) Raw() int64 { return c.Input + c.Output + c.CacheRead + c.CacheWrite }

// CacheReadWeight is a model's cache-read price against its base input.
func CacheReadWeight(model string) float64 {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-fable-5-1"), strings.HasPrefix(m, "claude-mythos-5-1"):
		return cacheReadTop
	case strings.HasPrefix(m, "claude-opus-5-5"):
		return cacheReadOpus
	}
	return CacheRead
}

// Weighted is counts in input-equivalent tokens for model.
func Weighted(model string, c Counts) float64 {
	return float64(c.Input) + Output*float64(c.Output) + CacheWrite*float64(c.CacheWrite) +
		CacheReadWeight(model)*float64(c.CacheRead)
}

// inputPrice is base input in US dollars per million tokens, Anthropic
// first-party rates as of 2026-10-06 (the pricing page above).
var inputPrice = map[string]float64{
	"claude-fable-5-1":  10,
	"claude-mythos-5-1": 10,
	"claude-fable-5":    10,
	"claude-opus-5-5":   4,
	"claude-opus-5":     5,
	"claude-opus-4-8":   5,
	"claude-opus-4-7":   5,
	"claude-opus-4-6":   5,
	"claude-sonnet-5-5": 2,
	"claude-sonnet-5":   2,
	"claude-sonnet-4-6": 3,
	"claude-haiku-5-5":  0.1,
	"claude-haiku-4-5":  1,
}

// InputPrice is a model's base input price per token, and whether it is
// known. A dated suffix or a platform prefix ("anthropic.", "@...") is
// ignored.
func InputPrice(model string) (float64, bool) {
	m := strings.ToLower(model)
	m = strings.TrimPrefix(m, "anthropic.")
	if i := strings.IndexAny(m, "@["); i >= 0 {
		m = m[:i]
	}
	best := ""
	for k := range inputPrice {
		if strings.HasPrefix(m, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return 0, false
	}
	return inputPrice[best] / 1e6, true
}

// Estimate is what counts of model cost at its published rates, and
// whether the model's price is known.
func Estimate(model string, c Counts) (float64, bool) {
	p, ok := InputPrice(model)
	if !ok {
		return 0, false
	}
	return Weighted(model, c) * p, true
}
