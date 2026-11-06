package main

import (
	"github.com/orzlee/displayKvm/internal/daemon"
	linuxevents "github.com/orzlee/displayKvm/internal/events/linux"
)

func registerPlatformSources(service *daemon.Daemon, opts options) error {
	return service.RegisterEventSource(func() daemon.Source {
		return linuxevents.NewSource(linuxevents.Config{BusctlPath: opts.busctlPath, UdevadmPath: opts.udevadmPath})
	})
}
