package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--smoke" {
		fmt.Println("ok")
		return
	}
	fmt.Println("product")
}
