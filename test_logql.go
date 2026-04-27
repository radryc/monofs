package main

import (
	"encoding/json"
	"fmt"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

func main() {
	q := `{service="payment"} |= "connection timeout"`
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%T\n", expr)
	b, _ := json.MarshalIndent(expr, "", "  ")
	fmt.Printf("%s\n", b)
}
