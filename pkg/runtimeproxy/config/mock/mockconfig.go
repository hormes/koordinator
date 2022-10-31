/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by MockGen. DO NOT EDIT.
// Source: pkg/runtimeproxy/config/config_manager.go

// Package mock_config is a generated GoMock package.
package mock_config

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
	config "github.com/koordinator-sh/koordinator/pkg/runtimeproxy/config"
)

// MockManagerInterface is a mock of ManagerInterface interface.
type MockManagerInterface struct {
	ctrl     *gomock.Controller
	recorder *MockManagerInterfaceMockRecorder
}

// MockManagerInterfaceMockRecorder is the mock recorder for MockManagerInterface.
type MockManagerInterfaceMockRecorder struct {
	mock *MockManagerInterface
}

// NewMockManagerInterface creates a new mock instance.
func NewMockManagerInterface(ctrl *gomock.Controller) *MockManagerInterface {
	mock := &MockManagerInterface{ctrl: ctrl}
	mock.recorder = &MockManagerInterfaceMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockManagerInterface) EXPECT() *MockManagerInterfaceMockRecorder {
	return m.recorder
}

// GetAllHook mocks base method.
func (m *MockManagerInterface) GetAllHook() []*config.RuntimeHookConfig {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAllHook")
	ret0, _ := ret[0].([]*config.RuntimeHookConfig)
	return ret0
}

// GetAllHook indicates an expected call of GetAllHook.
func (mr *MockManagerInterfaceMockRecorder) GetAllHook() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAllHook", reflect.TypeOf((*MockManagerInterface)(nil).GetAllHook))
}

// Run mocks base method.
func (m *MockManagerInterface) Run() error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Run")
	ret0, _ := ret[0].(error)
	return ret0
}

// Run indicates an expected call of Run.
func (mr *MockManagerInterfaceMockRecorder) Run() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Run", reflect.TypeOf((*MockManagerInterface)(nil).Run))
}
