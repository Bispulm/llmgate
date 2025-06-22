package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func TestTeamsCanBeCreatedAndRead(t *testing.T) {
	teams := auth.NewTeamStore(testenv.Postgres(t, 4))
	ctx := context.Background()
	created, err := teams.Create(ctx, auth.Team{ID: "acme", BudgetLimitMicros: 5_000_000, Limits: auth.TeamLimits{RequestsPerMinute: 60}})
	if err != nil {
		t.Fatal(err)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("no creation time")
	}
	got, err := teams.Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetLimitMicros != 5_000_000 || got.Limits.RequestsPerMinute != 60 {
		t.Fatalf("team = %+v", got)
	}
	if _, err := teams.Create(ctx, auth.Team{ID: "acme"}); !errors.Is(err, auth.ErrTeamExists) {
		t.Fatalf("err = %v", err)
	}
	if _, err := teams.Get(ctx, "nobody"); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Fatalf("err = %v", err)
	}
	if _, err := teams.Create(ctx, auth.Team{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	list, err := teams.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "acme" || list[1].ID != "beta" {
		t.Fatalf("list = %+v", list)
	}
}

func TestTeamLimitsCanChange(t *testing.T) {
	teams := auth.NewTeamStore(testenv.Postgres(t, 4))
	ctx := context.Background()
	if _, err := teams.Create(ctx, auth.Team{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if err := teams.SetLimits(ctx, "acme", auth.TeamLimits{TokensPerMinute: 1000, MaxInFlight: 4}); err != nil {
		t.Fatal(err)
	}
	got, err := teams.Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits != (auth.TeamLimits{TokensPerMinute: 1000, MaxInFlight: 4}) {
		t.Fatalf("limits = %+v", got.Limits)
	}
	if err := teams.SetLimits(ctx, "nobody", auth.TeamLimits{}); !errors.Is(err, auth.ErrUnknownTeam) {
		t.Fatalf("err = %v", err)
	}
	if err := teams.SetLimits(ctx, "acme", auth.TeamLimits{MaxInFlight: -1}); !errors.Is(err, auth.ErrInvalidTeam) {
		t.Fatalf("err = %v", err)
	}
}

// Team IDs end up in Redis keys, log fields, and metric labels, so they're kept plain.
func TestTeamIDsAreValidated(t *testing.T) {
	teams := auth.NewTeamStore(testenv.Postgres(t, 4))
	for _, id := range []string{"", "Acme", "a b", "{x}", "-lead", "x/y", string(make([]byte, 64))} {
		if _, err := teams.Create(context.Background(), auth.Team{ID: id}); !errors.Is(err, auth.ErrInvalidTeam) {
			t.Errorf("%q: err = %v", id, err)
		}
	}
	if _, err := teams.Create(context.Background(), auth.Team{ID: "ok", BudgetLimitMicros: -1}); !errors.Is(err, auth.ErrInvalidTeam) {
		t.Errorf("negative budget: err = %v", err)
	}
	for _, id := range []string{"a", "team-2", "data_science"} {
		if _, err := teams.Create(context.Background(), auth.Team{ID: id}); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
}
