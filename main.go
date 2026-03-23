package main

import (
	"os"

	"github.com/itsHabib/orchestra/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
