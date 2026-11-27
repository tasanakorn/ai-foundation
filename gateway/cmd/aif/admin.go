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
	svc := auth.NewService(st, cfg.Admin.SessionTTL, slog.New(slog.NewTextHandler(io.Discard, nil)))

	switch sub {
	case "create-user":
		if *email == "" {
			return errors.New("-email is required")
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		u, err := svc.CreateUser(ctx, auth.CreateUserInput{Email: *email, Name: *name, Role: *role, Password: pw})
		if err != nil {
			return err
		}
		fmt.Printf("created %s %s (%s)\n", u.ID, u.Email, u.Role)
	case "reset-password":
		if *email == "" {
			return errors.New("-email is required")
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		u, err := svc.ResetPassword(ctx, *email, pw)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account with email %s", *email)
		}
		if err != nil {
			return err
		}
		fmt.Printf("password set for %s; all its sessions ended\n", u.Email)
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
