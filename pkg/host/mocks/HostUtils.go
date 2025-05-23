// Code generated by mockery v2.53.3. DO NOT EDIT.

package mocks

import (
	time "time"

	mock "github.com/stretchr/testify/mock"
)

// HostUtils is an autogenerated mock type for the HostUtils type
type HostUtils struct {
	mock.Mock
}

// DiscoverOfedVersion provides a mock function with no fields
func (_m *HostUtils) DiscoverOfedVersion() string {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for DiscoverOfedVersion")
	}

	var r0 string
	if rf, ok := ret.Get(0).(func() string); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(string)
	}

	return r0
}

// GetHostUptimeSeconds provides a mock function with no fields
func (_m *HostUtils) GetHostUptimeSeconds() (time.Duration, error) {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for GetHostUptimeSeconds")
	}

	var r0 time.Duration
	var r1 error
	if rf, ok := ret.Get(0).(func() (time.Duration, error)); ok {
		return rf()
	}
	if rf, ok := ret.Get(0).(func() time.Duration); ok {
		r0 = rf()
	} else {
		r0 = ret.Get(0).(time.Duration)
	}

	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ScheduleReboot provides a mock function with no fields
func (_m *HostUtils) ScheduleReboot() error {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for ScheduleReboot")
	}

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// NewHostUtils creates a new instance of HostUtils. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewHostUtils(t interface {
	mock.TestingT
	Cleanup(func())
}) *HostUtils {
	mock := &HostUtils{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
