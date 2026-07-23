module github.com/Muxcore-Media/jellyfin

go 1.26.4

require (
	github.com/Muxcore-Media/core v0.4.0
	github.com/Muxcore-Media/core/pkg/contracts v0.0.0
	github.com/Muxcore-Media/core/sdk/go/client v0.1.0
	github.com/Muxcore-Media/core/sdk/go/module v0.1.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260610212136-7ab31c22f7ad // indirect
)

replace github.com/Muxcore-Media/core => ../core

replace github.com/Muxcore-Media/core/sdk/go/module => ../core/sdk/go/module

replace github.com/Muxcore-Media/core/sdk/go/client => ../core/sdk/go/client

replace github.com/Muxcore-Media/core/pkg/contracts => ../core/pkg/contracts
