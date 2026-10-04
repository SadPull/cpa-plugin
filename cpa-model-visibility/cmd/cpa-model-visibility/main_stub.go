//go:build !cshared

// The !cshared build keeps go test / go vet working without cgo; the real
// entry point lives in main.go and is only compiled by
// `go build -buildmode=c-shared -tags cshared`.
package main

func main() {}
