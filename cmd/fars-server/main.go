// cmd/fars-server/main.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"fars/internal/app"
	"fars/internal/config"
)

func main() {
	cmd := newRootCommand()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var configPath string

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the image resize service",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadFromEnvOrFile(configPath)
			if err != nil {
				return err
			}
			application := app.Build(cfg)
			application.Run()
			return application.Err()
		},
	}
	serveCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration")

	rootCmd := &cobra.Command{
		Use:   "fars-server",
		Short: "Dynamic image resize service",
	}

	rootCmd.AddCommand(serveCmd)
	return rootCmd
}
