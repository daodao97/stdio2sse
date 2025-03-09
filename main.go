package main

import (
	"flag"
	"fmt"
	"os"

	"github/daodao97/stdio2sse/server"
)

func main() {
	// 定义命令行参数
	cmdFlag := flag.String("cmd", "", "命令行启动命令")
	flag.Parse()

	// 检查命令是否提供
	if *cmdFlag == "" {
		fmt.Println("警告: 未提供启动命令，请使用 -cmd 参数指定")
		os.Exit(1)
	}

	// Create MCP server
	s := server.NewMCPServer(
		"get weather",
		"1.0.0",
	)

	_s := server.NewSSEServer(
		s,
		server.WithBaseURL("http://localhost:8080"),
		server.WithMessageEndpoint("/message"),
		server.WithSSEEndpoint("/sse"),
		server.WithStdioCmd(*cmdFlag),
	)

	fmt.Println("Starting server on port 8080")
	if err := _s.Start(":8080"); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}
