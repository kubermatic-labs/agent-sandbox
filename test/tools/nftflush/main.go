// Command nftflush tries to flush the network namespace's nftables ruleset.
// Integration tests run it as the session user, where it must fail.
package main

import (
	"fmt"
	"os"

	"github.com/google/nftables"
)

func main() {
	c, err := nftables.New()
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(2)
	}
	c.FlushRuleset()
	if err := c.Flush(); err != nil {
		fmt.Println("flush:", err)
		os.Exit(1)
	}
	fmt.Println("flushed")
}
