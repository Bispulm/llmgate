package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/routing"
)

const (
	defaultAdminURL      = "http://127.0.0.1:18302"
	adminURLEnv          = "LLMGATE_ADMIN_URL"
	defaultAdminTokenEnv = "LLMGATE_ADMIN_TOKEN" //nolint:gosec // The name of a variable, not a credential.
	adminRequestTimeout  = 30 * time.Second
	maxAdminResponse     = 4 << 20
)

// adminFlags are shared by every command that talks to the admin API. The token is only
// ever read from the environment, so it doesn't show up in a process listing.
type adminFlags struct {
	url      string
	tokenEnv string
	json     bool
}

type adminClient struct {
	base   string
	token  string
	client *http.Client
	out    io.Writer
	json   bool
}

func (f *adminFlags) client(p process) (*adminClient, error) {
	base := f.url
	if base == "" {
		base = p.getenv(adminURLEnv)
	}
	if base == "" {
		base = defaultAdminURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, errors.New("the admin URL is malformed")
	}
	token := p.getenv(f.tokenEnv)
	if token == "" {
		return nil, fmt.Errorf("set %s to the admin token", f.tokenEnv)
	}
	return &adminClient{base: strings.TrimRight(base, "/"), token: token, client: &http.Client{Timeout: adminRequestTimeout}, out: p.stdout, json: f.json}, nil
}

func (c *adminClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("calling the admin API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminResponse))
	if err != nil {
		return fmt.Errorf("reading the admin API's response: %w", err)
	}
	if resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &failure) == nil && failure.Error.Message != "" {
			return fmt.Errorf("%s (HTTP %d)", failure.Error.Message, resp.StatusCode)
		}
		return fmt.Errorf("the admin API answered HTTP %d", resp.StatusCode)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding the admin API's response: %w", err)
		}
	}
	return nil
}

// show prints v as JSON when asked to, and otherwise runs the human-readable printer.
func (c *adminClient) show(v any, human func(*tabwriter.Writer)) error {
	if c.json {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	human(w)
	return w.Flush()
}

// usd formats micro-USD exactly, with at least cents.
func usd(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	s := fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
	s = strings.TrimRight(s, "0")
	if dot := strings.IndexByte(s, '.'); len(s)-dot < 3 {
		s += strings.Repeat("0", 3-(len(s)-dot))
	}
	return sign + "$" + s
}

func addAdminCommands(root *cobra.Command, p process) {
	flags := &adminFlags{}
	groups := []*cobra.Command{teamsCommand(p, flags), keysCommand(p, flags), budgetsCommand(p, flags),
		usageCommand(p, flags), providersCommand(p, flags), reconcileCommand(p, flags), attemptsCommand(p, flags)}
	for _, g := range groups {
		g.PersistentFlags().StringVar(&flags.url, "admin-url", "", "admin API URL (default $"+adminURLEnv+", or "+defaultAdminURL+")")
		g.PersistentFlags().StringVar(&flags.tokenEnv, "admin-token-env", defaultAdminTokenEnv, "environment variable holding the admin token")
		g.PersistentFlags().BoolVar(&flags.json, "json", false, "print the admin API's JSON")
		root.AddCommand(g)
	}
}

// adminRun wraps a command body that needs an admin client.
func adminRun(p process, flags *adminFlags, body func(context.Context, *adminClient, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c, err := flags.client(p)
		if err != nil {
			return err
		}
		return body(cmd.Context(), c, args)
	}
}

func parseUSDFlag(name, value string) (int64, error) {
	micros, err := accounting.ParseUSD(value)
	if err != nil {
		return 0, fmt.Errorf("--%s: %w", name, err)
	}
	return micros, nil
}

func teamsCommand(p process, flags *adminFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "teams", Short: "Create and inspect teams"}

	list := &cobra.Command{Use: "list", Short: "List teams", Args: cobra.NoArgs,
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, _ []string) error {
			var teams []auth.Team
			if err := c.do(ctx, http.MethodGet, "/admin/v1/teams", nil, &teams); err != nil {
				return err
			}
			return c.show(teams, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintln(w, "TEAM\tBUDGET\tREQUESTS/MIN\tTOKENS/MIN\tIN FLIGHT")
				for _, t := range teams {
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, usd(t.BudgetLimitMicros),
						limit(t.Limits.RequestsPerMinute), limit(t.Limits.TokensPerMinute), limit(t.Limits.MaxInFlight))
				}
			})
		})}

	var budget string
	var limits auth.TeamLimits
	create := &cobra.Command{Use: "create TEAM", Short: "Create a team with a monthly budget", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			micros, err := parseUSDFlag("budget-usd", budget)
			if err != nil {
				return err
			}
			var team auth.Team
			if err := c.do(ctx, http.MethodPost, "/admin/v1/teams", auth.Team{ID: args[0], BudgetLimitMicros: micros, Limits: limits}, &team); err != nil {
				return err
			}
			return c.show(team, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintf(w, "created team %s with a monthly budget of %s\n", team.ID, usd(team.BudgetLimitMicros))
			})
		})}
	create.Flags().StringVar(&budget, "budget-usd", "", "monthly budget in USD, such as 25.00")
	_ = create.MarkFlagRequired("budget-usd")
	limitFlags(create, &limits)

	show := &cobra.Command{Use: "show TEAM", Short: "Show a team and its budget this month", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			return showTeam(ctx, c, args[0])
		})}

	var newLimits auth.TeamLimits
	setLimits := &cobra.Command{Use: "set-limits TEAM", Short: "Replace a team's rate and in-flight limits", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			if err := c.do(ctx, http.MethodPut, "/admin/v1/teams/"+url.PathEscape(args[0])+"/limits", newLimits, nil); err != nil {
				return err
			}
			_, err := fmt.Fprintf(c.out, "updated limits for %s; they apply as key caches expire\n", args[0])
			return err
		})}
	limitFlags(setLimits, &newLimits)

	cmd.AddCommand(list, create, show, setLimits)
	return cmd
}

func limitFlags(cmd *cobra.Command, l *auth.TeamLimits) {
	cmd.Flags().Int64Var(&l.RequestsPerMinute, "requests-per-minute", 0, "requests per minute, 0 for no limit")
	cmd.Flags().Int64Var(&l.TokensPerMinute, "tokens-per-minute", 0, "tokens per minute, 0 for no limit")
	cmd.Flags().Int64Var(&l.MaxInFlight, "max-in-flight", 0, "requests in flight at once, 0 for no limit")
}

func limit(n int64) string {
	if n == 0 {
		return "none"
	}
	return strconv.FormatInt(n, 10)
}

func showTeam(ctx context.Context, c *adminClient, team string) error {
	var detail struct {
		Team   auth.Team         `json:"team"`
		Budget accounting.Budget `json:"budget"`
	}
	if err := c.do(ctx, http.MethodGet, "/admin/v1/teams/"+url.PathEscape(team), nil, &detail); err != nil {
		return err
	}
	return c.show(detail, func(w *tabwriter.Writer) {
		b := detail.Budget
		_, _ = fmt.Fprintf(w, "team\t%s\n", detail.Team.ID)
		_, _ = fmt.Fprintf(w, "budget this month\t%s\n", usd(b.LimitMicros))
		_, _ = fmt.Fprintf(w, "settled\t%s\n", usd(b.SettledMicros))
		_, _ = fmt.Fprintf(w, "reserved\t%s\n", usd(b.ReservedMicros))
		_, _ = fmt.Fprintf(w, "available\t%s\n", usd(b.AvailableMicros))
	})
}

func keysCommand(p process, flags *adminFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Create, list, and revoke a team's API keys"}

	var name string
	create := &cobra.Command{Use: "create TEAM", Short: "Create a key, shown once", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			var key struct {
				ID     string `json:"id"`
				TeamID string `json:"team_id"`
				Key    string `json:"key"`
			}
			if err := c.do(ctx, http.MethodPost, "/admin/v1/teams/"+url.PathEscape(args[0])+"/keys", map[string]string{"name": name}, &key); err != nil {
				return err
			}
			return c.show(key, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintf(w, "%s\n", key.Key)
				_, _ = fmt.Fprintf(w, "key %s for team %s. This is the only time the key is shown.\n", key.ID, key.TeamID)
			})
		})}
	create.Flags().StringVar(&name, "name", "", "a label for the key")

	list := &cobra.Command{Use: "list TEAM", Short: "List a team's keys", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			var keys []auth.KeyInfo
			if err := c.do(ctx, http.MethodGet, "/admin/v1/teams/"+url.PathEscape(args[0])+"/keys", nil, &keys); err != nil {
				return err
			}
			return c.show(keys, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintln(w, "KEY\tNAME\tCREATED\tSTATUS")
				for _, k := range keys {
					status := "active"
					if k.RevokedAt != nil {
						status = "revoked " + k.RevokedAt.UTC().Format(time.RFC3339)
					}
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.ID, k.Name, k.CreatedAt.UTC().Format(time.RFC3339), status)
				}
			})
		})}

	revoke := &cobra.Command{Use: "revoke KEY_ID", Short: "Revoke a key", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			if err := c.do(ctx, http.MethodDelete, "/admin/v1/keys/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			_, err := fmt.Fprintf(c.out, "revoked %s; other replicas stop accepting it once their key cache expires\n", args[0])
			return err
		})}

	cmd.AddCommand(create, list, revoke)
	return cmd
}

func budgetsCommand(p process, flags *adminFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "budgets", Short: "Show and change team budgets"}
	show := &cobra.Command{Use: "show TEAM", Short: "Show a team's budget this month", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			return showTeam(ctx, c, args[0])
		})}
	var value string
	set := &cobra.Command{Use: "set TEAM", Short: "Set a team's monthly budget", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			micros, err := parseUSDFlag("limit-usd", value)
			if err != nil {
				return err
			}
			if err := c.do(ctx, http.MethodPut, "/admin/v1/teams/"+url.PathEscape(args[0])+"/budget", map[string]int64{"limit_micros": micros}, nil); err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.out, "set %s's monthly budget to %s\n", args[0], usd(micros))
			return err
		})}
	set.Flags().StringVar(&value, "limit-usd", "", "monthly budget in USD")
	_ = set.MarkFlagRequired("limit-usd")
	cmd.AddCommand(show, set)
	return cmd
}

func usageCommand(p process, flags *adminFlags) *cobra.Command {
	var recent int
	cmd := &cobra.Command{Use: "usage TEAM", Short: "Show a team's spending this month", Args: cobra.ExactArgs(1),
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, args []string) error {
			team := url.PathEscape(args[0])
			var usage accounting.Usage
			if err := c.do(ctx, http.MethodGet, "/admin/v1/teams/"+team+"/usage", nil, &usage); err != nil {
				return err
			}
			var attempts []accounting.AttemptRecord
			if recent > 0 {
				if err := c.do(ctx, http.MethodGet, "/admin/v1/teams/"+team+"/attempts?limit="+strconv.Itoa(recent), nil, &attempts); err != nil {
					return err
				}
			}
			view := struct {
				Usage    accounting.Usage           `json:"usage"`
				Attempts []accounting.AttemptRecord `json:"attempts,omitempty"`
			}{usage, attempts}
			return c.show(view, func(w *tabwriter.Writer) { printUsage(w, usage, attempts) })
		})}
	cmd.Flags().IntVar(&recent, "attempts", 0, "also list this many recent attempts")
	return cmd
}

func printUsage(w *tabwriter.Writer, u accounting.Usage, attempts []accounting.AttemptRecord) {
	b := u.Budget
	_, _ = fmt.Fprintf(w, "team %s, %s to %s\n", u.TeamID, u.PeriodStart.Format("2006-01-02"), u.PeriodEnd.Format("2006-01-02"))
	_, _ = fmt.Fprintf(w, "budget %s\tsettled %s\treserved %s\tavailable %s\n", usd(b.LimitMicros), usd(b.SettledMicros), usd(b.ReservedMicros), usd(b.AvailableMicros))
	states := []string{accounting.StateReconciled, accounting.StateReleased, accounting.StateUncertain, accounting.StateDispatched, accounting.StateReserved}
	parts := make([]string, 0, len(states))
	for _, s := range states {
		if n := u.Attempts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	_, _ = fmt.Fprintf(w, "%d requests\t%s\n\n", u.Requests, strings.Join(parts, ", "))
	_, _ = fmt.Fprintln(w, "PROVIDER\tMODEL\tATTEMPTS\tOPEN\tCHARGED\tPROMPT TOKENS\tCOMPLETION TOKENS")
	for _, r := range u.Routes {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%d\t%d\n", r.Provider, r.Model, r.Attempts, r.Open, usd(r.ChargeMicros), r.PromptTokens, r.CompletionTokens)
	}
	if len(attempts) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nATTEMPT\tREQUEST\t#\tROUTE\tSTATE\tOUTCOME\tRESERVED\tCHARGED")
	for _, a := range attempts {
		charged := "-"
		if a.ChargeMicros != nil {
			charged = usd(*a.ChargeMicros) + " " + a.ChargeKind
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s/%s\t%s\t%s\t%s\t%s\n", a.ID, a.RequestID, a.Number, a.Provider, a.Model, a.State, a.Outcome, usd(a.ReservedMicros), charged)
	}
}

func providersCommand(p process, flags *adminFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "providers", Short: "Inspect providers"}
	status := &cobra.Command{Use: "status", Short: "Show each route's circuit breaker, as one replica sees it", Args: cobra.NoArgs,
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, _ []string) error {
			var aliases []struct {
				Alias  string                `json:"alias"`
				Routes []routing.RouteStatus `json:"routes"`
			}
			if err := c.do(ctx, http.MethodGet, "/admin/v1/providers", nil, &aliases); err != nil {
				return err
			}
			return c.show(aliases, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintln(w, "ALIAS\tROUTE\tTIER\tWEIGHT\tBREAKER\tSUCCESSES\tFAILURES\tHELD FOR")
				for _, a := range aliases {
					for _, r := range a.Routes {
						_, _ = fmt.Fprintf(w, "%s\t%s/%s\t%d\t%d\t%s\t%d\t%d\t%s\n", a.Alias, r.Provider, r.Model, r.Tier, r.Weight,
							r.Breaker.State, r.Breaker.Successes, r.Breaker.Failures, r.HeldFor.Round(time.Millisecond))
					}
				}
			})
		})}
	cmd.AddCommand(status)
	return cmd
}

func reconcileCommand(p process, flags *adminFlags) *cobra.Command {
	return &cobra.Command{Use: "reconcile", Short: "Run one reconciliation pass now", Args: cobra.NoArgs,
		RunE: adminRun(p, flags, func(ctx context.Context, c *adminClient, _ []string) error {
			var result accounting.ReconcileResult
			if err := c.do(ctx, http.MethodPost, "/admin/v1/reconcile", nil, &result); err != nil {
				return err
			}
			return c.show(result, func(w *tabwriter.Writer) {
				_, _ = fmt.Fprintf(w, "released %d, marked uncertain %d, charged conservatively %d\n",
					result.Released, result.MarkedUncertain, result.ChargedConservatively)
			})
		})}
}

func attemptsCommand(p process, flags *adminFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "attempts", Short: "Correct individual attempts"}
	var body struct {
		PromptTokens           int64  `json:"prompt_tokens"`
		CompletionTokens       int64  `json:"completion_tokens"`
		CachedPromptTokens     *int64 `json:"cached_prompt_tokens,omitempty"`
		CacheWritePromptTokens *int64 `json:"cache_write_prompt_tokens,omitempty"`
	}
	var cached, written int64
	late := &cobra.Command{Use: "late-usage ATTEMPT_ID", Short: "Apply usage that arrived after an attempt ended", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.client(p)
			if err != nil {
				return err
			}
			// A breakdown that wasn't given is unknown, which isn't the same as zero.
			if cmd.Flags().Changed("cached-prompt-tokens") {
				body.CachedPromptTokens = &cached
			}
			if cmd.Flags().Changed("cache-write-prompt-tokens") {
				body.CacheWritePromptTokens = &written
			}
			var result struct {
				Applied bool `json:"applied"`
			}
			if err := c.do(cmd.Context(), http.MethodPost, "/admin/v1/attempts/"+url.PathEscape(args[0])+"/usage", body, &result); err != nil {
				return err
			}
			return c.show(result, func(w *tabwriter.Writer) {
				if result.Applied {
					_, _ = fmt.Fprintln(w, "applied: the charge now comes from this report")
				} else {
					_, _ = fmt.Fprintln(w, "not applied: the attempt's charge was already based on a report, or it was never sent")
				}
			})
		}}
	late.Flags().Int64Var(&body.PromptTokens, "prompt-tokens", 0, "prompt tokens, including cached ones")
	late.Flags().Int64Var(&body.CompletionTokens, "completion-tokens", 0, "completion tokens, including reasoning")
	late.Flags().Int64Var(&cached, "cached-prompt-tokens", 0, "prompt tokens read from a cache")
	late.Flags().Int64Var(&written, "cache-write-prompt-tokens", 0, "prompt tokens written to a cache")
	_ = late.MarkFlagRequired("prompt-tokens")
	_ = late.MarkFlagRequired("completion-tokens")
	cmd.AddCommand(late)
	return cmd
}
