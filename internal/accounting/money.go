package accounting

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"

	"github.com/Bispulm/llmgate/internal/chat"
)

// Amounts are int64 micro-USD. Prices are int64 pico-USD per token, which is USD per
// million tokens times 10^6, so that a price like $0.0375 per MTok is exactly 37,500.
// Floating point is never used for money.
const picoPerMicro = 1_000_000

// Parts of the input bound. See InputBound.
const (
	inputOverheadPerRequest = 64
	inputOverheadPerMessage = 16
)

const maxPriceDecimals = 6

var (
	// ErrAmountOutOfRange means an amount couldn't be represented. Callers must treat
	// it as a rejection, never as zero.
	ErrAmountOutOfRange = errors.New("accounting: amount out of range")
	// ErrUsageUntrusted means a usage report can't support a charge.
	ErrUsageUntrusted = errors.New("accounting: usage can't support a charge")
)

// Cost returns the micro-USD for tokens at a price, rounding up, so that every rounding
// error falls on the side of the budget limit.
func Cost(tokens, pricePico int64) (int64, error) {
	if tokens < 0 || pricePico < 0 {
		return 0, ErrAmountOutOfRange
	}
	// The product can need more than 64 bits even when the result in micros fits.
	hi, lo := bits.Mul64(uint64(tokens), uint64(pricePico))
	if hi >= picoPerMicro {
		return 0, ErrAmountOutOfRange
	}
	quotient, remainder := bits.Div64(hi, lo, picoPerMicro)
	if remainder > 0 {
		quotient++
	}
	if quotient > math.MaxInt64 {
		return 0, ErrAmountOutOfRange
	}
	return int64(quotient), nil
}

func addChecked(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrAmountOutOfRange
	}
	return a + b, nil
}

// ParseUSDPerMTok converts a decimal string of USD per million tokens to pico-USD per
// token. Prices stay as strings in configuration, because a float can't represent most
// of them exactly. Anything but a plain non-negative decimal is rejected.
func ParseUSDPerMTok(value string) (int64, error) {
	whole, fraction, hasPoint := strings.Cut(value, ".")
	if whole == "" || (hasPoint && fraction == "") {
		return 0, fmt.Errorf("price %q must be a decimal number", value)
	}
	if len(fraction) > maxPriceDecimals {
		return 0, fmt.Errorf("price %q has more than %d decimal places", value, maxPriceDecimals)
	}
	var pico int64
	for _, c := range whole + fraction + strings.Repeat("0", maxPriceDecimals-len(fraction)) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("price %q must be a decimal number", value)
		}
		digit := int64(c - '0')
		if pico > (math.MaxInt64-digit)/10 {
			return 0, fmt.Errorf("price %q is too large", value)
		}
		pico = pico*10 + digit
	}
	return pico, nil
}

// Prices are one model's rates, in pico-USD per token.
type Prices struct {
	Input       int64
	CachedInput int64
	CacheWrite  int64
	Output      int64
}

// Charge returns the cost of a usage report. It needs counts it can trust: absent or
// partial usage is uncertain, never zero, and has to be handled by the caller.
//
// Completion tokens already include reasoning, for both supported providers, so
// reasoning isn't added again. A report whose total disagrees with its parts is charged
// on the higher figure, since the lower one could undercharge.
func (p Prices) Charge(u *chat.Usage) (int64, error) {
	if u == nil || (u.State != chat.UsageReported && u.State != chat.UsageInconsistent) {
		return 0, ErrUsageUntrusted
	}
	cached, write := int64(0), int64(0)
	if u.CachedPromptTokens != nil {
		cached = *u.CachedPromptTokens
	}
	if u.CacheWritePromptTokens != nil {
		write = *u.CacheWritePromptTokens
	}
	if cached < 0 || write < 0 || cached > u.PromptTokens || write > u.PromptTokens-cached {
		return 0, ErrUsageUntrusted
	}
	plain := u.PromptTokens - cached - write
	completion := max(u.CompletionTokens, u.TotalTokens-u.PromptTokens)

	var total int64
	for _, part := range []struct{ tokens, price int64 }{
		{plain, p.Input},
		{cached, p.CachedInput},
		{write, p.CacheWrite},
		{completion, p.Output},
	} {
		cost, err := Cost(part.tokens, part.price)
		if err != nil {
			return 0, err
		}
		if total, err = addChecked(total, cost); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// HighestInput returns the highest rate any input token could be charged at.
func (p Prices) HighestInput() int64 {
	return max(p.Input, p.CachedInput, p.CacheWrite)
}

// Reservation returns the most an attempt could cost under these prices, if its tokens
// stay within the bounds. Input is priced at the highest input rate, because which
// category each token falls into isn't known until the provider reports.
func (p Prices) Reservation(inputBound, outputBound int64) (int64, error) {
	input, err := Cost(inputBound, p.HighestInput())
	if err != nil {
		return 0, err
	}
	output, err := Cost(outputBound, p.Output)
	if err != nil {
		return 0, err
	}
	return addChecked(input, output)
}

// InputBound estimates the most input tokens a request could count as: one token per
// byte of text, plus a fixed overhead for each message and the request. Tokenizers
// generally produce fewer tokens than bytes, but that's not guaranteed, so this is a
// bounded risk and not a hard limit. See ADR 0002.
func InputBound(req *chat.Request) int64 {
	bound := int64(inputOverheadPerRequest)
	for _, m := range req.Messages {
		bound += inputOverheadPerMessage + int64(len(m.Content))
	}
	for _, s := range req.Stop {
		bound += int64(len(s))
	}
	return bound
}
