package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/spf13/cobra"
)

func TestCLIConfirmationRequiresExplicitCommandRetry(t *testing.T) {
	for _, mode := range []string{"interactive", "script", "json", "failed-confirmation", "concurrent-login"} {
		t.Run(mode, func(t *testing.T) {
			state := installTestCLIConfig(t)
			oldInteractive, oldOpen := interactiveCLI, openURL
			interactiveCLI = func(*cobra.Command) bool { return mode != "script" }
			t.Cleanup(func() { interactiveCLI = oldInteractive; openURL = oldOpen })
			mutations, starts, exchanges := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/auth/device/confirm/start":
					starts++
					if r.Header.Get("Authorization") != "Bearer old-cli" {
						t.Error("confirmation did not use original bearer")
					}
					if mode == "failed-confirmation" {
						w.WriteHeader(401)
						return
					}
					_ = json.NewEncoder(w).Encode(api.DeviceStartResponse{DeviceCode: "device", UserCode: "ABCD-EFGH", ExpiresInSeconds: 60})
				case "/api/auth/device/token":
					exchanges++
					_ = json.NewEncoder(w).Encode(api.DeviceTokenResponse{AccessToken: "new-cli"})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			if err := state.SaveLogin(server.URL, "old-cli"); err != nil {
				t.Fatal(err)
			}
			root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().String("api-url", server.URL, "")
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			action := &cobra.Command{Use: "member", RunE: permissionRun(func(cmd *cobra.Command, _ []string) error {
				if _, err := sessionControlPlaneClient(cmd); err != nil {
					return err
				}
				mutations++
				if mode == "concurrent-login" {
					if err := state.SaveLogin(server.URL, "other-cli"); err != nil {
						t.Fatal(err)
					}
				}
				return &httpclient.Error{Code: "recent_authentication_required", Message: "Confirm again"}
			})}
			action.Flags().Bool("json", false, "")
			root.AddCommand(action)
			args := []string{"member"}
			if mode == "json" {
				args = append(args, "--json")
			}
			err := executeCommand(root, args)
			if err == nil {
				t.Fatal("rejected mutation reported success")
			}
			if _, ok := errors.AsType[*httpclient.Error](err); !ok {
				t.Fatal("lost structured error")
			}
			if mutations != 1 {
				t.Fatalf("mutation replayed %d", mutations)
			}
			wantStarts, wantExchanges := 0, 0
			if mode == "interactive" || mode == "concurrent-login" {
				wantStarts = 1
				wantExchanges = 1
			}
			if mode == "failed-confirmation" {
				wantStarts = 1
			}
			if starts != wantStarts || exchanges != wantExchanges {
				t.Fatalf("starts=%d exchanges=%d", starts, exchanges)
			}
			token, tokenErr := state.Token(server.URL)
			if tokenErr != nil {
				t.Fatal(tokenErr)
			}
			wantToken := "old-cli"
			if mode == "interactive" || mode == "concurrent-login" {
				wantToken = "new-cli"
			}
			if token != wantToken {
				t.Fatalf("saved token %q", token)
			}
			if mode == "json" && !json.Valid([]byte(output.String()+err.Error())) {
				t.Fatalf("JSON contaminated: %s %v", output.String(), err)
			}
		})
	}
}

func TestCLIActivityOnlyAfterSuccessfulInteractiveHumanCommand(t *testing.T) {
	for _, mode := range []string{"human", "script", "failed", "machine"} {
		t.Run(mode, func(t *testing.T) {
			state := installTestCLIConfig(t)
			old := interactiveCLI
			interactiveCLI = func(*cobra.Command) bool { return mode != "script" }
			t.Cleanup(func() { interactiveCLI = old })
			activities := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/auth/device/activity" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer human-token" {
					t.Error("wrong activity bearer")
				}
				activities++
				w.WriteHeader(204)
			}))
			defer server.Close()
			if err := state.SaveLogin(server.URL, "human-token"); err != nil {
				t.Fatal(err)
			}
			t.Setenv(helmrAPIURLEnv, server.URL)
			if mode == "machine" {
				t.Setenv(helmrAPIKeyEnv, "machine-key")
			}
			root := &cobra.Command{Use: "test", RunE: func(cmd *cobra.Command, _ []string) error {
				if _, err := controlPlaneClient(cmd); err != nil {
					return err
				}
				if mode == "failed" {
					return errors.New("rejected")
				}
				return nil
			}}
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			err := executeCommand(root, nil)
			if mode != "failed" && err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "human" {
				want = 1
			}
			if activities != want {
				t.Fatalf("activities=%d want%d", activities, want)
			}
		})
	}
}

func TestLoginConfirmUsesSavedBearer(t *testing.T) {
	state := installTestCLIConfig(t)
	starts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/device/confirm/start":
			starts++
			if r.Header.Get("Authorization") != "Bearer old" {
				t.Error("missing original bearer")
			}
			_ = json.NewEncoder(w).Encode(api.DeviceStartResponse{DeviceCode: "device", ExpiresInSeconds: 60})
		case "/api/auth/device/token":
			_ = json.NewEncoder(w).Encode(api.DeviceTokenResponse{AccessToken: "new"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	if err := state.SaveLogin(server.URL, "old"); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := executeCommand(cmd, []string{"login", server.URL, "--confirm", "--no-browser"}); err != nil {
		t.Fatal(err)
	}
	token, err := state.Token(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || !strings.EqualFold(token, "new") {
		t.Fatalf("starts%d token%q", starts, token)
	}
}
