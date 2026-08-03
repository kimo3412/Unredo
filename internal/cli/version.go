package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/buildinfo"
)

type versionOutput struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionOutput{
				Version: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.BuildDate,
			}
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if format == "json" {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetEscapeHTML(false)
				return encoder.Encode(info)
			}
			if format != "table" {
				return fmt.Errorf("unsupported output format %q (want table or json)", format)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "version:    %s\ncommit:     %s\nbuild_date: %s\n", info.Version, info.Commit, info.BuildDate)
			return nil
		},
	}
}
