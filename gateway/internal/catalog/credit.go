package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
)

// ErrBadCreditEntry means a ledger entry was refused (task-0010).
var ErrBadCreditEntry = errors.New("credit entry refused")

// Limits of one ledger entry.
const (
	maxEntryUSD  = 1e7
	maxEntryNote = 200
)

// AddCreditEntry records a top-up or a correction in the ledger of a prepaid account.
// The account must exist in the database catalog with credit type prepaid. At may be
// in the past (money that arrived earlier) but not in the future; zero means now.
func AddCreditEntry(ctx context.Context, st store.Store, e *store.CreditEntry) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBadCreditEntry, fmt.Sprintf(format, a...))
	}
	c, err := st.GetCatalog(ctx)
	if err != nil {
		return err
	}
	acct, ok := findAccount(c, e.Provider, e.Account)
	if !ok {
		return fmt.Errorf("provider %q account %q: %w", e.Provider, e.Account, store.ErrNotFound)
	}
	now := time.Now().UTC()
	e.Note = strings.TrimSpace(e.Note)
	switch {
	case acct.Credit == nil || acct.Credit.Type != config.CreditPrepaid:
		return bad("provider %q account %q: only accounts with credit type prepaid have a ledger", e.Provider, e.Account)
	case e.USD == 0 || math.IsNaN(e.USD) || math.Abs(e.USD) > maxEntryUSD:
		return bad("usd must be a non-zero amount up to %g (negative corrects the balance)", float64(maxEntryUSD))
	case len(e.Note) > maxEntryNote:
		return bad("note is longer than %d characters", maxEntryNote)
	case e.At.After(now.Add(time.Minute)):
		return bad("at is in the future")
	}
	if e.At.IsZero() {
		e.At = now
	}
	e.ID, e.At, e.CreatedAt = id.New("cre_"), e.At.UTC(), now
	return st.AddCreditEntry(ctx, e)
}

func findAccount(c *store.Catalog, prov, account string) (store.CatalogAccount, bool) {
	for _, p := range c.Providers {
		if p.Name != prov {
			continue
		}
		for _, a := range p.Accounts {
			if a.Name == account {
				return a, true
			}
		}
	}
	return store.CatalogAccount{}, false
}

// FillCredits adds the spend to every account with a credit: the cost metered on all
// nodes this cycle (monthly, payg), or since the first ledger entry (prepaid).
func FillCredits(ctx context.Context, st store.Store, pools []provider.PoolState) error {
	for _, p := range pools {
		for _, a := range p.Accounts {
			c := a.Credit
			if c == nil {
				continue
			}
			if c.Type != config.CreditPrepaid {
				spent, err := spend(ctx, st, p.Name, a.Name, *c.CycleStart, *c.CycleEnd)
				if err != nil {
					return err
				}
				c.SetSpent(spent)
				continue
			}
			entries, err := st.ListCreditEntries(ctx, p.Name, a.Name)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				c.SetPrepaid(0, 0, nil)
				continue
			}
			topups, since := 0.0, entries[0].At
			for _, e := range entries {
				topups += e.USD
				if e.At.Before(since) {
					since = e.At
				}
			}
			spent, err := spend(ctx, st, p.Name, a.Name, since, time.Time{})
			if err != nil {
				return err
			}
			c.SetPrepaid(topups, spent, &since)
		}
	}
	return nil
}

func spend(ctx context.Context, st store.Store, prov, account string, from, to time.Time) (float64, error) {
	rows, err := st.SummarizeUsage(ctx, store.UsageFilter{Provider: prov, Account: account, From: from, To: to}, "none")
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, r := range rows {
		total += r.CostUSD
	}
	return total, nil
}
