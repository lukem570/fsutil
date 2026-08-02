package lock

import (
	"fmt"
	"io/fs"
	"time"
)

const (
	// defaultRetryInterval is the first pause between acquisition attempts. It
	// is short enough that handing a lock between processes feels immediate
	// and long enough that a contended lock does not spin.
	defaultRetryInterval = 5 * time.Millisecond

	// defaultMaxRetryInterval bounds the backoff. A lock held for minutes
	// should not be probed hundreds of times a second, but a waiter should
	// still notice its release promptly.
	defaultMaxRetryInterval = 250 * time.Millisecond

	// defaultFileMode is deliberately group-writable: lock files commonly
	// coordinate processes running as different members of one group, and a
	// mode that prevents the second process from opening the file turns a lock
	// into an outage.
	defaultFileMode fs.FileMode = 0o664

	defaultDirMode fs.FileMode = 0o755
)

type options struct {
	retryInterval    time.Duration
	maxRetryInterval time.Duration
	timeout          time.Duration
	fileMode         fs.FileMode
	dirMode          fs.FileMode
	createDirs       bool
	recordHolder     bool
}

func defaultOptions() options {
	return options{
		retryInterval:    defaultRetryInterval,
		maxRetryInterval: defaultMaxRetryInterval,
		fileMode:         defaultFileMode,
		dirMode:          defaultDirMode,
	}
}

func (o *options) validate() error {
	if o.retryInterval <= 0 {
		return fmt.Errorf("lock: retry interval must be positive, got %s", o.retryInterval)
	}
	if o.maxRetryInterval < o.retryInterval {
		return fmt.Errorf("lock: maximum retry interval %s is below the initial interval %s",
			o.maxRetryInterval, o.retryInterval)
	}
	if o.timeout < 0 {
		return fmt.Errorf("lock: timeout must not be negative, got %s", o.timeout)
	}
	return nil
}

// Option configures a [Lock].
type Option interface{ apply(*options) }

type optionFunc func(*options)

func (f optionFunc) apply(o *options) { f(o) }

// WithRetryInterval sets the initial pause between acquisition attempts.
//
// Acquisition backs off from this value up to the maximum set by
// [WithMaxRetryInterval], so a lock released quickly is noticed quickly while
// one held for a long time is not probed needlessly.
func WithRetryInterval(d time.Duration) Option {
	return optionFunc(func(o *options) { o.retryInterval = d })
}

// WithMaxRetryInterval caps the backoff between acquisition attempts. It also
// bounds how long after a lock is released a waiter may take to notice.
func WithMaxRetryInterval(d time.Duration) Option {
	return optionFunc(func(o *options) { o.maxRetryInterval = d })
}

// WithTimeout gives every blocking acquisition a deadline.
//
// It applies on top of the context passed to [Lock.Lock] and [Lock.RLock];
// whichever expires first wins. A timeout of zero, the default, means the
// context alone decides.
func WithTimeout(d time.Duration) Option {
	return optionFunc(func(o *options) { o.timeout = d })
}

// WithFileMode sets the permissions used when creating the lock file.
//
// The mode applies only at creation; an existing file keeps its own. The
// default is group-writable, because a lock file whose permissions exclude the
// other participants prevents them locking at all.
func WithFileMode(m fs.FileMode) Option {
	return optionFunc(func(o *options) { o.fileMode = fileModeOrDefault(m, defaultFileMode) })
}

// WithCreateDirs creates the lock file's parent directories if they are
// missing.
func WithCreateDirs() Option {
	return optionFunc(func(o *options) { o.createDirs = true })
}

// WithDirMode sets the permissions used by [WithCreateDirs].
func WithDirMode(m fs.FileMode) Option {
	return optionFunc(func(o *options) { o.dirMode = fileModeOrDefault(m, defaultDirMode) })
}

// WithHolderInfo records the holding process's identity in the lock file when
// an exclusive lock is taken, so that [Lock.Holder] can report it.
//
// It exists to make waiting diagnosable: "waiting for the lock" is a support
// ticket, while "waiting for pid 4123 on build-07" is an answer. It costs a
// write on each acquisition and is off by default.
//
// The lock file's contents are never used to decide whether a lock is held.
// That question is answered only by trying to take it.
func WithHolderInfo() Option {
	return optionFunc(func(o *options) { o.recordHolder = true })
}
