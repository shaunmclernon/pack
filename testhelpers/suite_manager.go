package testhelpers

import (
	"testing"

	"github.com/sclevine/spec"
)

type SuiteManager struct {
	t             *testing.T
	suite         spec.Suite
	out           func(format string, args ...interface{})
	results       map[string]interface{}
	afterAllTasks map[string]func() error
}

func (s *SuiteManager) DoOnceString(key string, run func() (string, error)) (string, error) {
	v, err := s.DoOnce(key, func() (interface{}, error) {
		return run()
	})
	if err != nil {
		return "", err
	}

	return v.(string), nil
}

func (s *SuiteManager) DoOnce(key string, run func() (interface{}, error)) (interface{}, error) {
	if s.results == nil {
		s.results = map[string]interface{}{}
	}

	value, found := s.results[key]
	if !found {
		s.out("Running Once task '%s'\n", key)
		v, err := run()
		if err != nil {
			return nil, err
		}

		s.results[key] = v

		return v, nil
	}

	return value, nil
}

func (s *SuiteManager) DoOnceAfterAll(key string, cleanUp func() error) {
	if s.afterAllTasks == nil {
		s.afterAllTasks = map[string]func() error{}
	}

	s.afterAllTasks[key] = cleanUp
}

func (s *SuiteManager) Run() {
	defer s.runAfterAll()
	s.suite.Run(s.t)
}

func (s *SuiteManager) runAfterAll() error {
	for key, cleanUp := range s.afterAllTasks {
		s.out("Running AfterAll task '%s'\n", key)
		if err := cleanUp(); err != nil {
			return err
		}
	}

	return nil
}