//go:build !js || !wasm

package main

// The real entry point lives in main.go behind the js && wasm build tag. This
// stub keeps `go build ./...`, `go vet ./...` and `go test ./...` working on
// the host, where syscall/js does not exist.
func main() {}
