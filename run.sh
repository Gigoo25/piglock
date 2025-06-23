#!/bin/bash
# Set GO111MODULE variable
export GO111MODULE=on

# Explicitly get all dependencies, including those used by the functions package
go get github.com/beevik/ntp
go get github.com/stianeikeland/go-rpio/v4
go get github.com/d2r2/go-i2c
go get github.com/d2r2/go-logger

# Ensure we're building for the correct architecture
# For Raspberry Pi, this should typically be ARM
export GOARCH=$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/' | sed 's/armv7l/arm/')

# Tidy up the go.mod and go.sum files
go mod tidy

# Build and run the application
go build -o piclock
./piclock "$@"
