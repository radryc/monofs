package main

import (
	"fmt"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

func main() {
	var f syntax.WalkFn = func(e interface{}) {}
	fmt.Printf("%T\n", f)
}
