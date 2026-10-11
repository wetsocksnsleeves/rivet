package watch

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dirWatcher watches directories with inotify and reports a directory's
// path whenever an entry in it is added, removed or renamed, or the
// directory itself is deleted or moved. Only directories are watched: an
// event names the directory, and the caller reconciles what is in it, as
// with the kqueue watcher on macOS.
type dirWatcher struct {
	fd     int
	events chan string
	errors chan error
	done   chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	byWd   map[int]string
	byPath map[string]int
}

const (
	dirMask  = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW
	goneMask = unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_IGNORED | unix.IN_UNMOUNT
)

func newDirWatcher() (*dirWatcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	w := &dirWatcher{
		fd:     fd,
		events: make(chan string, 256),
		errors: make(chan error, 16),
		done:   make(chan struct{}),
		byWd:   map[int]string{},
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
	wd, err := unix.InotifyAddWatch(w.fd, dir, dirMask)
	if err != nil {
		return &pathError{dir, err}
	}
	// inotify watches inodes, so a second path to the same directory gets
	// the same descriptor; the newer path wins.
	if old, ok := w.byWd[wd]; ok {
		delete(w.byPath, old)
	}
	w.byWd[wd] = dir
	w.byPath[dir] = wd
	return nil
}

// Remove stops watching dir.
func (w *dirWatcher) Remove(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wd, ok := w.byPath[dir]; ok {
		unix.InotifyRmWatch(w.fd, uint32(wd))
		delete(w.byWd, wd)
		delete(w.byPath, dir)
	}
}

// Close stops the watcher and releases every watch.
func (w *dirWatcher) Close() {
	close(w.done)
	w.wg.Wait()
	unix.Close(w.fd)
}

func (w *dirWatcher) read() {
	defer w.wg.Done()
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-w.done:
			return
		default:
		}
		// A timeout lets the loop notice Close without a wakeup pipe.
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR || n == 0 {
			continue
		}
		if err != nil {
			w.report(err)
			continue
		}
		n, err = unix.Read(w.fd, buf)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			w.report(err)
			continue
		}
		for _, dir := range w.parse(buf[:n]) {
			select {
			case w.events <- dir:
			case <-w.done:
				return
			}
		}
	}
}

// parse returns the directories the events in buf touched, in order.
func (w *dirWatcher) parse(buf []byte) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var dirs []string
	for off := 0; off+unix.SizeofInotifyEvent <= len(buf); {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		off += unix.SizeofInotifyEvent + int(ev.Len)
		if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
			// Events were dropped: every watched directory may have changed.
			for dir := range w.byPath {
				dirs = append(dirs, dir)
			}
			continue
		}
		dir, ok := w.byWd[int(ev.Wd)]
		if !ok {
			continue
		}
		if ev.Mask&goneMask != 0 {
			// The watch now follows a moved directory, or is gone with a
			// deleted one; stop using it. The next watch sync re-adds the
			// path if a directory is there again.
			if ev.Mask&unix.IN_IGNORED == 0 {
				unix.InotifyRmWatch(w.fd, uint32(ev.Wd))
			}
			delete(w.byWd, int(ev.Wd))
			delete(w.byPath, dir)
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

func (w *dirWatcher) report(err error) {
	select {
	case w.errors <- err:
	default:
	}
}
