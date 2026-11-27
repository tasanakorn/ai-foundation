package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// catalogCommand reads and changes the database catalog (spec-0006 section 8.5). Like
// the admin commands, it needs the config file and PostgreSQL; NATS is used, when it
// is up, to tell running nodes about a change.
func catalogCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("aif catalog "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", envOr("AIF_CONFIG", "aif.yaml"), "path to config file")
	out := fs.String("out", "", "export: write to this file instead of stdout")
	file := fs.String("file", "", "import: catalog file (config file format)")
	ifRev := fs.Int64("if-revision", store.AnyRevision, "import: fail unless the catalog is still at this revision")
	prov := fs.String("provider", "", "set-key: provider name")
	acct := fs.String("account", catalog.DefaultAccount, "set-key: account name")
	limit := fs.Int("limit", 20, "history: number of revisions")
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
	case "export":
		c, err := st.GetCatalog(ctx)
		if err != nil {
			return err
		}
		b, err := catalog.Export(c)
		if err != nil {
			return err
		}
		if *out == "" {
			_, err = os.Stdout.Write(b)
			return err
		}
		if err := os.WriteFile(*out, b, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote revision %d (%s) to %s\n", c.Revision, catalog.Describe(c), *out)
		return nil

	case "import":
		if err := databaseMode(cfg); err != nil {
			return err
		}
		if *file == "" {
			return errors.New("-file is required")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		next, err := catalog.ParseImport(raw)
		if err != nil {
			return err
		}
		var kept []string
		c, err := catalog.Update(ctx, st, *ifRev, "import", "imported "+catalog.Describe(next)+" from "+filepath.Base(*file), func(c *store.Catalog) error {
			kept = catalog.KeepEncryptedKeys(c, next)
			c.Passthrough, c.Providers, c.Models = next.Passthrough, next.Providers, next.Models
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("imported revision %d: %s\n", c.Revision, catalog.Describe(c))
		if len(kept) > 0 {
			fmt.Printf("kept the encrypted keys of: %s\n", strings.Join(kept, ", "))
		}
		notify(ctx, cfg, c.Revision)
		return nil

	case "set-key":
		if err := databaseMode(cfg); err != nil {
			return err
		}
		if *prov == "" {
			return errors.New("-provider is required")
		}
		kr, err := catalog.ParseKeyring(cfg.Catalog.SecretKeys)
		if err != nil {
			return err
		}
		if !kr.Enabled() {
			return catalog.ErrNoSecretKey
		}
		key, err := readSecret("Provider key", false)
		if err != nil {
			return err
		}
		c, err := catalog.Update(ctx, st, store.AnyRevision, "cli", "set key of "+*prov+"/"+*acct, func(c *store.Catalog) error {
			for i := range c.Providers {
				if c.Providers[i].Name != *prov {
					continue
				}
				for j := range c.Providers[i].Accounts {
					if a := &c.Providers[i].Accounts[j]; a.Name == *acct {
						return kr.SetKey(a, key)
					}
				}
				return fmt.Errorf("provider %q has no account %q", *prov, *acct)
			}
			return fmt.Errorf("no provider %q", *prov)
		})
		if err != nil {
			return err
		}
		fmt.Printf("stored the encrypted key of %s/%s in revision %d\n", *prov, *acct, c.Revision)
		notify(ctx, cfg, c.Revision)
		return nil

	case "history":
		ch, err := st.ListCatalogChanges(ctx, *limit)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "REVISION\tWHEN\tBY\tSUMMARY")
		for _, c := range ch {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", c.Revision, c.CreatedAt.Format(time.RFC3339), c.Actor, c.Summary)
		}
		return tw.Flush()
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
	return nil
}

// databaseMode refuses writes when the file is the source of truth: the next file
// sync would undo them (spec-0006 section 3).
func databaseMode(cfg *config.Config) error {
	if cfg.Catalog.Mode == config.CatalogFile {
		return errors.New("catalog.mode is file: the catalog is managed by the file; change the file instead")
	}
	return nil
}

// notify tells running nodes about a new revision. Without NATS they reload at their
// next poll (catalog.poll_interval).
func notify(ctx context.Context, cfg *config.Config, rev int64) {
	nb, err := bus.Connect(ctx, cfg.NATS, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		err = nb.PublishCatalogChanged(ctx, rev)
		nb.Close(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "running nodes not notified (%v); they reload within %s\n", err, cfg.Catalog.PollInterval)
		return
	}
	fmt.Println("running nodes notified")
}
