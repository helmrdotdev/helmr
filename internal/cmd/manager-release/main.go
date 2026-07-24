package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("manager release command must be verify")
	}
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "Manager catalog")
	bundlePath := flags.String("bundle", "", "Manager attestation bundle")
	trustedRootPath := flags.String("trusted-root", "", "Sigstore trusted root")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 ||
		*catalogPath == "" ||
		*bundlePath == "" ||
		*trustedRootPath == "" {
		return errors.New(
			"verify requires --catalog, --bundle, and --trusted-root",
		)
	}
	catalog, err := os.ReadFile(*catalogPath)
	if err != nil {
		return err
	}
	bundle, err := os.ReadFile(*bundlePath)
	if err != nil {
		return err
	}
	trustedRoot, err := os.ReadFile(*trustedRootPath)
	if err != nil {
		return err
	}
	verified, err := deployment.VerifyManagerCatalog(
		catalog,
		bundle,
		trustedRoot,
	)
	if err != nil {
		return err
	}
	digest, err := verified.Digest()
	if err != nil {
		return err
	}
	fmt.Println(digest)
	return nil
}
