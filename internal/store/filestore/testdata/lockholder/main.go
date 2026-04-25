// Build helper for filestore.TestReadActiveRunState_ReadsWhileSeparateProcessHoldsLock.
// Lives under testdata/ so go build / go test do not pick it up; the test
// builds it on demand by passing this file path to `go build`.
//
//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: lockholder <workspace> <signal-file> <hold-seconds>")
		os.Exit(2)
	}
	workspace := os.Args[1]
	signalPath := os.Args[2]
	hold, err := strconv.Atoi(os.Args[3])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse hold: %v\n", err)
		os.Exit(2)
	}

	st := filestore.New(workspace)
	release, err := st.AcquireRunLock(context.Background(), store.LockExclusive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acquire: %v\n", err)
		os.Exit(1)
	}
	defer release()

	if err := os.WriteFile(signalPath, []byte("locked"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "signal: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(time.Duration(hold) * time.Second)
}
