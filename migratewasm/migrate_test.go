package main

import (
	"fmt"
	"testing"
)

func TestFoo(t *testing.T) {
	s := `cmd/compile`
	if pathRegex.MatchString(s) {
		fmt.Printf("%#v\n", pathRegex.FindStringSubmatch(s))
		s1 := pathRegex.ReplaceAllString(s, "${1}"+pathPrefix+"${2}${5}")
		fmt.Println(s1)
	}
}
