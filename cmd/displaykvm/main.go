package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/orzlee/displayKvm/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], cli.Options{})
	// os.Exit不会执行defer，必须先释放信号订阅；取消不代表后台已发送的动作被撤回。
	stop()
	os.Exit(code)
}
