package main

import "fmt"

func helloMessage() string {
	msg := "hello world"
	if msg == "" {
		panic("assertion failed: hello message must not be empty")
	}
	return msg
}

func main() {
	fmt.Println(helloMessage())
}
