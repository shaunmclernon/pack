package testhelpers

import "testing"

type ServiceProvider struct {
	t            *testing.T
	suiteManager *SuiteManager
	runCombo     RunCombo
}

func (s *ServiceProvider) RequestRegistry() (*TestRegistryConfig, error) {
	registryI, err := s.suiteManager.DoOnce("start-registry", func() (interface{}, error) {
		registry := RunRegistry(s.t)
		return registry, nil
	})
	if err != nil {
		return nil, err
	}

	registry := registryI.(*TestRegistryConfig)
	s.suiteManager.DoOnceAfterAll("stop-registry", func() error {
		registry.StopRegistry(s.t)
		return nil
	})
	
	return registry, nil
}
