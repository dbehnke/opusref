// Package server defines the future reflector service boundaries.
package server

import (
	"context"
	"errors"
)

// ErrNotImplemented marks bootstrap-only operations.
var ErrNotImplemented = errors.New("opusref server networking is not implemented")

// Service is the lifecycle contract for the future reflector.
type Service interface {
	Run(context.Context) error
	Close() error
}
