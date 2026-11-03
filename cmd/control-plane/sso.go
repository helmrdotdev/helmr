package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runSSOCommand lets a self-hosted operator return an organization to optional
// SSO when its owners have lost both identity provider access and every
// recovery code. Owners then sign in normally and generate new codes.
func runSSOCommand(ctx context.Context, output io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "unrequire" {
		return errors.New("usage: control-plane sso unrequire --organization SLUG")
	}
	flags := flag.NewFlagSet("sso unrequire", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	slug := flags.String("organization", "", "Organization slug")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(*slug) == "" {
		return errors.New("usage: control-plane sso unrequire --organization SLUG")
	}
	policy, err := auditpolicy.Parse(os.Getenv("AUDIT_RETENTION_SECONDS"))
	if err != nil {
		return err
	}
	return withWorkerDatabase(ctx, func(pool *pgxpool.Pool) error {
		if err := identity.UnrequireSSO(ctx, pool, policy, audit.Internal(audit.ControlPlaneCLI), strings.TrimSpace(*slug)); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "Single sign-on is optional for %s. Owners can sign in normally and generate new recovery codes.\n", strings.TrimSpace(*slug))
		return err
	})
}
