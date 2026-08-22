package daemon

import "testing"

// A pid is never liveness evidence, and the two values a damaged pidfile is
// likeliest to yield are the two that turn a shutdown request into an attack on
// the machine: on Unix, Kill(0, SIGTERM) signals ccdad's OWN process group and
// Kill(-1, SIGTERM) signals every process the user is allowed to signal.
// ReadPID already refuses to hand either of them out; this refuses to act on
// one whatever route it arrived by.
func TestRequestShutdownRefusesAPidThatIsNotOne(t *testing.T) {
	for _, pid := range []int{0, -1, -1000} {
		if err := RequestShutdown(pid); err == nil {
			t.Errorf("RequestShutdown(%d) = nil, want a refusal", pid)
		}
	}
}

// Reporting success for a process that is not there would make `ccdad daemon
// stop` announce a stop that never happened and then sit out its whole timeout
// waiting for a singleton nobody was ever going to release. Every platform
// either delivers the request or says why it could not; none of them returns
// nil for a pid above the system maximum.
func TestRequestShutdownDoesNotReportSuccessForAProcessThatCannotExist(t *testing.T) {
	if err := RequestShutdown(1 << 30); err == nil {
		t.Error("RequestShutdown(1<<30) = nil, want an error naming the pid that is not there")
	}
}
