// plico — GitOps deployer for Docker Compose stacks.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/gitrepo"
	"github.com/Gu1llaum-3/plico/internal/registryauth"
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

	// Credential-helper mode: when docker invokes us as docker-credential-plico
	// (routed by the DOCKER_CONFIG that registryauth.Setup wrote), answer its
	// registry credential requests from the config file, then exit with the
	// code docker expects. Also runs before cobra.
	if os.Getenv(registryauth.EnvFlag) == "1" {
		code := registryauth.Serve(os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
			func() (map[string]config.RegistryAuth, error) {
				return config.LoadRegistryAuths(os.Getenv(registryauth.EnvConfig))
			})
		os.Exit(code)
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
