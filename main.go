// rumbo-claude is an external rumbo provider; stdout is JSON-RPC only.
package main

import (
	"context"
	"fmt"
	"os"

	"rumbo-claude/internal/claude"
	"rumbo-claude/protocol"
)

func main() {
	if err := protocol.Serve(context.Background(), os.Stdin, os.Stdout, claude.New()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
