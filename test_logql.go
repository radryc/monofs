//go:build ignore
// +build ignore

package main

import (
	"encoding/json"
	"fmt"

	"github.com/radryc/monofs/internal/storage/logquery"
)

func main() {
	q := `{service="payment"} |= "connection timeout"`
	expr, err := logquery.Parse(q)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%T\n", expr)
	b, _ := json.MarshalIndent(expr, "", "  ")
	fmt.Printf("%s\n", b)
}
