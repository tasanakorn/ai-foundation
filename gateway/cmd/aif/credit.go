package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// creditCommand records and lists the ledger of prepaid accounts (task-0010). The
// ledger lives only in the database, so it works in both catalog modes.
func creditCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("aif credit "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", envOr("AIF_CONFIG", "aif.yaml"), "path to config file")
	prov := fs.String("provider", "", "provider name")
	acct := fs.String("account", catalog.DefaultAccount, "account name")
	usd := fs.Float64("usd", 0, "add: amount in USD; negative corrects the balance")
	note := fs.String("note", "", "add: short note, for example an invoice number")
	at := fs.String("at", "", "add: when the money arrived (RFC 3339 or YYYY-MM-DD, UTC); default now")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		if *prov == "" {
			return errors.New("-provider is required")
		}
		e := &store.CreditEntry{Provider: *prov, Account: *acct, USD: *usd, Note: *note, Actor: "cli"}
		if *at != "" {
			if e.At, err = parseAt(*at); err != nil {
				return err
			}
		}
		if err := catalog.AddCreditEntry(ctx, st, e); err != nil {
			return err
		}
		fmt.Printf("added %s: %+.2f USD to %s/%s at %s\n", e.ID, e.USD, e.Provider, e.Account, e.At.Format(time.RFC3339))
		return nil

	case "list":
		if *prov == "" && flagSet(fs, "account") {
			return errors.New("-account needs -provider")
		}
		var entries []store.CreditEntry
		if *prov == "" {
			entries, err = st.ListCreditEntries(ctx, "", "")
		} else {
			entries, err = st.ListCreditEntries(ctx, *prov, *acct)
		}
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "AT\tPROVIDER/ACCOUNT\tUSD\tNOTE\tBY")
		total := map[string]float64{}
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s/%s\t%+.2f\t%s\t%s\n", e.At.Format(time.RFC3339), e.Provider, e.Account, e.USD, e.Note, e.Actor)
			total[e.Provider+"/"+e.Account] += e.USD
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for k, v := range total {
			fmt.Printf("total %s: %.2f USD\n", k, v)
		}
		return nil

	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// parseAt reads RFC 3339 or a plain date (midnight UTC).
func parseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("-at %q: use RFC 3339 (2026-11-27T10:00:00+07:00) or a date (2026-11-27)", s)
}
