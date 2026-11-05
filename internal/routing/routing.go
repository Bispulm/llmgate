// Package routing maps client-visible model aliases to upstream routes.
package routing

import "github.com/Bispulm/llmgate/internal/provider"

// Route is one way to serve an alias.
type Route struct {
	ProviderName string
	Provider     provider.Provider
	Model        string
}

// Alias is a client-visible model name.
type Alias struct {
	Name            string
	MaxOutputTokens int64
	Routes          []Route
}

// Table holds the configured aliases. It doesn't change after creation, so it is
// safe for concurrent use.
type Table struct {
	aliases map[string]*Alias
}

// NewTable returns a table of the given aliases.
func NewTable(aliases []Alias) *Table {
	t := &Table{aliases: make(map[string]*Alias, len(aliases))}
	for i := range aliases {
		t.aliases[aliases[i].Name] = &aliases[i]
	}
	return t
}

// Resolve looks up an alias by its exact name.
func (t *Table) Resolve(name string) (*Alias, bool) {
	a, ok := t.aliases[name]
	return a, ok
}
