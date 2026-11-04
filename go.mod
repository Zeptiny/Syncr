module github.com/Zeptiny/Syncr

go 1.27

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	connectrpc.com/connect v1.21.1
	go.yaml.in/yaml/v3 v3.0.6
	google.golang.org/protobuf v1.36.12
)
