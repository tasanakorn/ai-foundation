package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// adminCommand manages admin accounts directly in the database. It needs only
// the config file and PostgreSQL (not NATS), so it works when the gateway is down.
func adminCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("aif admin "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", envOr("AIF_CONFIG", "aif.yaml"), "path to config file")
	email := fs.String("email", "", "account email")
	name := fs.String("name", "", "display name")
	role := fs.String("role", store.RoleAdmin, "admin or viewer")
	_ = fs.Parse(args)

	if sub == "hash-password" { // needs no config or database
		pw, err := readPassword()
		if err != nil {
			return err
		}
		if err := auth.CheckPassword(pw); err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		fmt.Println(hash)
		fmt.Fprintln(os.Stderr, "Set it as AIF_ADMIN_PASSWORD_HASH (admin.bootstrap.password_hash) and restart; quote it, it contains $.")
		return nil
	}
	if sub == "reset-password" {
		return errors.New("reset-password is gone: the local account's password is set in config (AIF_ADMIN_PASSWORD or AIF_ADMIN_PASSWORD_HASH); change it and restart. Other accounts sign in through OIDC")
	}
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
	svc := auth.NewService(st, auth.Options{SessionTTL: cfg.Admin.SessionTTL, LocalEmail: cfg.Admin.Bootstrap.Email,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	switch sub {
	case "create-user":
		if *email == "" {
			return errors.New("-email is required")
		}
		u, err := svc.CreateUser(ctx, auth.CreateUserInput{Email: *email, Name: *name, Role: *role})
		if err != nil {
			return err
		}
		fmt.Printf("created %s %s (%s). It has no password: allow a one-time link in the UI, then the user signs in through OIDC\n", u.ID, u.Email, u.Role)
	case "list-users":
		users, err := st.ListAdminUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tROLE\tSTATUS\tLAST LOGIN")
		for _, u := range users {
			status, last := "active", "-"
			if u.DisabledAt != nil {
				status = "disabled"
			}
			if svc.IsLocal(&u) {
				status += ", local"
			}
			if u.LastLoginAt != nil {
				last = u.LastLoginAt.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Email, u.Name, u.Role, status, last)
		}
		return tw.Flush()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	return nil
}

// readPassword asks twice on a terminal (no echo), or reads the first line of stdin.
func readPassword() (string, error) { return readSecret("Password", true) }

// readSecret asks on a terminal (no echo; twice with repeat), or reads the first line of stdin.
func readSecret(label string, repeat bool) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, label+": ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil || !repeat {
		return string(a), err
	}
	fmt.Fprint(os.Stderr, "Repeat "+strings.ToLower(label)+": ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("the two entries do not match")
	}
	return string(a), nil
}
