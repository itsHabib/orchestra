package main

import (
	"os"

	"github.com/michaelhabib/orchestra/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
