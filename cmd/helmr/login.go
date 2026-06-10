package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/cli/browser"
	"github.com/helmrdotdev/helmr/internal/cli/session"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

const defaultControlURL = "https://helmr.dev"

func loginCommand() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [URL]",
		Short: "Authenticate this CLI with Helmr.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawURL := explicitAPIURL(cmd)
			if len(args) > 0 {
				if rawURL != "" {
					return errors.New("pass the control URL either as an argument or --api-url, not both")
				}
				rawURL = args[0]
			} else {
				rawURL = cliControlURL(cmd)
			}
			baseURL, err := loginURL(rawURL)
			if err != nil {
				return err
			}
			control, err := client.New(baseURL)
			if err != nil {
				return err
			}
			start, err := control.StartDeviceCode(cmd.Context())
			if err != nil {
				return err
			}
			verificationURL := start.VerificationURIComplete
			if verificationURL == "" {
				verificationURL = start.VerificationURI
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Visit %s\n", verificationURL)
			fmt.Fprintf(cmd.OutOrStdout(), "Code: %s\n", start.UserCode)
			if !noBrowser && verificationURL != "" {
				if err := openURL(cmd.Context(), verificationURL); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Could not open browser: %v\n", err)
				}
			}
			token, err := waitForDeviceToken(cmd, control, start.DeviceCode, start.IntervalSeconds, start.ExpiresInSeconds)
			if err != nil {
				return err
			}
			state, err := newSessionStore()
			if err != nil {
				return err
			}
			if err := state.SaveLogin(baseURL, token); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s\n", baseURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the login URL without opening a browser.")
	return cmd
}

func logoutCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout [URL]",
		Short: "Log out this CLI from Helmr.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawURL := explicitAPIURL(cmd)
			if len(args) > 0 {
				if rawURL != "" {
					return errors.New("pass the control URL either as an argument or --api-url, not both")
				}
				rawURL = args[0]
			} else {
				rawURL = cliControlURL(cmd)
			}
			state, err := newSessionStore()
			if err != nil {
				return err
			}
			baseURL, err := savedLoginURL(state, rawURL)
			if err != nil {
				return err
			}
			token, err := state.Token(baseURL)
			if err != nil {
				if errors.Is(err, session.ErrNotFound) {
					return fmt.Errorf("not logged in to %s", baseURL)
				}
				return err
			}
			control, err := client.New(baseURL, client.WithBearerToken(token))
			if err != nil {
				return err
			}
			if err := control.Logout(cmd.Context()); err != nil {
				return err
			}
			if err := state.DeleteToken(baseURL); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged out from %s\n", baseURL)
			return nil
		},
	}
	return cmd
}

func loginURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		state, err := newSessionStore()
		if err == nil {
			cfg, err := state.Load()
			if err == nil {
				rawURL = cfg.DefaultHost
			} else if !errors.Is(err, session.ErrNotFound) {
				return "", err
			}
		}
	}
	if rawURL == "" {
		rawURL = defaultControlURL
	}
	parsed, err := parseControlURL(rawURL)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func savedLoginURL(state *session.Store, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		cfg, err := state.Load()
		if err != nil {
			if errors.Is(err, session.ErrNotFound) {
				return "", errors.New("no saved login")
			}
			return "", err
		}
		rawURL = cfg.DefaultHost
	}
	parsed, err := parseControlURL(rawURL)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func waitForDeviceToken(cmd *cobra.Command, control *client.Client, deviceCode string, intervalSeconds int64, expiresInSeconds int64) (string, error) {
	if intervalSeconds <= 0 {
		intervalSeconds = 5
	}
	if expiresInSeconds <= 0 {
		expiresInSeconds = 600
	}
	deadline := time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
	fmt.Fprintln(cmd.OutOrStdout(), "Waiting for authorization...")
	for {
		response, err := control.ExchangeDeviceCode(cmd.Context(), deviceCode)
		if err != nil {
			if kind := deviceTokenError(err); kind != "" {
				return "", fmt.Errorf("device authorization failed: %s", kind)
			}
			return "", err
		}
		switch response.Error {
		case "":
			if response.AccessToken == "" {
				return "", errors.New("device token response did not include an access token")
			}
			return response.AccessToken, nil
		case "authorization_pending":
			if time.Now().After(deadline) {
				return "", errors.New("device authorization expired")
			}
			timer := time.NewTimer(time.Duration(intervalSeconds) * time.Second)
			select {
			case <-cmd.Context().Done():
				timer.Stop()
				return "", cmd.Context().Err()
			case <-timer.C:
			}
		default:
			return "", fmt.Errorf("device authorization failed: %s", response.Error)
		}
	}
}

func deviceTokenError(err error) string {
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Message
	}
	return ""
}

var openURL = browser.Open
