package main

import (
	"context"
	"github.com/kok-stack/native-kubelet/cmd/virtual-kubelet/internal/provider"
	"github.com/kok-stack/native-kubelet/cmd/virtual-kubelet/internal/provider/mock"
	"github.com/kok-stack/native-kubelet/cmd/virtual-kubelet/internal/provider/native"
)

func registerMock(s *provider.Store) {
	s.Register("mock", func(cfg provider.InitConfig) (provider.Provider, error) { //nolint:errcheck
		return mock.NewMockProvider(
			cfg.ConfigPath,
			cfg.NodeName,
			cfg.OperatingSystem,
			cfg.InternalIP,
			cfg.DaemonPort,
		)
	})
}

func registerClusterProvider(ctx context.Context, s *provider.Store) {
	if err := s.Register("native-kubelet", func(cfg provider.InitConfig) (provider.Provider, error) {
		return native.NewProvider(ctx, cfg)
	}); err != nil {
		panic(err.Error())
	}
}
