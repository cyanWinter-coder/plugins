// Copyright 2015-2017 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ns

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// switchMu serializes every setns(2) syscall made through this package.
//
// setns changes the network namespace of the calling OS thread, so two
// goroutines switching at the same time can observe each other's
// intermediate state even when their threads are locked. Holding this
// mutex around the whole "save current ns -> setns -> restore" critical
// section guarantees concurrent callers cannot interleave a switch, and
// also keeps a namespace handle from being closed (and its fd reused)
// while another goroutine is switching to it.
var switchMu sync.Mutex

// Returns an object representing the current OS thread's network namespace
func GetCurrentNS() (NetNS, error) {
	// Lock the thread in case other goroutine executes in it and changes its
	// network namespace after getCurrentThreadNetNSPath(), otherwise it might
	// return an unexpected network namespace.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return getCurrentNSNoLock()
}

func getCurrentNSNoLock() (NetNS, error) {
	return GetNS(getCurrentThreadNetNSPath())
}

func getCurrentThreadNetNSPath() string {
	// /proc/self/ns/net returns the namespace of the main thread, not
	// of whatever thread this goroutine is running on.  Make sure we
	// use the thread's net namespace since the thread is switching around
	return fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid())
}

type NetNS interface {
	// Executes the passed closure in this object's network namespace,
	// attempting to restore the original namespace before returning.
	// However, since each OS thread can have a different network namespace,
	// and Go's thread scheduling is highly variable, callers cannot
	// guarantee any specific namespace is set unless operations that
	// require that namespace are wrapped with Do().  Also, no code called
	// from Do() should call runtime.UnlockOSThread(), or the risk of
	// executing code in an incorrect namespace will be greater.  See
	// https://github.com/golang/go/wiki/LockOSThread for further details.
	//
	// The original namespace is unconditionally restored before Do
	// returns, including when toRun panics or returns an error. A panic
	// in toRun is recovered and returned as an error instead of crashing
	// the process; a failed restore is reported as an error as well, in
	// which case the locked OS thread is intentionally not unlocked so
	// the runtime scraps it rather than reuse it with a wrong namespace.
	Do(toRun func(NetNS) error) error

	// Sets the current network namespace to this object's network namespace.
	// Note this only affects the calling OS thread; the caller must hold
	// runtime.LockOSThread(). All switches in the process are serialized,
	// so callers cannot observe each other's intermediate namespaces.
	Set() error

	// Returns the filesystem path representing this object's network namespace
	Path() string

	// Returns a file descriptor representing this object's network namespace
	Fd() uintptr

	// Cleans up this instance of the network namespace; if this instance
	// is the last user the namespace will be destroyed.
	// Close is idempotent: closing an already closed handle is a no-op
	// returning nil, and a failed close can be retried.
	Close() error
}

type netNS struct {
	// mu guards file and closed. It must never be held while running
	// caller-supplied code.
	mu     sync.RWMutex
	file   *os.File
	closed bool
}

// netNS implements the NetNS interface
var _ NetNS = &netNS{}

const (
	// https://github.com/torvalds/linux/blob/master/include/uapi/linux/magic.h
	NSFS_MAGIC   = unix.NSFS_MAGIC
	PROCFS_MAGIC = unix.PROC_SUPER_MAGIC
)

type NSPathNotExistErr struct{ msg string }

func (e NSPathNotExistErr) Error() string { return e.msg }

type NSPathNotNSErr struct{ msg string }

func (e NSPathNotNSErr) Error() string { return e.msg }

func IsNSorErr(nspath string) error {
	stat := syscall.Statfs_t{}
	if err := syscall.Statfs(nspath, &stat); err != nil {
		if os.IsNotExist(err) {
			err = NSPathNotExistErr{msg: fmt.Sprintf("failed to Statfs %q: %v", nspath, err)}
		} else {
			err = fmt.Errorf("failed to Statfs %q: %v", nspath, err)
		}
		return err
	}

	switch stat.Type {
	case PROCFS_MAGIC, NSFS_MAGIC:
		return nil
	default:
		return NSPathNotNSErr{msg: fmt.Sprintf("unknown FS magic on %q: %x", nspath, stat.Type)}
	}
}

// isFdNS validates that an already-open file descriptor actually refers to
// a namespace file. Statfs on the path followed by Open is a TOCTOU window;
// fstatfs on the fd closes it by checking the exact object we hold.
func isFdNS(fd uintptr, name string) error {
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(fd), &stat); err != nil {
		return fmt.Errorf("failed to Fstatfs %q: %v", name, err)
	}

	switch stat.Type {
	case PROCFS_MAGIC, NSFS_MAGIC:
		return nil
	default:
		return NSPathNotNSErr{msg: fmt.Sprintf("unknown FS magic on %q: %x", name, stat.Type)}
	}
}

// Returns an object representing the namespace referred to by @path.
// The path is validated before anything is opened, and the resulting
// descriptor is validated again, so a missing or non-namespace target
// fails here without any change to the host's network state.
func GetNS(nspath string) (NetNS, error) {
	if err := IsNSorErr(nspath); err != nil {
		return nil, err
	}

	fd, err := os.Open(nspath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NSPathNotExistErr{msg: fmt.Sprintf("failed to open %q: %v", nspath, err)}
		}
		return nil, fmt.Errorf("failed to open %q: %v", nspath, err)
	}

	if err := isFdNS(fd.Fd(), nspath); err != nil {
		fd.Close()
		return nil, err
	}

	return &netNS{file: fd}, nil
}

// Returns a new empty NetNS.
// Calling Close() let the kernel garbage collect the network namespace.
func TempNetNS() (NetNS, error) {
	var tempNS NetNS
	var err error
	var wg sync.WaitGroup
	wg.Add(1)

	// Create the new namespace in a new goroutine so that if we later fail
	// to switch the namespace back to the original one, we can safely
	// leave the thread locked to die without a risk of the current thread
	// left lingering with incorrect namespace.
	go func() {
		defer wg.Done()
		runtime.LockOSThread()

		// save a handle to current network namespace
		var threadNS NetNS
		threadNS, err = getCurrentNSNoLock()
		if err != nil {
			err = fmt.Errorf("failed to open current namespace: %v", err)
			return
		}

		// Restore and release the original-namespace handle
		// unconditionally. Registering both before Unshare means a panic
		// during namespace creation still restores the thread instead of
		// leaving it stranded in the wrong namespace.
		defer func() {
			if closeErr := threadNS.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("failed to close original namespace handle: %w", closeErr)
			}
		}()
		defer func() {
			restoreErr := threadNS.Set()
			if restoreErr != nil {
				// Keep the thread locked so the runtime scraps it rather
				// than reusing a thread with an unknown namespace.
				if err != nil {
					err = errors.Join(err, fmt.Errorf("failed to restore original namespace: %w", restoreErr))
				} else {
					err = fmt.Errorf("failed to restore original namespace: %w", restoreErr)
				}
				return
			}
			// Unlock only after a successful restore.
			runtime.UnlockOSThread()
		}()

		// create the temporary network namespace
		if err = unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return
		}

		// get a handle to the temporary network namespace
		tempNS, err = getCurrentNSNoLock()
	}()

	wg.Wait()
	if err != nil {
		// Never hand back a half-built handle: the caller ignores it on
		// error and the descriptor would leak.
		if tempNS != nil {
			tempNS.Close()
			tempNS = nil
		}
	}
	return tempNS, err
}

func (ns *netNS) Path() string {
	return ns.file.Name()
}

func (ns *netNS) Fd() uintptr {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.file.Fd()
}

func (ns *netNS) errorIfClosed() error {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if ns.closed {
		return fmt.Errorf("%q has already been closed", ns.file.Name())
	}
	return nil
}

// Close releases the underlying namespace descriptor.
//
// It is safe to call concurrently with Set/Fd and safe to call more than
// once: repeat calls on a closed handle are a no-op returning nil. If the
// underlying close fails the handle is not marked closed, so the caller
// can retry rather than leaking the descriptor.
func (ns *netNS) Close() error {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	if ns.closed {
		return nil
	}

	if err := ns.file.Close(); err != nil {
		return fmt.Errorf("failed to close %q: %v", ns.file.Name(), err)
	}
	ns.closed = true

	return nil
}

// Set switches the calling (locked) OS thread to this namespace. The
// process-wide switch lock makes the switch atomic with respect to every
// other switch, and the per-handle lock prevents a concurrent Close from
// recycling the descriptor out from under setns(2).
func (ns *netNS) Set() error {
	if err := ns.errorIfClosed(); err != nil {
		return err
	}

	switchMu.Lock()
	defer switchMu.Unlock()

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if ns.closed {
		return fmt.Errorf("%q has already been closed", ns.file.Name())
	}

	if err := unix.Setns(int(ns.file.Fd()), unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("error switching to ns %v: %v", ns.file.Name(), err)
	}

	return nil
}

func (ns *netNS) Do(toRun func(NetNS) error) error {
	if err := ns.errorIfClosed(); err != nil {
		return err
	}

	// save a handle to current network namespace
	hostNS, err := GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to open current namespace: %v", err)
	}
	// The host handle is closed on the way out no matter how the
	// function returns. A close error is joined into the returned error
	// instead of being silently dropped.
	defer func() {
		if closeErr := hostNS.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("failed to close original namespace handle: %w", closeErr)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	// Start the callback in a new green thread so that if we later fail
	// to switch the namespace back to the original one, we can safely
	// leave the thread locked to die without a risk of the current thread
	// left lingering with incorrect namespace.
	var innerError error
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		innerError = ns.runLocked(hostNS, toRun)
	}()
	wg.Wait()

	if innerError != nil {
		if err != nil {
			// Preserve both the defer-time close error and the inner error.
			err = errors.Join(err, innerError)
		} else {
			err = innerError
		}
	}
	return err
}

// runLocked executes toRun in ns's namespace and restores the original
// one unconditionally. It runs on a locked OS thread and never unlocks it
// unless the restore has demonstrably succeeded.
func (ns *netNS) runLocked(hostNS NetNS, toRun func(NetNS) error) (retErr error) {
	threadNS, err := getCurrentNSNoLock()
	if err != nil {
		return fmt.Errorf("failed to open current netns: %v", err)
	}
	defer func() {
		// The descriptor is always released exactly once, including the
		// panic path.
		if closeErr := threadNS.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close thread namespace handle: %w", closeErr)
		}
	}()

	// switch to target namespace
	if err := ns.Set(); err != nil {
		return fmt.Errorf("error switching to ns %v: %v", ns.Path(), err)
	}

	// Restore runs in a defer so it cannot be skipped by a panicking
	// callback or an early return. Set() takes the process-wide switch
	// lock itself, so the restore is serialized against every other
	// switch.
	defer func() {
		restoreErr := threadNS.Set()

		if restoreErr != nil {
			// Keep the thread locked: the runtime will scrap it instead
			// of reusing a thread whose namespace is unknown.
			restoreErr = fmt.Errorf("failed to restore original network namespace: %w", restoreErr)
			if retErr != nil {
				retErr = errors.Join(retErr, restoreErr)
			} else {
				retErr = restoreErr
			}
			return
		}

		// Unlock only after a successful restore; see comment above.
		runtime.UnlockOSThread()
	}()

	// Recover panics so the deferred restore still executes and the long
	// lived daemon process is not torn down by one bad callback. The
	// panic is reported as an error together with its stack trace.
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic in netns callback: %v\n%s", r, debug.Stack())
		}
	}()

	return toRun(hostNS)
}

// WithNetNSPath executes the passed closure under the given network
// namespace, restoring the original namespace afterwards. A missing or
// invalid target path fails before any namespace switch, so the host's
// network state is never touched on a bad target.
func WithNetNSPath(nspath string, toRun func(NetNS) error) error {
	ns, err := GetNS(nspath)
	if err != nil {
		return err
	}
	defer ns.Close()
	return ns.Do(toRun)
}
