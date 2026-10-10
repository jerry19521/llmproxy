package config

import "github.com/google/uuid"

// NewUUID 生成 UUID 字符串。
func NewUUID() string { return uuid.NewString() }
