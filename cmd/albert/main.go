package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("albert %s\n", version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: albert version")
	os.Exit(2)
}
