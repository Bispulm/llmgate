package routing

import "testing"

func TestResolve(t *testing.T) {
	table := NewTable([]Alias{{Name: "fast", MaxOutputTokens: 10, Routes: []Route{{ProviderName: "p", Model: "m"}}}})

	got, ok := table.Resolve("fast")
	if !ok {
		t.Fatal("alias not found")
	}
	if got.MaxOutputTokens != 10 || got.Routes[0].Model != "m" {
		t.Fatalf("alias = %+v", got)
	}
	if _, ok := table.Resolve("missing"); ok {
		t.Fatal("found an alias that doesn't exist")
	}
	if _, ok := table.Resolve("FAST"); ok {
		t.Fatal("alias lookup must be exact")
	}
}
