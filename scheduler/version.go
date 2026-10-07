package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

var Version = "dev"

var SourceCommit = ""

func runVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the release version and the embedded source commit as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "version: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if !*asJSON {
		fmt.Println(Version)
		return 0
	}
	out, err := json.Marshal(struct {
		Version      string `json:"version"`
		SourceCommit string `json:"source_commit"`
	}{Version, SourceCommit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "version: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}
