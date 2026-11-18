// The Go code the ClipMux docs show, tested against the vector files. Its own
// module, so the server's go.mod and `go vet ./...` don't see its dependency.
module github.com/visheshgubrani/clipmux/site/snippets/go

go 1.25

require github.com/golang-jwt/jwt/v5 v5.3.1
