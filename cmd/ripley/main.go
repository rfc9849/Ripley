package main

import (
	"fmt"
	"os"

	"ripley/internal/app"
)

func main() {
	if err := app.Main(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ripley: %v\n", err)
		os.Exit(1)
	}
}
