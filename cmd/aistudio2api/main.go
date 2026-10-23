package main

import (
	"os"

	"github.com/Mag1cFall/AIStudio2API/internal/app"
	"github.com/Mag1cFall/AIStudio2API/internal/authhelper"
)

// main 执行 aistudio2api 命令入口
func main() {
	if len(os.Args) > 1 && os.Args[1] == "auth-helper" {
		os.Exit(authhelper.RunCLI(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(app.Run(os.Args[1:]))
}
