module github.com/maximhq/bifrost/deploy/cloudflare/container

go 1.27.0

require (
	github.com/maximhq/bifrost/core v1.9.1
	github.com/maximhq/bifrost/framework v1.7.1
)

replace github.com/maximhq/bifrost/core => ../../../core

replace github.com/maximhq/bifrost/framework => ../../../framework
