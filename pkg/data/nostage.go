//go:build no_stage
// +build no_stage

package data

func AssetNames() []string { return []string{} }

func Asset(_ string) ([]byte, error) { return nil, nil }
