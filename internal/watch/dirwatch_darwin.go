package watch

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// dirWatcher watches directories with kqueue and reports a directory's path
// whenever an entry in it is added, removed or renamed, or the directory
// itself is deleted or moved. Only directories are watched: an event names
// the directory, and the caller reconciles what is in it. This catches what
// per-file watches miss, such as a symlink being deleted or a file renamed
// over an existing name, and holds one descriptor per directory rather than
// one per file.
type dirWatcher struct {
	kq     int
	events chan string
	errors chan error
	done   chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	byFd   map[int]string
	byPath map[string]int
}

const dirFlags = unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE

func newDirWatcher() (*dirWatcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	w := &dirWatcher{
		kq:     kq,
		events: make(chan string, 256),
		errors: make(chan error, 16),
		done:   make(chan struct{}),
		byFd:   map[int]string{},
		byPath: map[string]int{},
	}
	w.wg.Add(1)
	go w.read()
	return w, nil
}

// Add starts watching dir.
func (w *dirWatcher) Add(dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.byPath[dir]; ok {
		return nil
	}
	fd, err := unix.Open(dir, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return &pathError{dir, err}
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR|unix.EV_ENABLE)
	ev.Fflags = dirFlags
	if _, err := unix.Kevent(w.kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		unix.Close(fd)
		return &pathError{dir, err}
	}
	w.byFd[fd] = dir
	w.byPath[dir] = fd
	return nil
}

// Remove stops watching dir. Closing the descriptor removes it from kqueue.
func (w *dirWatcher) Remove(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if fd, ok := w.byPath[dir]; ok {
		unix.Close(fd)
		delete(w.byFd, fd)
		delete(w.byPath, dir)
	}
}

// Close stops the watcher and releases every descriptor.
func (w *dirWatcher) Close() {
	close(w.done)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	for fd := range w.byFd {
		unix.Close(fd)
	}
	unix.Close(w.kq)
}

func (w *dirWatcher) read() {
	defer w.wg.Done()
	buf := make([]unix.Kevent_t, 64)
	// A timeout lets the loop notice Close without a wakeup pipe.
	timeout := unix.NsecToTimespec(int64(100 * time.Millisecond))
	for {
		select {
		case <-w.done:
			return
		default:
		}
		n, err := unix.Kevent(w.kq, nil, buf, &timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			select {
			case w.errors <- err:
			default:
			}
			continue
		}
		for _, ev := range buf[:n] {
			w.mu.Lock()
			dir, ok := w.byFd[int(ev.Ident)]
			gone := ev.Fflags&(unix.NOTE_DELETE|unix.NOTE_RENAME|unix.NOTE_REVOKE) != 0
			if ok && gone {
				// The descriptor now follows a moved or deleted directory;
				// stop using it. The next watch sync re-adds the path if a
				// directory is there again.
				unix.Close(int(ev.Ident))
				delete(w.byFd, int(ev.Ident))
				delete(w.byPath, dir)
			}
			w.mu.Unlock()
			if !ok {
				continue
			}
			select {
			case w.events <- dir:
			case <-w.done:
				return
			}
		}
	}
}
