package main

import (
	"github.com/orzlee/displayKvm/internal/daemon"
	darwinevents "github.com/orzlee/displayKvm/internal/events/darwin"
)

func registerPlatformSources(service *daemon.Daemon, _ options) error {
	return service.RegisterEventSource(func() daemon.Source { return darwinevents.New() })
}
