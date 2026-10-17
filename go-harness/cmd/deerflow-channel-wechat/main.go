package main

import (
	"context"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/wechat"
)

func main() {
	if channelproto.Serve(context.Background(), os.Stdin, os.Stdout, wechat.NewPlugin()) != nil {
		os.Exit(1)
	}
}
