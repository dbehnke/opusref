// Package server implements reflector policy and UDP composition.
package server

import "context"

// Service is the reflector lifecycle contract.
type Service interface {
	Run(context.Context) error
	Close() error
}
