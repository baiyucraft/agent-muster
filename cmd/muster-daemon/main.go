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
		log.Fatal("muster-daemon 仅接受命令行选项")
	}
	if *version {
		fmt.Println("muster-daemon dev")
		return
	}
	flag.Usage()
}
