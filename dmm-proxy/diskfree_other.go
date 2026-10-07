//go:build !linux

package main

import "errors"

// freeBytes 在非 linux 平台不可用（只用于本地 go vet），调用方会跳过余量检查。
func freeBytes(dir string) (int64, error) {
	return 0, errors.New("unsupported platform")
}
