#!/bin/bash

echo "Building plugin..."
go mod tidy
GOOS=wasip1 GOARCH=wasm go build -o plugin.wasm -buildmode=c-shared .

if [ $? -eq 0 ]; then
    echo "Plugin built successfully"
else
    echo "Build failed"
    exit 1
fi
