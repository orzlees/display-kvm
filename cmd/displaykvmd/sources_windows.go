package main

import (
	"github.com/orzlee/displayKvm/internal/daemon"
	windowsevents "github.com/orzlee/displayKvm/internal/events/windows"
)

func registerPlatformSources(service *daemon.Daemon, _ options) error {
	return service.RegisterEventSource(func() daemon.Source { return windowsevents.New() })
}
