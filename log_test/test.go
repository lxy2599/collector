package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("Edge Log Tester Started...")
	count := 0
	for {
		// 打印带有时间戳的日志
		fmt.Printf("[%s] Log ID: %d | Status: Edge node is logging locally...\n",
			time.Now().Format("2006-01-02 15:04:05"), count)
		count++
		time.Sleep(10 * time.Second)
	}
}