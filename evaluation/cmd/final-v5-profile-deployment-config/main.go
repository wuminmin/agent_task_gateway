// Command final-v5-profile-deployment-config resolves the closed, source-
// controlled Compose environment for one profile deployment.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	flags := flag.NewFlagSet("final-v5-profile-deployment-config", flag.ContinueOnError)
	registry := flags.String("registry", "", "source-controlled profile registry")
	overrides := flags.String("overrides", "", "source-controlled profile deployment overrides")
	retainedSourcePath := flags.String("retained-source-path", "", "campaign-relative retained override source")
	alias := flags.String("alias", "", "registered profile alias")
	out := flags.String("out", "", "create-exclusive deployment configuration record")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	config, err := finalv5profile.ResolveProfileDeploymentConfig(*registry, *overrides, *retainedSourcePath, *alias)
	if err == nil {
		var payload []byte
		payload, err = json.MarshalIndent(config, "", "  ")
		if err == nil {
			payload = append(payload, '\n')
			output, openErr := os.OpenFile(strings.TrimSpace(*out), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if openErr != nil {
				err = openErr
			} else if _, writeErr := output.Write(payload); writeErr != nil {
				err = writeErr
			} else {
				err = output.Close()
			}
		}
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "final-v5-profile-deployment-config:", err)
		}
		os.Exit(1)
	}
}
