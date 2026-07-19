package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/config"
)

func init() {
	var configPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration without starting the daemon (F29)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			scheduled := 0
			for _, st := range cfg.Stacks {
				if st.Schedule != "" {
					scheduled++
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: OK — %d stack(s), %d scheduled\n",
				configPath, len(cfg.Stacks), scheduled)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "/etc/plico/config.toml", "path to config.toml")
	rootCmd.AddCommand(cmd)
}
