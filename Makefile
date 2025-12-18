# Create a build that run go build -o bin/acp-nvim

.PHONY: build
build:
	go build -o bin/acp-nvim -buildvcs=false ./go
