package main

import (
	"github.com/omengye/deerflow-acp/go-harness/internal/wslhelper"
	"os"
)

func main() { os.Exit(wslhelper.Main(os.Args[1:])) }
