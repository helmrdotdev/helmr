package main

import (
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/spf13/cobra"
)

type whoamiResponse struct {
	api.MeResponse
	AuthSource string `json:"auth_source"`
	APIURL     string `json:"api_url,omitempty"`
}

func whoamiCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the active CLI authentication source.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := currentAuthSource(cmd)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", response.AuthSource, response.APIURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func currentAuthSource(cmd *cobra.Command) (whoamiResponse, error) {
	control, err := controlClient(cmd)
	if err != nil {
		return whoamiResponse{}, err
	}
	authSource := "api_key"
	if control.UsesSessionScopedRoutes() {
		authSource = "login"
		me, err := control.GetMe(cmd.Context())
		if err != nil {
			return whoamiResponse{}, err
		}
		return whoamiResponse{MeResponse: me, AuthSource: authSource, APIURL: control.BaseURL()}, nil
	}
	return whoamiResponse{AuthSource: authSource, APIURL: strings.TrimRight(control.BaseURL(), "/")}, nil
}
