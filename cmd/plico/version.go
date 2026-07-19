package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is overridable at build time:
// go build -ldflags "-X main.version=v0.1.0"
var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the plico version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		v := version
		if v == "dev" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
		}
		fmt.Println("plico", v)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
