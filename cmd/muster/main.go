package main

import (
	"flag"
	"fmt"
	"log"
)

func main() {
	version := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("muster 命令暂不可用: %s", flag.Arg(0))
	}
	if *version {
		fmt.Println("muster dev")
		return
	}
	flag.Usage()
}
