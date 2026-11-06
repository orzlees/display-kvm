package main

import (
	"bytes"

	"github.com/orzlee/displayKvm/internal/config"
)

type persistenceResult struct {
	saved *bool
	err   error
}

func known(value bool) *bool { return &value }

func encodeManagedConfig(cfg config.Config) ([]byte, error) {
	var data bytes.Buffer
	if err := config.Encode(&data, cfg); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}
