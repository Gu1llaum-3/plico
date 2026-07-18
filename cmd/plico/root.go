package main

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "plico",
	Short:         "plico — pull-based GitOps deployer for Docker Compose stacks",
	Long:          "plico watches git repositories and deploys Docker Compose stacks,\nwith a blocking pre-deploy backup gate, SOPS decryption in memory,\nand failure-oriented notifications.",
	SilenceUsage:  true,
	SilenceErrors: false,
}
