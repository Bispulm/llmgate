package metrics_test

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/limits"
	"github.com/Bispulm/llmgate/internal/metrics"
	"github.com/Bispulm/llmgate/internal/routing"
)

// everyMetric records something in every metric, so that each one is exported.
func everyMetric(t *testing.T) *metrics.Metrics {
	t.Helper()
	m := metrics.New(metrics.Options{})
	limiter := limits.NewFairLimiter(limits.Config{MaxConcurrency: 1})
	m.WatchRoutes(routing.NewTable([]routing.Alias{{Name: "a", MaxOutputTokens: 1, Routes: []routing.Route{{ProviderName: "p", Model: "m", Limiter: limiter}}}}, routing.Options{}),
		map[string]*limits.FairLimiter{"p": limiter})
	m.RequestFinished("a", false, http.StatusOK, time.Millisecond)
	m.Attempt(accounting.Attempt{Provider: "p", Model: "m", Reason: accounting.ReasonFallback, TimeToFirstEvent: time.Millisecond})
	m.CacheResult("a", "hit")
	m.RouteSkipped("a", map[routing.SkipReason]int{routing.SkipSaturated: 1})
	m.TeamLimited(limits.ReasonRequests)
	m.BudgetRefused()
	m.Charged("p", "m", accounting.ChargeReported, 1)
	m.SettlementFailed()
	m.Reconciled(accounting.ReconcileResult{Released: 1})
	m.DependencyError("postgres", "admit")
	return m
}

// The dashboard can only show what the gateway exports, so every metric it queries has
// to exist, under that exact name.
func TestTheDashboardOnlyUsesExportedMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/grafana/dashboards/llmgate.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		UID    string `json:"uid"`
		Panels []struct {
			ID      int    `json:"id"`
			Type    string `json:"type"`
			Title   string `json:"title"`
			GridPos struct {
				W, X int
			} `json:"gridPos"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.UID == "" || len(dashboard.Panels) == 0 {
		t.Fatal("an empty dashboard")
	}

	families, err := everyMetric(t).Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for _, f := range families {
		exported[f.GetName()] = true
	}

	name := regexp.MustCompile(`\b(?:llmgate|go|process)_[a-z_]+\b`)
	ids := map[int]bool{}
	queried := 0
	for _, p := range dashboard.Panels {
		if ids[p.ID] {
			t.Errorf("panel %q reuses id %d", p.Title, p.ID)
		}
		ids[p.ID] = true
		if p.GridPos.X+p.GridPos.W > 24 {
			t.Errorf("panel %q is wider than the dashboard", p.Title)
		}
		if p.Type == "row" {
			continue
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no queries", p.Title)
		}
		for _, target := range p.Targets {
			for _, metric := range name.FindAllString(target.Expr, -1) {
				queried++
				base := metric
				for _, suffix := range []string{"_bucket", "_count", "_sum"} {
					base = strings.TrimSuffix(base, suffix)
				}
				if !exported[base] && !exported[metric] {
					t.Errorf("panel %q queries %s, which isn't exported", p.Title, metric)
				}
			}
		}
	}
	if queried == 0 {
		t.Fatal("no metrics were queried")
	}
}
