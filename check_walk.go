//go:build ignore
// +build ignore

package main

import (
	"fmt"

	"github.com/radryc/monofs/internal/storage/logquery"
)

func main() {
	parsed, err := logquery.Parse(`{service="payment"} |= "timeout"`)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%T\n", parsed.Matchers)
}
