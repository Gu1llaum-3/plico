// plico — GitOps deployer for Docker Compose stacks.
package main

import (
	"fmt"
	"os"
	"strings"

	"plico/internal/gitrepo"
)

func main() {
	// Internal askpass mode (F4): when git invokes us via GIT_ASKPASS we
	// answer its username/password prompts from the environment that
	// gitrepo set on the git subprocess, then exit. Must run before cobra.
	if os.Getenv(gitrepo.AskpassEnvFlag) == "1" {
		prompt := strings.ToLower(strings.Join(os.Args[1:], " "))
		if strings.HasPrefix(prompt, "username") {
			fmt.Println(os.Getenv(gitrepo.EnvUsername))
		} else {
			fmt.Println(os.Getenv(gitrepo.EnvPassword))
		}
		return
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
