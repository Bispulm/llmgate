package accounting

import (
	"errors"
	"math"
	"testing"

	"github.com/Bispulm/llmgate/internal/chat"
)

func TestCostRoundsUpToTheNextMicro(t *testing.T) {
	tests := []struct {
		name          string
		tokens, price int64
		want          int64
	}{
		{"zero tokens", 0, 1_000_000, 0},
		{"zero price", 1000, 0, 0},
		{"exact", 1, 1_000_000, 1},
		{"one pico rounds up to a whole micro", 1, 1, 1},
		{"just under a micro", 1, 999_999, 1},
		{"just over a micro", 1, 1_000_001, 2},
		{"a sub-micro price across many tokens", 1_000_000, 37_500, 37_500},
		{"$2.50 per MTok for a million tokens is $2.50", 1_000_000, 2_500_000, 2_500_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Cost(tt.tokens, tt.price)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("Cost(%d, %d) = %d, want %d", tt.tokens, tt.price, got, tt.want)
			}
		})
	}
}

func TestCostHandlesValuesBeyondSixtyFourBits(t *testing.T) {
	// The product overflows 64 bits, but the result in micros fits.
	const tokens, price = int64(1) << 40, int64(1) << 40
	got, err := Cost(tokens, price)
	if err != nil {
		t.Fatal(err)
	}
	const want = int64(1208925819614629175) // ceil(2^80 / 10^6)
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestCostRejectsOverflowAndNegatives(t *testing.T) {
	for name, args := range map[string][2]int64{
		"the result doesn't fit": {math.MaxInt64, math.MaxInt64},
		"negative tokens":        {-1, 1},
		"negative price":         {1, -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Cost(args[0], args[1]); !errors.Is(err, ErrAmountOutOfRange) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAddChecked(t *testing.T) {
	if got, err := addChecked(1, 2); err != nil || got != 3 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := addChecked(math.MaxInt64, 1); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseUSDPerMTok(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"1.00", 1_000_000},
		{"0.075", 75_000},
		{"0.0375", 37_500},
		{"2.5", 2_500_000},
		{"0.000001", 1},
		{"15", 15_000_000},
	}
	for _, tt := range tests {
		got, err := ParseUSDPerMTok(tt.input)
		if err != nil {
			t.Errorf("%q: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q = %d, want %d", tt.input, got, tt.want)
		}
	}
	for _, bad := range []string{"", "-1", "abc", "1e3", "1.2.3", "0.0000001", ".5", "5.", " 1", "1,000", "99999999999999999999", "NaN", "+1"} {
		if got, err := ParseUSDPerMTok(bad); err == nil {
			t.Errorf("%q was accepted as %d", bad, got)
		}
	}
}

func testPrices() Prices {
	return Prices{
		Input:       3_000_000,  // $3 per MTok
		CachedInput: 300_000,    // $0.30
		CacheWrite:  6_000_000,  // $6, the highest input rate
		Output:      15_000_000, // $15
	}
}

func TestChargeUsesEachTokenCategorysRate(t *testing.T) {
	cached, write := int64(100), int64(50)
	usage := &chat.Usage{
		State:                  chat.UsageReported,
		PromptTokens:           1000, // 850 plain, 100 cached, 50 written
		CompletionTokens:       200,
		TotalTokens:            1200,
		CachedPromptTokens:     &cached,
		CacheWritePromptTokens: &write,
	}
	got, err := testPrices().Charge(usage)
	if err != nil {
		t.Fatal(err)
	}
	// 850*3 + 100*0.3 + 50*6 + 200*15 = 2550 + 30 + 300 + 3000
	if got != 5880 {
		t.Fatalf("charge = %d, want 5880", got)
	}
}

func TestChargeCountsReasoningOnce(t *testing.T) {
	reasoning := int64(150)
	usage := &chat.Usage{State: chat.UsageReported, PromptTokens: 0, CompletionTokens: 200, TotalTokens: 200, ReasoningTokens: &reasoning}
	got, err := testPrices().Charge(usage)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3000 {
		t.Fatalf("charge = %d: completion tokens already include reasoning", got)
	}
}

func TestChargeTakesTheHigherCountWhenAReportIsInconsistent(t *testing.T) {
	usage := &chat.Usage{State: chat.UsageInconsistent, PromptTokens: 100, CompletionTokens: 10, TotalTokens: 500}
	got, err := testPrices().Charge(usage)
	if err != nil {
		t.Fatal(err)
	}
	// Total implies 400 completion tokens. Charging the lower figure could undercharge.
	if want := int64(100*3 + 400*15); got != want {
		t.Fatalf("charge = %d, want %d", got, want)
	}
}

func TestChargeRejectsUsageItCantTrust(t *testing.T) {
	cached := int64(2000)
	for name, usage := range map[string]*chat.Usage{
		"absent":    nil,
		"malformed": {State: chat.UsageMalformed},
		"partial":   {State: chat.UsagePartial, PromptTokens: 5},
		"breakdown larger than the total": {
			State: chat.UsageReported, PromptTokens: 1000, CompletionTokens: 1, TotalTokens: 1001, CachedPromptTokens: &cached,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testPrices().Charge(usage); !errors.Is(err, ErrUsageUntrusted) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReservationIsAnUpperBoundForEveryCategoryMix(t *testing.T) {
	prices := testPrices()
	const inputBound, outputBound = int64(1000), int64(500)
	reservation, err := prices.Reservation(inputBound, outputBound)
	if err != nil {
		t.Fatal(err)
	}
	// All input at the highest rate, all output at the output rate.
	if want := int64(1000*6 + 500*15); reservation != want {
		t.Fatalf("reservation = %d, want %d", reservation, want)
	}

	for _, mix := range []struct{ cached, write int64 }{{0, 0}, {1000, 0}, {0, 1000}, {300, 300}, {999, 1}} {
		cached, write := mix.cached, mix.write
		charge, err := prices.Charge(&chat.Usage{
			State:                  chat.UsageReported,
			PromptTokens:           inputBound,
			CompletionTokens:       outputBound,
			TotalTokens:            inputBound + outputBound,
			CachedPromptTokens:     &cached,
			CacheWritePromptTokens: &write,
		})
		if err != nil {
			t.Fatal(err)
		}
		if charge > reservation {
			t.Fatalf("a %+v charge of %d exceeds the reservation of %d", mix, charge, reservation)
		}
	}
}

func TestInputBoundCoversTheRequest(t *testing.T) {
	short := InputBound(&chat.Request{Messages: []chat.Message{{Role: chat.RoleUser, Content: "hi"}}})
	long := InputBound(&chat.Request{Messages: []chat.Message{
		{Role: chat.RoleSystem, Content: "you are"},
		{Role: chat.RoleUser, Content: "héllo 世界"},
	}})
	if short <= 2 {
		t.Fatalf("bound %d doesn't exceed the text's bytes", short)
	}
	if long <= short {
		t.Fatalf("a longer request has a bound of %d, against %d", long, short)
	}
	// A bound by bytes, not characters: each byte could be a token.
	if bytes := int64(len("you are") + len("héllo 世界")); long < bytes {
		t.Fatalf("bound %d is under the %d bytes of text", long, bytes)
	}
	stops := InputBound(&chat.Request{Messages: []chat.Message{{Role: chat.RoleUser, Content: "hi"}}, Stop: []string{"stopstopstop"}})
	if stops <= short {
		t.Fatalf("stop sequences weren't counted: %d", stops)
	}
}

func FuzzCostNeverUnderCharges(f *testing.F) {
	f.Add(int64(1), int64(1))
	f.Add(int64(1_000_000), int64(37_500))
	f.Add(int64(3), int64(333_333))
	f.Fuzz(func(t *testing.T, tokens, price int64) {
		if tokens < 0 || price < 0 || tokens > 1<<31 || price > 1<<31 {
			return
		}
		got, err := Cost(tokens, price)
		if err != nil {
			t.Fatal(err)
		}
		// Small enough for plain arithmetic to check the wide version.
		product := tokens * price
		if got*picoPerMicro < product {
			t.Fatalf("Cost(%d, %d) = %d under-charges %d pico", tokens, price, got, product)
		}
		if (got-1)*picoPerMicro >= product && product > 0 {
			t.Fatalf("Cost(%d, %d) = %d rounds up too far", tokens, price, got)
		}
	})
}
