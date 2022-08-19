package main

import (
	"log"

	"vitess.io/vitess/go/cmd/rulesctl/cmd"
	vtlog "vitess.io/vitess/go/vt/log"
)

func main() {
	rootCmd := cmd.Main()
	vtlog.RegisterFlags(rootCmd.PersistentFlags())
	if err := rootCmd.Execute(); err != nil {
		log.Printf("%v", err)
	}
}
