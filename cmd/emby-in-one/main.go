package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"emby-in-one/internal/backend"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--reset-password" {
		if len(args) != 2 {
			_, _ = io.WriteString(stderr, "usage: emby-in-one --reset-password <new-password>\n")
			return 1
		}
		if err := resetPassword(args[1], stdout); err != nil {
			_, _ = io.WriteString(stderr, err.Error()+"\n")
			return 1
		}
		return 0
	}

	app, err := backend.NewApp()
	if err != nil {
		_, _ = io.WriteString(stderr, err.Error()+"\n")
		return 1
	}
	defer app.Close()
	if err := app.Run(); err != nil {
		_, _ = io.WriteString(stderr, err.Error()+"\n")
		return 1
	}
	return 0
}

func resetPassword(newPassword string, stdout io.Writer) error {
	if strings.TrimSpace(newPassword) == "" {
		return fmt.Errorf("new password cannot be empty")
	}
	store, err := backend.LoadConfigStore()
	if err != nil {
		return err
	}
	hashed, err := backend.HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := store.Mutate(func(cfg *backend.Config) error {
		cfg.Admin.Password = hashed
		return nil
	}); err != nil {
		return err
	}
	if err := store.Save(); err != nil {
		return err
	}
	_, _ = io.WriteString(stdout, "Administrator password reset successfully.\n")
	return nil
}