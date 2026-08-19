// Command final-v5-control-init applies the production Control PostgreSQL
// migrations to an already-created disposable database. It exists solely for
// the deployment-free Outcome-Merkle evaluation process; it creates no Task,
// ProfileBinding, Gateway, or publication evidence.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/internal/control"
)

func main() {
	dsnEnv := flag.String("dsn-env", "TASKGATE_FINAL_V5_CONTROL_DSN", "environment variable holding the disposable Control PostgreSQL DSN")
	flag.Parse()
	if flag.NArg() != 0 || *dsnEnv == "" {
		fmt.Fprintln(os.Stderr, "-dsn-env is required and positional arguments are forbidden")
		os.Exit(2)
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		fmt.Fprintf(os.Stderr, "%s is empty\n", *dsnEnv)
		os.Exit(2)
	}
	cipher, err := control.NewAES256GCM(make([]byte, 32))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store, err := control.Open(context.Background(), dsn, cipher, control.WithoutStartupRecovery())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := store.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
